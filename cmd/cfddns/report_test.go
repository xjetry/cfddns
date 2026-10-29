package main

import (
	"fmt"
	"strings"
	"testing"

	"cfddns/internal/updater"
)

func TestReporter_SilentInSteadyState(t *testing.T) {
	var lines []string
	rep := newReporter(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	r := func(action, errMsg string) updater.Result {
		return updater.Result{Family: "ipv4", Domain: "h.example.com", IP: "198.51.100.1", Source: "trace", Action: action, Error: errMsg}
	}

	seq := []updater.Result{
		r(updater.ActionUnchanged, ""), // first cycle: logged
		r(updater.ActionUnchanged, ""), // steady: silent
		r(updater.ActionUnchanged, ""), // steady: silent
		r(updater.ActionUpdated, ""),   // logged
		r(updater.ActionUnchanged, ""), // after a change: logged once
		r(updater.ActionUnchanged, ""), // silent
		r(updater.ActionError, "boom"), // logged
		r(updater.ActionError, "boom"), // same failure again: silent
		r(updater.ActionError, "bang"), // a different failure: logged
		r(updater.ActionUnchanged, ""), // recovery: logged
		r(updater.ActionUnchanged, ""), // silent
	}
	for _, res := range seq {
		rep.report([]updater.Result{res})
	}
	want := []string{
		"ipv4 h.example.com unchanged ip=198.51.100.1",
		"ipv4 h.example.com updated ip=198.51.100.1",
		"ipv4 h.example.com unchanged ip=198.51.100.1",
		"ipv4 h.example.com error: boom",
		"ipv4 h.example.com error: bang",
		"ipv4 h.example.com unchanged ip=198.51.100.1",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestReporter_FamiliesTrackedSeparately(t *testing.T) {
	var lines []string
	rep := newReporter(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	rep.report([]updater.Result{
		{Family: "ipv4", Domain: "h.example.com", IP: "198.51.100.1", Action: updater.ActionUnchanged},
		{Family: "ipv6", Domain: "h.example.com", Action: updater.ActionError, Error: "no route"},
	})
	rep.report([]updater.Result{
		{Family: "ipv4", Domain: "h.example.com", IP: "198.51.100.1", Action: updater.ActionUnchanged},
		{Family: "ipv6", Domain: "h.example.com", IP: "2001:db8::1", Source: "iface:eth0", Action: updater.ActionUnchanged},
	})
	want := []string{
		"ipv4 h.example.com unchanged ip=198.51.100.1",
		"ipv6 h.example.com error: no route",
		"ipv6 h.example.com unchanged ip=2001:db8::1 source=iface:eth0",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestReporter_WarningBreaksSilence(t *testing.T) {
	var lines []string
	rep := newReporter(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	base := updater.Result{Family: "ipv4", Domain: "h.example.com", IP: "198.51.100.1", Action: updater.ActionUnchanged}
	rep.report([]updater.Result{base})
	rep.report([]updater.Result{base})
	withWarn := base
	withWarn.Warning = "3 A records named h.example.com; only rec-a is managed, the other 2 keep their content"
	rep.report([]updater.Result{withWarn})
	if len(lines) != 2 || !strings.Contains(lines[1], "warning: 3 A records") {
		t.Fatalf("lines: %q", lines)
	}
	// The same warning on the next unchanged cycles is steady state too:
	// the HTTP body still carries it every time, the log once.
	rep.report([]updater.Result{withWarn})
	rep.report([]updater.Result{withWarn})
	if len(lines) != 2 {
		t.Fatalf("a persisting warning must not log every cycle: %q", lines)
	}
	// The warning going away is a state change worth a line.
	rep.report([]updater.Result{base})
	if len(lines) != 3 || strings.Contains(lines[2], "warning") {
		t.Fatalf("lines: %q", lines)
	}
}

// A failure that persists for days must not fill the log at one line per
// cycle (an OpenRC host writes to a plain file with no rotation), but an
// operator reading the log must still see that it is ongoing.
func TestReporter_PersistentErrorRepeatsEveryNCycles(t *testing.T) {
	var lines []string
	rep := newReporter(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	res := updater.Result{Family: "ipv4", Domain: "h.example.com", Action: updater.ActionError, Error: "boom"}
	for i := 0; i < 2*errorRepeatEvery; i++ {
		rep.report([]updater.Result{res})
	}
	want := []string{
		"ipv4 h.example.com error: boom",
		fmt.Sprintf("ipv4 h.example.com error: boom (%d consecutive cycles)", errorRepeatEvery),
		fmt.Sprintf("ipv4 h.example.com error: boom (%d consecutive cycles)", 2*errorRepeatEvery),
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	// Recovery resets the count: the next failure is new again.
	rep.report([]updater.Result{{Family: "ipv4", Domain: "h.example.com", IP: "198.51.100.1", Action: updater.ActionUnchanged}})
	rep.report([]updater.Result{res})
	if len(lines) != 5 || lines[4] != "ipv4 h.example.com error: boom" {
		t.Fatalf("lines: %q", lines)
	}
}
