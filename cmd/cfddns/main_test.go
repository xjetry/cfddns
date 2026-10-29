package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/httpserver"
	"cfddns/internal/ip"
	"cfddns/internal/updater"
)

// stubIP answers instantly: a failing family makes a cycle take microseconds,
// which is the widest window for the ticker and HTTP requests to overlap.
type stubIP struct {
	addr  string
	err   error
	calls atomic.Int32
}

func (s *stubIP) Detect(context.Context, ip.Family) (ip.Detection, error) {
	s.calls.Add(1)
	if s.err != nil {
		return ip.Detection{}, s.err
	}
	return ip.Detection{Addr: s.addr, Source: "trace"}, nil
}

// stubDNS holds one record whose content never changes behind the updater's
// back, so every cycle after the first is served from the cache.
type stubDNS struct{ content string }

func (stubDNS) ZoneIDFor(context.Context, string) (string, error) { return "zone", nil }
func (s stubDNS) FindRecords(_ context.Context, _, typ, name string) ([]cloudflare.Record, error) {
	return []cloudflare.Record{{ID: "rec", Type: typ, Name: name, Content: s.content}}, nil
}
func (stubDNS) CreateRecord(_ context.Context, _, typ, name, content string) (cloudflare.Record, error) {
	return cloudflare.Record{ID: "new", Type: typ, Name: name, Content: content}, nil
}
func (stubDNS) UpdateRecordContent(context.Context, string, string, string) error { return nil }
func (stubDNS) DeleteRecord(context.Context, string, string) error                { return nil }

// logSink collects log lines and lets a test wait for one matching a pattern.
type logSink struct {
	mu    sync.Mutex
	lines []string
	ch    chan string
}

func newLogSink() *logSink { return &logSink{ch: make(chan string, 256)} }

func (l *logSink) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	l.ch <- line
}

func (l *logSink) wait(t *testing.T, re *regexp.Regexp) []string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-l.ch:
			if m := re.FindStringSubmatch(line); m != nil {
				return m
			}
		case <-deadline:
			t.Fatalf("no log line matching %s; got %q", re, l.snapshot())
		}
	}
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func wire(det updater.IPDetector, dns updater.DNS, logf func(string, ...any)) *updater.Updater {
	rep := newReporter(logf)
	return &updater.Updater{DomainV4: "h.example.com", DomainV6: "h.example.com", IP: det, DNS: dns, Observe: rep.report}
}

func TestOnce_ExitCodeFollowsAllOK(t *testing.T) {
	sink := newLogSink()
	if code := once(wire(&stubIP{addr: "198.51.100.1"}, stubDNS{content: "198.51.100.1"}, sink.logf)); code != 0 {
		t.Fatalf("all families ok: exit %d, want 0", code)
	}
	if code := once(wire(&stubIP{err: errors.New("no route")}, stubDNS{}, sink.logf)); code != 1 {
		t.Fatalf("a failing family: exit %d, want 1", code)
	}
	if lines := sink.snapshot(); len(lines) != 4 || !strings.Contains(lines[2], "error: no route") {
		t.Fatalf("every -once result is logged: %q", lines)
	}
}

// The HTTP handler runs cycles on request goroutines while the ticker runs
// its own; both end in the reporter. This is the wiring main uses, under
// -race with instantly failing cycles and a burst of requests.
func TestDaemon_HTTPAndTickerShareTheReporterSafely(t *testing.T) {
	sink := newLogSink()
	det := &stubIP{err: errors.New("no route")}
	up := wire(det, stubDNS{}, sink.logf)
	d := &daemon{period: 2 * time.Millisecond, cycle: up.Run, logf: sink.logf}
	d.http = (&httpserver.Server{Passwd: "s", Run: up.Run}).HTTPServer("127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.run(ctx)
		close(done)
	}()
	addr := sink.wait(t, regexp.MustCompile(`^http listening on (\S+)$`))[1]

	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/update", nil)
			req.Header.Set("Authorization", "Bearer s")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				bad.Add(1)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d of 50 requests failed", n)
	}
	// The ticker kept cycling beside the requests: 50 requests × 2 families
	// plus the first cycle is 102 detections; more means ticker cycles ran.
	deadline := time.Now().Add(2 * time.Second)
	for det.calls.Load() <= 102 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if det.calls.Load() <= 102 {
		t.Fatalf("ticker did not run beside HTTP: %d detections", det.calls.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after ctx was cancelled")
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("HTTP port still open after shutdown")
	}
	sink.wait(t, regexp.MustCompile(`^shutdown$`))
}

func TestDaemon_FirstCycleThenTicker(t *testing.T) {
	sink := newLogSink()
	det := &stubIP{addr: "198.51.100.1"}
	up := wire(det, stubDNS{content: "198.51.100.1"}, sink.logf)
	d := &daemon{period: 5 * time.Millisecond, cycle: up.Run, logf: sink.logf}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.run(ctx)
		close(done)
	}()
	// The first cycle runs before the ticker's first tick and is logged.
	sink.wait(t, regexp.MustCompile(`^ipv4 h.example.com unchanged ip=198.51.100.1$`))
	deadline := time.Now().Add(2 * time.Second)
	for det.calls.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if n := det.calls.Load(); n < 6 {
		t.Fatalf("ticker cycles missing: %d detections", n)
	}
	// Steady state: nothing after the first cycle's two lines but shutdown.
	for _, line := range sink.snapshot() {
		if !strings.HasPrefix(line, "ipv") && line != "shutdown" {
			t.Fatalf("unexpected log line %q", line)
		}
	}
	if got := sink.snapshot(); len(got) != 3 {
		t.Fatalf("want 2 first-cycle lines + shutdown, got %q", got)
	}
}

func TestDaemon_BindFailureIsLoggedAndTickerContinues(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { taken.Close() })

	sink := newLogSink()
	det := &stubIP{addr: "198.51.100.1"}
	up := wire(det, stubDNS{content: "198.51.100.1"}, sink.logf)
	d := &daemon{period: 5 * time.Millisecond, cycle: up.Run, logf: sink.logf}
	d.http = (&httpserver.Server{Passwd: "s", Run: up.Run}).HTTPServer(taken.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.run(ctx)
		close(done)
	}()
	sink.wait(t, regexp.MustCompile(`^http listen 127\.0\.0\.1:\d+: .*address already in use`))
	deadline := time.Now().Add(2 * time.Second)
	for det.calls.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if n := det.calls.Load(); n < 6 {
		t.Fatalf("ticker must survive a bind failure: %d detections", n)
	}
	for _, line := range sink.snapshot() {
		if strings.HasPrefix(line, "http listening on") {
			t.Fatalf("listening must only be announced for a bound port: %q", line)
		}
	}
}
