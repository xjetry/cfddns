package status

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"cfddns/internal/updater"
)

type decodedSnapshot struct {
	Version   int             `json:"version"`
	StartedAt time.Time       `json:"started_at"`
	WrittenAt time.Time       `json:"written_at"`
	Period    string          `json:"period"`
	Families  []decodedFamily `json:"families"`
}

type decodedFamily struct {
	Family     string            `json:"family"`
	Domain     string            `json:"domain"`
	RecordType string            `json:"record_type"`
	Detection  *decodedDetection `json:"detection"`
	Execution  *decodedExecution `json:"execution"`
}

type decodedDetection struct {
	At     time.Time `json:"at"`
	IP     string    `json:"ip"`
	Source string    `json:"source"`
	Error  string    `json:"error"`
}

type decodedExecution struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Error   string    `json:"error"`
	Warning string    `json:"warning"`
}

func readSnapshot(t *testing.T, path string) (decodedSnapshot, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snapshot decodedSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("decode snapshot %s: %v", data, err)
	}
	return snapshot, data
}

func TestReporterReportWritesSnapshot(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	started := time.Date(2026, 10, 27, 9, 0, 0, 123456789, zone)
	written := time.Date(2026, 10, 27, 9, 5, 0, 987654321, zone)
	currentTime := started
	path := filepath.Join(t.TempDir(), "cfddns.status")
	reporter := New(Config{
		Path:     path,
		Period:   5 * time.Minute,
		DomainV4: "v4.example.com",
		DomainV6: "v6.example.com",
		Now:      func() time.Time { return currentTime },
	})
	currentTime = written
	detected := time.Date(2026, 10, 27, 9, 4, 58, 111111111, zone)
	finished := time.Date(2026, 10, 27, 9, 4, 59, 222222222, zone)

	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		Domain:     "v4.example.com",
		IP:         "203.0.113.10",
		Source:     "trace",
		Action:     updater.ActionUnchanged,
		DetectedAt: &detected,
		FinishedAt: &finished,
	}}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got, _ := readSnapshot(t, path)
	want := decodedSnapshot{
		Version:   1,
		StartedAt: time.Date(2026, 10, 27, 7, 0, 0, 123456789, time.UTC),
		WrittenAt: time.Date(2026, 10, 27, 7, 5, 0, 987654321, time.UTC),
		Period:    "5m0s",
		Families: []decodedFamily{
			{
				Family:     "ipv4",
				Domain:     "v4.example.com",
				RecordType: "A",
				Detection: &decodedDetection{
					At:     time.Date(2026, 10, 27, 7, 4, 58, 111111111, time.UTC),
					IP:     "203.0.113.10",
					Source: "trace",
				},
				Execution: &decodedExecution{
					At:     time.Date(2026, 10, 27, 7, 4, 59, 222222222, time.UTC),
					Action: updater.ActionUnchanged,
				},
			},
			{
				Family:     "ipv6",
				Domain:     "v6.example.com",
				RecordType: "AAAA",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot mismatch\ngot:  %+v\nwant: %+v", got, want)
	}
}

func sortedKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode object %s: %v", raw, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestReporterReport_KeySets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	zone := time.FixedZone("UTC+2", 2*60*60)
	now := time.Date(2026, 10, 27, 10, 5, 1, 734512000, zone)
	reporter := New(Config{
		Path:     path,
		Period:   5 * time.Minute,
		DomainV4: "v4.example.com",
		DomainV6: "v6.example.com",
		Now:      func() time.Time { return now },
	})
	detected := now.Add(-time.Second)
	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		IP:         "203.0.113.10",
		Source:     "trace",
		Action:     updater.ActionUnchanged,
		DetectedAt: &detected,
		FinishedAt: &now,
	}}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	if got, want := sortedKeys(t, data), []string{"families", "period", "started_at", "version", "written_at"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("top-level keys = %q, want %q", got, want)
	}
	var top struct {
		WrittenAt json.RawMessage   `json:"written_at"`
		Families  []json.RawMessage `json:"families"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("decode snapshot %s: %v", data, err)
	}
	if got, want := string(top.WrittenAt), `"2026-10-27T08:05:01.734512Z"`; got != want {
		t.Fatalf("written_at = %s, want %s", got, want)
	}
	if len(top.Families) != 2 {
		t.Fatalf("families = %s, want 2 entries", data)
	}
	if got, want := sortedKeys(t, top.Families[0]), []string{"detection", "domain", "execution", "family", "record_type"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reported family keys = %q, want %q", got, want)
	}
	if got, want := sortedKeys(t, top.Families[1]), []string{"domain", "family", "record_type"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unreported family keys = %q, want %q", got, want)
	}
	var family struct {
		Detection json.RawMessage `json:"detection"`
		Execution json.RawMessage `json:"execution"`
	}
	if err := json.Unmarshal(top.Families[0], &family); err != nil {
		t.Fatalf("decode family %s: %v", top.Families[0], err)
	}
	if got, want := sortedKeys(t, family.Detection), []string{"at", "error", "ip", "source"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("detection keys = %q, want %q", got, want)
	}
	if got, want := sortedKeys(t, family.Execution), []string{"action", "at", "error", "warning"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("execution keys = %q, want %q", got, want)
	}
}

func TestReporterReport_DetectionFailureClearsPreviousIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	reporter := New(Config{Path: path, DomainV4: "home.example.com", Now: func() time.Time { return now }})
	oldDetected := now.Add(-2 * time.Minute)
	oldFinished := oldDetected.Add(time.Second)
	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		IP:         "203.0.113.10",
		Source:     "trace",
		Action:     updater.ActionUpdated,
		DetectedAt: &oldDetected,
		FinishedAt: &oldFinished,
	}}); err != nil {
		t.Fatalf("initial Report: %v", err)
	}

	newDetected := now.Add(-time.Minute)
	newFinished := newDetected.Add(time.Second)
	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		Action:     updater.ActionError,
		Error:      "detect failed",
		DetectedAt: &newDetected,
		FinishedAt: &newFinished,
	}}); err != nil {
		t.Fatalf("failure Report: %v", err)
	}

	got, raw := readSnapshot(t, path)
	family := got.Families[0]
	wantDetection := &decodedDetection{At: newDetected, Error: "detect failed"}
	if !reflect.DeepEqual(family.Detection, wantDetection) {
		t.Fatalf("detection = %+v, want %+v", family.Detection, wantDetection)
	}
	wantExecution := &decodedExecution{At: newFinished, Action: updater.ActionError}
	if !reflect.DeepEqual(family.Execution, wantExecution) {
		t.Fatalf("execution = %+v, want %+v", family.Execution, wantExecution)
	}
	if bytes.Contains(raw, []byte("203.0.113.10")) {
		t.Fatalf("snapshot retains stale successful IP: %s", raw)
	}
}

func TestReporterReport_NoDetectionPreservesPreviousDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	reporter := New(Config{Path: path, DomainV4: "home.example.com", Now: func() time.Time { return now }})
	detected := now.Add(-2 * time.Minute)
	finished := detected.Add(time.Second)
	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		IP:         "203.0.113.10",
		Source:     "trace",
		Action:     updater.ActionCreated,
		DetectedAt: &detected,
		FinishedAt: &finished,
	}}); err != nil {
		t.Fatalf("initial Report: %v", err)
	}
	before, _ := readSnapshot(t, path)

	newFinished := now.Add(-time.Minute)
	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		Action:     updater.ActionError,
		Error:      "waiting for a running update cycle: context deadline exceeded",
		FinishedAt: &newFinished,
	}}); err != nil {
		t.Fatalf("lock-wait Report: %v", err)
	}

	got, _ := readSnapshot(t, path)
	if !reflect.DeepEqual(got.Families[0].Detection, before.Families[0].Detection) {
		t.Fatalf("detection changed without DetectedAt: got %+v want %+v", got.Families[0].Detection, before.Families[0].Detection)
	}
	wantExecution := &decodedExecution{
		At:     newFinished,
		Action: updater.ActionError,
		Error:  "waiting for a running update cycle: context deadline exceeded",
	}
	if !reflect.DeepEqual(got.Families[0].Execution, wantExecution) {
		t.Fatalf("execution = %+v, want %+v", got.Families[0].Execution, wantExecution)
	}
}

func TestReporterReport_PartialFamilies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	reporter := New(Config{
		Path:     path,
		DomainV4: "v4.example.com",
		DomainV6: "v6.example.com",
		Now:      func() time.Time { return now },
	})
	detected := now.Add(-2 * time.Minute)
	finished := detected.Add(time.Second)
	if err := reporter.Report([]updater.Result{
		{Family: "ipv4", IP: "203.0.113.10", Source: "trace", Action: updater.ActionUnchanged, DetectedAt: &detected, FinishedAt: &finished},
		{Family: "ipv6", IP: "2001:db8::10", Source: "iface:eth0", Action: updater.ActionUnchanged, DetectedAt: &detected, FinishedAt: &finished},
	}); err != nil {
		t.Fatalf("initial Report: %v", err)
	}
	before, _ := readSnapshot(t, path)

	newDetected := now.Add(-time.Minute)
	newFinished := newDetected.Add(time.Second)
	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		Action:     updater.ActionError,
		Error:      "IPv4 detection failed",
		DetectedAt: &newDetected,
		FinishedAt: &newFinished,
	}}); err != nil {
		t.Fatalf("partial Report: %v", err)
	}

	got, _ := readSnapshot(t, path)
	if !reflect.DeepEqual(got.Families[1], before.Families[1]) {
		t.Fatalf("unreported IPv6 family changed\ngot:  %+v\nwant: %+v", got.Families[1], before.Families[1])
	}
	if got.Families[0].Detection == nil || got.Families[0].Detection.Error != "IPv4 detection failed" {
		t.Fatalf("IPv4 detection failure not retained: %+v", got.Families[0].Detection)
	}
}

func TestReporterReport_PostDetectionErrorIsExecutionError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	reporter := New(Config{Path: path, DomainV4: "home.example.com", Now: func() time.Time { return now }})
	detected := now.Add(-time.Minute)
	finished := detected.Add(time.Second)

	if err := reporter.Report([]updater.Result{{
		Family:     "ipv4",
		IP:         "203.0.113.10",
		Source:     "trace",
		Action:     updater.ActionError,
		Error:      "cloudflare api: patch failed",
		Warning:    "2 A records; only one is managed",
		DetectedAt: &detected,
		FinishedAt: &finished,
	}}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	got, _ := readSnapshot(t, path)
	family := got.Families[0]
	wantDetection := &decodedDetection{At: detected, IP: "203.0.113.10", Source: "trace"}
	if !reflect.DeepEqual(family.Detection, wantDetection) {
		t.Fatalf("detection = %+v, want %+v", family.Detection, wantDetection)
	}
	wantExecution := &decodedExecution{
		At:      finished,
		Action:  updater.ActionError,
		Error:   "cloudflare api: patch failed",
		Warning: "2 A records; only one is managed",
	}
	if !reflect.DeepEqual(family.Execution, wantExecution) {
		t.Fatalf("execution = %+v, want %+v", family.Execution, wantExecution)
	}
}

func TestReporterClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	reporter := New(Config{Path: path, DomainV4: "home.example.com"})
	if err := reporter.Report(nil); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot was not created: %v", err)
	}

	if err := reporter.Clear(); err != nil {
		t.Fatalf("Clear existing snapshot: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists or unexpected Stat error: %v", err)
	}
	if err := reporter.Clear(); err != nil {
		t.Fatalf("Clear missing snapshot: %v", err)
	}
}

func TestReporterReport_Concurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	reporter := New(Config{
		Path:     path,
		DomainV4: "v4.example.com",
		DomainV6: "v6.example.com",
		Now:      func() time.Time { return now },
	})

	const goroutines = 12
	const reportsPerGoroutine = 20
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for worker := 0; worker < goroutines; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for cycle := 0; cycle < reportsPerGoroutine; cycle++ {
				family := "ipv4"
				address := fmt.Sprintf("203.0.113.%d", worker+1)
				if worker%2 == 1 {
					family = "ipv6"
					address = fmt.Sprintf("2001:db8::%x", worker+1)
				}
				detected := now.Add(time.Duration(worker*reportsPerGoroutine+cycle) * time.Second)
				finished := detected.Add(time.Millisecond)
				if err := reporter.Report([]updater.Result{{
					Family:     family,
					IP:         address,
					Source:     "trace",
					Action:     updater.ActionUnchanged,
					DetectedAt: &detected,
					FinishedAt: &finished,
				}}); err != nil {
					errs <- fmt.Errorf("worker %d cycle %d: %w", worker, cycle, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final snapshot: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("final snapshot is invalid JSON: %s", data)
	}
	snapshot, _ := readSnapshot(t, path)
	if snapshot.Version != 1 || len(snapshot.Families) != 2 {
		t.Fatalf("final snapshot = %+v", snapshot)
	}
}

func TestReporterClear_ConcurrentWithReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfddns.status")
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	reporter := New(Config{Path: path, DomainV4: "home.example.com", Now: func() time.Time { return now }})

	const goroutines = 8
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for worker := 0; worker < goroutines; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for cycle := 0; cycle < 20; cycle++ {
				var err error
				if (worker+cycle)%3 == 0 {
					err = reporter.Clear()
				} else {
					detected := now.Add(time.Duration(cycle) * time.Second)
					finished := detected.Add(time.Millisecond)
					err = reporter.Report([]updater.Result{{
						Family: "ipv4", IP: "203.0.113.10", Source: "trace", Action: updater.ActionUnchanged,
						DetectedAt: &detected, FinishedAt: &finished,
					}})
				}
				if err != nil {
					errs <- fmt.Errorf("worker %d cycle %d: %w", worker, cycle, err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if err := reporter.Report(nil); err != nil {
		t.Fatalf("final Report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final snapshot: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("final snapshot is invalid JSON: %s", data)
	}
}
