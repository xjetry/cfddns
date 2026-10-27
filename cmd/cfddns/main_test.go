package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cfddns/internal/cloudflare"
	"cfddns/internal/inspector"
	"cfddns/internal/status"
	"cfddns/internal/updater"
)

type inspectDNS struct {
	zoneErrors map[string]error
	records    map[string][]cloudflare.Record
}

func (f *inspectDNS) ZoneIDFor(_ context.Context, domain string) (string, error) {
	if err := f.zoneErrors[domain]; err != nil {
		return "", err
	}
	return "zone-test", nil
}

func (f *inspectDNS) FindRecords(_ context.Context, _, typ, name string) ([]cloudflare.Record, error) {
	return f.records[typ+"|"+name], nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestRunInspect_JSONContract(t *testing.T) {
	queriedAt := time.Date(2026, 10, 27, 10, 10, 0, 531208000, time.FixedZone("UTC+2", 2*60*60))
	insp := &inspector.Inspector{
		DomainV4: "v4.example.com",
		DomainV6: "v6.example.com",
		DNS: &inspectDNS{
			zoneErrors: map[string]error{"v4.example.com": errors.New("zone lookup failed")},
			records: map[string][]cloudflare.Record{
				"AAAA|v6.example.com": {{ID: "rec-6", Type: "AAAA", Name: "v6.example.com", Content: "2001:db8::10"}},
			},
		},
		Now: func() time.Time { return queriedAt },
	}
	var out bytes.Buffer

	if err := runInspect(context.Background(), insp, &out); err != nil {
		t.Fatalf("runInspect = %v, want nil", err)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope %s: %v", out.Bytes(), err)
	}
	if got, want := sortedKeys(envelope), []string{"ok", "results"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("envelope keys = %q, want %q", got, want)
	}
	var ok bool
	if err := json.Unmarshal(envelope["ok"], &ok); err != nil {
		t.Fatalf("decode ok %s: %v", envelope["ok"], err)
	}
	if ok {
		t.Fatalf("ok = true, want false with one failed family")
	}

	var results []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["results"], &results); err != nil {
		t.Fatalf("decode results %s: %v", envelope["results"], err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %s, want 2 entries", envelope["results"])
	}
	wantKeys := []string{"content", "domain", "error", "family", "found", "queried_at", "record_type", "warning"}
	for i, wantFamily := range []string{`"ipv4"`, `"ipv6"`} {
		if got := string(results[i]["family"]); got != wantFamily {
			t.Fatalf("results[%d].family = %s, want %s", i, got, wantFamily)
		}
		if got := sortedKeys(results[i]); !reflect.DeepEqual(got, wantKeys) {
			t.Fatalf("results[%d] keys = %q, want %q", i, got, wantKeys)
		}
		if got, want := string(results[i]["queried_at"]), `"2026-10-27T08:10:00.531208Z"`; got != want {
			t.Fatalf("results[%d].queried_at = %s, want %s", i, got, want)
		}
	}
	if got := string(results[0]["error"]); got != `"zone lookup failed"` {
		t.Fatalf("failed result error = %s, want zone lookup failed", got)
	}
	if got := string(results[1]["found"]); got != "true" {
		t.Fatalf("successful result found = %s, want true", got)
	}
	if got := string(results[1]["error"]); got != `""` {
		t.Fatalf("successful result error = %s, want empty", got)
	}
}

func TestReportStatus_LogsAndContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "cfddns.status")
	rep := status.New(status.Config{
		Path:     path,
		Period:   time.Minute,
		DomainV4: "home.example.com",
	})
	detectedAt := time.Date(2026, time.October, 27, 10, 0, 0, 0, time.UTC)
	finishedAt := detectedAt.Add(time.Second)
	results := []updater.Result{{
		Family:     "ipv4",
		Domain:     "home.example.com",
		IP:         "192.0.2.1",
		Source:     "trace",
		Action:     updater.ActionUpdated,
		Warning:    "duplicate warning",
		DetectedAt: &detectedAt,
		FinishedAt: &finishedAt,
	}}
	before := append([]updater.Result(nil), results...)
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	reportStatus(rep, results, logf)

	if len(logs) != 1 || !strings.Contains(logs[0], filepath.Dir(path)) {
		t.Fatalf("logs = %q, want one status error naming %q", logs, filepath.Dir(path))
	}
	if !reflect.DeepEqual(results, before) {
		t.Fatalf("update results changed:\ngot  %+v\nwant %+v", results, before)
	}
}

func TestReportStatus_NilReporter(t *testing.T) {
	reportStatus(nil, []updater.Result{{Family: "ipv4"}}, func(string, ...any) {
		t.Fatal("disabled status must not log")
	})
}
