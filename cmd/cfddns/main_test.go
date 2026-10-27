package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cfddns/internal/status"
	"cfddns/internal/updater"
)

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
