package main

import (
	"fmt"

	"cfddns/internal/updater"
)

// reporter decides which results reach the log. created, updated and error
// results and anything carrying a warning are always logged; "unchanged" is
// logged only when the family's previous result was something else (first
// cycle, or recovery after an error), so a steady daemon writes nothing per
// cycle.
type reporter struct {
	logf func(format string, args ...any)
	last map[string]string // family → previous action
}

func newReporter(logf func(string, ...any)) *reporter {
	return &reporter{logf: logf, last: map[string]string{}}
}

func (r *reporter) report(results []updater.Result) {
	for _, res := range results {
		prev, seen := r.last[res.Family]
		r.last[res.Family] = res.Action
		if res.Action == updater.ActionUnchanged && seen && prev == updater.ActionUnchanged && res.Warning == "" {
			continue
		}
		r.logf("%s", format(res))
	}
}

func format(res updater.Result) string {
	s := fmt.Sprintf("%s %s %s", res.Family, res.Domain, res.Action)
	if res.Action == updater.ActionError {
		return s + ": " + res.Error
	}
	s += " ip=" + res.IP
	if res.Source != "" && res.Source != "trace" {
		s += " source=" + res.Source
	}
	if res.Warning != "" {
		s += " warning: " + res.Warning
	}
	return s
}
