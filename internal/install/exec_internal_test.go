package install

import (
	"strings"
	"testing"
	"time"
)

func TestRunWithin_BoundsAHungCommand(t *testing.T) {
	t0 := time.Now()
	err := runWithin(100*time.Millisecond, "sleep", "10")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("the command was not killed at the deadline")
	}
	// Init commands fork helpers (rc-service → openrc-run.sh →
	// supervise-daemon) that inherit the output pipe; killing only the direct
	// child would leave the helper holding the pipe and the wait blocked
	// until it exits on its own.
	t0 = time.Now()
	err = runWithin(100*time.Millisecond, "sh", "-c", "sleep 10 & wait")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatal("the command's helper process kept the wait open past the deadline")
	}
	if err := runWithin(5*time.Second, "sh", "-c", "echo oops >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "oops") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("failure must carry status and output: %v", err)
	}
	if err := runWithin(5*time.Second, "true"); err != nil {
		t.Fatal(err)
	}
}
