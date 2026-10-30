package ip

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The leak fix depends on one Transport per family living as long as the
// Detector; a regression to per-call construction shows up here as distinct
// pointers.
func TestClient_ReusedPerFamily(t *testing.T) {
	d := &Detector{}
	v4a, v4b := d.client(IPv4), d.client(IPv4)
	v6a, v6b := d.client(IPv6), d.client(IPv6)
	if v4a != v4b || v6a != v6b {
		t.Fatal("client must return the same *http.Client for a family on every call")
	}
	if v4a == v6a {
		t.Fatal("families must not share a client: their dialers force different networks")
	}
	for _, c := range []*http.Client{v4a, v6a} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("transport type %T", c.Transport)
		}
		if tr.Proxy != nil {
			t.Fatal("detection must dial directly; a proxy would report the proxy's address")
		}
		if tr.IdleConnTimeout == 0 {
			t.Fatal("idle connections must expire")
		}
	}
}

func TestCloseIdleConnections_BeforeAndAfterUse(t *testing.T) {
	d := &Detector{}
	d.CloseIdleConnections() // no clients yet
	_ = d.client(IPv4)
	d.CloseIdleConnections()
	if d.client(IPv4) == nil {
		t.Fatal("client must survive CloseIdleConnections")
	}
}

// recorder logs, in order, which fake trace endpoints were asked.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

func (r *recorder) take() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := strings.Join(r.calls, " ")
	r.calls = nil
	return s
}

type fakeTrace struct {
	URL  string
	fail atomic.Bool // answer 503 instead of the trace body
}

// endpoint starts a trace server that answers body.
func (r *recorder) endpoint(t *testing.T, name, body string) *fakeTrace {
	t.Helper()
	e := &fakeTrace{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.add(name)
		if e.fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	e.URL = srv.URL + "/cdn-cgi/trace"
	return e
}

// hang starts a trace server that runs before (if set) and then never
// answers, like a blocked 1.1.1.1.
func (r *recorder) hang(t *testing.T, name string, before func()) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.add(name)
		if before != nil {
			before()
		}
		select {
		case <-req.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL + "/cdn-cgi/trace"
}

// reset returns a trace URL whose server drops every connection unanswered.
func reset(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "http://" + ln.Addr().String() + "/cdn-cgi/trace"
}

// plainClients sends both families over an ordinary loopback client, for
// IPv6 tests against IPv4 test servers.
func plainClients(t *testing.T) func(Family) *http.Client {
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	c := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	return func(Family) *http.Client { return c }
}

func TestTrace_PrimaryFailsFallbackAnswers(t *testing.T) {
	cases := []struct {
		name    string
		primary func(*testing.T, *recorder) string
		want    string
	}{
		{"timeout", func(t *testing.T, r *recorder) string { return r.hang(t, "primary", nil) }, "primary fallback"},
		{"connection reset", func(t *testing.T, _ *recorder) string { return reset(t) }, "fallback"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			fallback := rec.endpoint(t, "fallback", "fl=1\nip=203.0.113.7\n")
			// The real per-family client: forced tcp4 dial, per-attempt timeout.
			d := &Detector{Timeout: 300 * time.Millisecond}
			t.Cleanup(d.CloseIdleConnections)
			d.traceURLs[IPv4] = []string{c.primary(t, rec), fallback.URL}
			got, err := d.Detect(context.Background(), IPv4)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.Addr != "203.0.113.7" || got.Source != "trace" {
				t.Fatalf("got %+v", got)
			}
			if calls := rec.take(); calls != c.want {
				t.Fatalf("calls %q, want %q", calls, c.want)
			}
		})
	}
}

func TestTrace_RemembersLastWorkingEndpoint(t *testing.T) {
	rec := &recorder{}
	primary := rec.endpoint(t, "primary", "ip=203.0.113.1\n")
	fallback := rec.endpoint(t, "fallback", "ip=203.0.113.2\n")
	v6 := rec.endpoint(t, "v6primary", "ip=2001:db8::1\n")
	v6fallback := rec.endpoint(t, "v6fallback", "ip=2001:db8::2\n")
	d := &Detector{ClientFor: plainClients(t)}
	d.traceURLs[IPv4] = []string{primary.URL, fallback.URL}
	d.traceURLs[IPv6] = []string{v6.URL, v6fallback.URL}

	primary.fail.Store(true)
	if got, err := d.Detect(context.Background(), IPv4); err != nil || got.Addr != "203.0.113.2" {
		t.Fatalf("first: %+v %v", got, err)
	}
	if calls := rec.take(); calls != "primary fallback" {
		t.Fatalf("first calls %q", calls)
	}

	// The primary works again, but the remembered fallback is asked first.
	primary.fail.Store(false)
	if got, err := d.Detect(context.Background(), IPv4); err != nil || got.Addr != "203.0.113.2" {
		t.Fatalf("second: %+v %v", got, err)
	}
	if calls := rec.take(); calls != "fallback" {
		t.Fatalf("second calls %q, want only the remembered fallback", calls)
	}

	// Each family remembers its own endpoint.
	if got, err := d.Detect(context.Background(), IPv6); err != nil || got.Addr != "2001:db8::1" {
		t.Fatalf("v6: %+v %v", got, err)
	}
	if calls := rec.take(); calls != "v6primary" {
		t.Fatalf("v6 calls %q", calls)
	}
}

