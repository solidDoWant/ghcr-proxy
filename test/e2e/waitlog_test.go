//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitLogPrefix starts every line the suite prints about a wait, so the stage
// log can be searched and summed with one pattern.
const waitLogPrefix = "e2e-wait: "

// waitStep names the test or TestMain step that is running, from the call
// stack. Code that runs outside any test function reports "cleanup".
func waitStep() string {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])

	for {
		frame, more := frames.Next()

		name := frame.Function[strings.LastIndex(frame.Function, "/")+1:]
		name = strings.TrimPrefix(name, "e2e.")

		switch {
		case name == "TestMain", strings.HasPrefix(name, "(*env).setup"):
			// setup's goroutines have no TestMain frame, so name them by their own.
			return "TestMain"
		case strings.HasPrefix(name, "Test") && !strings.Contains(name, "."):
			return name
		}

		if !more {
			return "cleanup"
		}
	}
}

// logWait prints one line for a finished wait.
func logWait(step, what string, started time.Time, detail string) {
	fmt.Fprintf(os.Stderr, "%s%s: %s took %.1fs%s\n", waitLogPrefix, step, what, time.Since(started).Seconds(), detail)
}

// timed runs a step and logs how long it took, whether or not it failed.
func timed(what string, run func() error) error {
	step, started := waitStep(), time.Now()
	defer func() { logWait(step, what, started, "") }()

	return run()
}

// pollCounter wraps a condition and counts its calls.
func pollCounter(condition func() bool) (func() bool, *int) {
	polls := new(int)

	return func() bool {
		*polls++

		return condition()
	}, polls
}

// requireEventually is require.Eventually that logs what it waited for, how
// long it took and how many polls it made.
func requireEventually(t *testing.T, what string, condition func() bool, waitFor, tick time.Duration, msgAndArgs ...any) {
	t.Helper()

	counted, polls := pollCounter(condition)
	started := time.Now()

	defer func() { logWait(t.Name(), "eventually "+what, started, fmt.Sprintf(" (%d polls)", *polls)) }()

	require.Eventually(t, counted, waitFor, tick, msgAndArgs...)
}

// assertEventually is the assert variant of requireEventually.
func assertEventually(t *testing.T, what string, condition func() bool, waitFor, tick time.Duration, msgAndArgs ...any) {
	t.Helper()

	counted, polls := pollCounter(condition)
	started := time.Now()

	defer func() { logWait(t.Name(), "eventually "+what, started, fmt.Sprintf(" (%d polls)", *polls)) }()

	assert.Eventually(t, counted, waitFor, tick, msgAndArgs...)
}

// waitingKubectl reports whether a kubectl call waits: an explicit wait, a
// rollout, or a delete (which waits for the object to go).
func waitingKubectl(args []string) bool {
	return slices.ContainsFunc(args, func(arg string) bool {
		return arg == "wait" || arg == "rollout" || arg == "delete"
	})
}

// helmWaits reports whether a helm call waits for the cluster.
func helmWaits(args []string) bool {
	return slices.Contains(args, "--wait")
}
