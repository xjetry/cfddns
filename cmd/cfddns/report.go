package main

import (
	"fmt"

	"cfddns/internal/config"
	"cfddns/internal/updater"
)

// errorRepeatEvery is config.ErrorRepeatEvery, the one place the number is
// chosen and explained; the help text quotes the same value.
const errorRepeatEvery = config.ErrorRepeatEvery

// reporter decides which results reach the log. A family's state is its
// action, error and warning; a cycle is logged when that state differs from
// the previous cycle's, created and updated are logged every time, and an
// unchanged error state is logged again every errorRepeatEvery cycles. A
// steady daemon therefore writes nothing per cycle, whether it is healthy,
// carrying a duplicate-record warning, or failing the same way.
//
// report is called from Updater.Observe, which runs it inside the cycle
// lock: calls never overlap and arrive in cycle order, so the state map
// needs no lock of its own. Calling report from anywhere else would break
// that.
type reporter struct {
	logf func(format string, args ...any)
	last map[string]familyState // by family
}

type familyState struct {
	action, err, warning string
	// consecutive counts cycles in a row that reported exactly this state.
	consecutive int
}

func newReporter(logf func(string, ...any)) *reporter {
	return &reporter{logf: logf, last: map[string]familyState{}}
}

func (r *reporter) report(results []updater.Result) {
	for _, res := range results {
		cur := familyState{action: res.Action, err: res.Error, warning: res.Warning, consecutive: 1}
		prev, seen := r.last[res.Family]
		same := seen && prev.action == cur.action && prev.err == cur.err && prev.warning == cur.warning
		if same {
			cur.consecutive = prev.consecutive + 1
		}
		r.last[res.Family] = cur

		switch {
		case res.Action == updater.ActionCreated || res.Action == updater.ActionUpdated:
			r.logf("%s", format(res))
		case !same:
			r.logf("%s", format(res))
		case res.Action == updater.ActionError && cur.consecutive%errorRepeatEvery == 0:
			r.logf("%s (%d consecutive cycles)", format(res), cur.consecutive)
		}
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
