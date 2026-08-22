package activities

import (
	"log/slog"
	"os"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// newTestLogger returns a slog logger that writes to stdout at Debug level
// for test output visibility.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// newActEnv creates a properly-initialised TestActivityEnvironment.
// Using new(testsuite.TestActivityEnvironment) leaves impl nil and panics;
// always use this helper instead.
func newActEnv(_ *testing.T) *testsuite.TestActivityEnvironment {
	var ts testsuite.WorkflowTestSuite
	return ts.NewTestActivityEnvironment()
}