func TestTrace_RememberedEndpointFailsTriesOthersInOrder(t *testing.T) {
	rec := &recorder{}
	a := rec.endpoint(t, "a", "ip=203.0.113.1\n")
	b := rec.endpoint(t, "b", "ip=203.0.113.2\n")
	c := rec.endpoint(t, "c", "ip=203.0.113.3\n")
	d := &Detector{ClientFor: plainClients(t)}
	d.traceURLs[IPv4] = []string{a.URL, b.URL, c.URL}

	a.fail.Store(true)
	b.fail.Store(true)
	if got, err := d.Detect(context.Background(), IPv4); err != nil || got.Addr != "203.0.113.3" {
		t.Fatalf("first: %+v %v", got, err)
	}
	if calls := rec.take(); calls != "a b c" {
		t.Fatalf("first calls %q", calls)
	}

	c.fail.Store(true)
	b.fail.Store(false)
	if got, err := d.Detect(context.Background(), IPv4); err != nil || got.Addr != "203.0.113.2" {
		t.Fatalf("second: %+v %v", got, err)
	}
	if calls := rec.take(); calls != "c a b" {
		t.Fatalf("second calls %q, want remembered c then the list order", calls)
	}

	if _, err := d.Detect(context.Background(), IPv4); err != nil {
		t.Fatalf("third: %v", err)
	}
	if calls := rec.take(); calls != "b" {
		t.Fatalf("third calls %q", calls)
	}
}

func TestTrace_AllFailNamesEveryEndpoint(t *testing.T) {
	rec := &recorder{}
	primary := rec.endpoint(t, "primary", "ip=203.0.113.1\n")
	primary.fail.Store(true)
	fallback := rec.endpoint(t, "fallback", "ip=2001:db8::1\n")
	d := &Detector{ClientFor: plainClients(t)}
	d.traceURLs[IPv4] = []string{primary.URL, fallback.URL}

	_, err := d.Detect(context.Background(), IPv4)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "trace ipv4: ") {
		t.Fatalf("error %q must keep the trace prefix", msg)
	}
	for _, want := range []string{primary.URL + ": status 503", fallback.URL + ": ", "2001:db8::1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q lacks %q", msg, want)
		}
	}
}

func TestTrace_RejectsOtherFamilyAnswer(t *testing.T) {
	cases := []struct {
		f           Family
		wrong, want string
	}{
		{IPv4, "2001:db8::1", "203.0.113.5"},
		{IPv4, "::ffff:203.0.113.6", "203.0.113.5"},
		{IPv6, "203.0.113.6", "2001:db8::5"},
		{IPv6, "::ffff:203.0.113.6", "2001:db8::5"},
	}
	for _, c := range cases {
		t.Run(c.f.String()+" "+c.wrong, func(t *testing.T) {
			rec := &recorder{}
			primary := rec.endpoint(t, "primary", "ip="+c.wrong+"\n")
			fallback := rec.endpoint(t, "fallback", "ip="+c.want+"\n")
			d := &Detector{ClientFor: plainClients(t)}
			d.traceURLs[c.f] = []string{primary.URL, fallback.URL}
			got, err := d.Detect(context.Background(), c.f)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got.Addr != c.want {
				t.Fatalf("got %+v, want %s", got, c.want)
			}
			if calls := rec.take(); calls != "primary fallback" {
				t.Fatalf("calls %q", calls)
			}
		})
	}
}

func TestTrace_StopsWhenContextDone(t *testing.T) {
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fallback := rec.endpoint(t, "fallback", "ip=203.0.113.9\n")
	d := &Detector{}
	t.Cleanup(d.CloseIdleConnections)
	d.traceURLs[IPv4] = []string{rec.hang(t, "primary", cancel), fallback.URL}

	// The transport refuses a done context before dialing, so the error must
	// also show that the fallback was never attempted.
	_, err := d.Detect(ctx, IPv4)
	if err == nil || !strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), fallback.URL) {
		t.Fatalf("err = %v", err)
	}
	if calls := rec.take(); calls != "primary" {
		t.Fatalf("calls %q, want no endpoint after the cancel", calls)
	}

	_, err = d.Detect(ctx, IPv4)
	if err == nil || err.Error() != "trace ipv4: context canceled" {
		t.Fatalf("already cancelled: err = %v", err)
	}
	if calls := rec.take(); calls != "" {
		t.Fatalf("already cancelled: calls %q", calls)
	}
}

// Run with -race: the remembered endpoint is shared by concurrent callers.
func TestTrace_ConcurrentDetect(t *testing.T) {
	rec := &recorder{}
	primary := rec.endpoint(t, "primary", "ip=203.0.113.1\n")
	primary.fail.Store(true)
	fallback := rec.endpoint(t, "fallback", "ip=203.0.113.2\n")
	d := &Detector{ClientFor: plainClients(t)}
	d.traceURLs[IPv4] = []string{primary.URL, fallback.URL}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := d.Detect(context.Background(), IPv4); err != nil || got.Addr != "203.0.113.2" {
				t.Errorf("%+v %v", got, err)
			}
		}()
	}
	wg.Wait()
}

func TestTrace_DefaultEndpoints(t *testing.T) {
	d := &Detector{}
	want := map[Family]string{
		IPv4: "https://1.1.1.1/cdn-cgi/trace https://www.cloudflare.com/cdn-cgi/trace",
		IPv6: "https://[2606:4700:4700::1111]/cdn-cgi/trace https://www.cloudflare.com/cdn-cgi/trace",
	}
	for f, w := range want {
		if got := strings.Join(d.traceEndpoints(f), " "); got != w {
			t.Errorf("%v: %q, want %q", f, got, w)
		}
	}
}
