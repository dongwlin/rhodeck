package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/stretchr/testify/require"
)

// TestCommandSurfaceIsStable pins the command tree and the flags of the plan
// commands. The reported command list and the help text are derived from this
// tree, so a change here is a change of the published surface.
func TestCommandSurfaceIsStable(t *testing.T) {
	app := New("dev", strings.NewReader(""), output.NewWriter(io.Discard))
	t.Cleanup(app.sink.Close)
	root := app.Root()
	// Cobra adds the help command to the tree when it initialises the
	// command, so the reported list is read from an initialised tree.
	root.InitDefaultHelpCmd()
	require.Equal(t,
		[]string{"run", "dry-run", "doctor", "capabilities", "version", "help"},
		app.commandNames(root),
		"capabilities command list drifted from the command tree")
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
