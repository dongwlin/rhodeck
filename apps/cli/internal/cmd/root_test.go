package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/stretchr/testify/require"
)

// TestPlanCommandFlags checks the shared flags and the run-only preview flag.
func TestPlanCommandFlags(t *testing.T) {
	app := New("dev", strings.NewReader(""), output.NewWriter(io.Discard))
	t.Cleanup(app.sink.Close)
	root := app.Root()
	for _, name := range []string{"run", "dry-run", "doctor"} {
		cmd, _, err := root.Find([]string{name})
		require.NoError(t, err, "command %q", name)
		for _, flag := range []string{"input", "worker", "data-dir", "timeout", "startup-timeout",
			"request-timeout", "stop-timeout", "shutdown-timeout", "output-timeout", "include-raw-events"} {
			require.NotNil(t, cmd.Flags().Lookup(flag), "command %q lacks --%s", name, flag)
		}
	}
	// The preview switch belongs to run only.
	run, _, err := root.Find([]string{"run"})
	require.NoError(t, err)
	require.NotNil(t, run.Flags().Lookup("dry-run"))
	for _, name := range []string{"dry-run", "doctor"} {
		cmd, _, err := root.Find([]string{name})
		require.NoError(t, err, "command %q", name)
		require.Nil(t, cmd.Flags().Lookup("dry-run"), "command %q offers --dry-run", name)
	}
}
