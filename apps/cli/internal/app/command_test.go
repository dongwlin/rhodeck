package app

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommandArgumentContract pins the rejection surface of the command tree.
// A rejected invocation prints exactly one machine error envelope on stdout,
// exactly one stable diagnostic line on stderr and exits with code 2. The
// rejected arguments themselves never reach either stream.
func TestCommandArgumentContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code string
	}{
		{name: "no command", args: nil, code: "command_required"},
		{name: "unknown command", args: []string{"unknown"}, code: "unknown_command"},
		{name: "version with extra argument", args: []string{"version", "extra"}, code: "unexpected_arguments"},
		{name: "capabilities with extra argument", args: []string{"capabilities", "extra"}, code: "unexpected_arguments"},
		{name: "run with extra argument", args: []string{"run", "extra"}, code: "invalid_arguments"},
		{name: "run with unknown flag", args: []string{"run", "--bogus"}, code: "invalid_arguments"},
		{name: "negative timeout", args: []string{"dry-run", "--timeout=-1s"}, code: "invalid_arguments"},
		{name: "unknown format", args: []string{"--format", "xml"}, code: "invalid_arguments"},
		{name: "single dash long flag", args: []string{"dry-run", "-input", "-"}, code: "invalid_arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			code := runCLI("dev", context.Background(), tc.args, strings.NewReader(""), &out, &diagnostics)
			require.Equal(t, output.ExitRejected, code, "args %q: stdout %s stderr %s", tc.args, out.String(), diagnostics.String())
			lines := decodeLines(t, out.Bytes())
			require.Len(t, lines, 1, "args %q must print one JSON object: %s", tc.args, out.String())
			var failure output.Error
			require.NoError(t, json.Unmarshal(lines[0], &failure), "args %q: %s", tc.args, out.String())
			assert.Equal(t, "error", failure.Type, "args %q: %s", tc.args, out.String())
			assert.Equal(t, tc.code, failure.Code, "args %q: %s", tc.args, out.String())
			assert.Equal(t, "rhodeck-cli: "+tc.code+"\n", diagnostics.String(), "args %q diagnostic line", tc.args)
			// The rejected arguments are never echoed back. A value that
			// coincides with the fixed vocabulary of the output (the envelope
			// field names, the diagnostic prefix or the stable code itself)
			// cannot be told apart from the output and is exempt.
			vocabulary := "schema_version type error code " + tc.code + " rhodeck-cli:"
			for _, value := range tc.args {
				if len(value) < 2 || strings.Contains(vocabulary, value) {
					continue
				}
				assert.NotContains(t, out.String(), value, "args %q leaked into stdout", tc.args)
				assert.NotContains(t, diagnostics.String(), value, "args %q leaked into stderr", tc.args)
			}
		})
	}
}

// TestHelpRenderingMatchesTheRequestedFormat keeps the two help renderings
// in step: the text form and the machine envelope carry the same help text,
// and each command documents its own flags.
func TestHelpRenderingMatchesTheRequestedFormat(t *testing.T) {
	var textOut, textDiagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), []string{"help"}, strings.NewReader(""), &textOut, &textDiagnostics)
	require.Equal(t, output.ExitSuccess, code, "stderr: %s", textDiagnostics.String())
	require.Empty(t, textDiagnostics.String(), "unexpected diagnostics")
	text := textOut.String()
	require.NotEmpty(t, strings.TrimSpace(text), "text help printed nothing")
	require.False(t, strings.HasPrefix(strings.TrimSpace(text), "{"), "help must print human-readable text, not a machine envelope: %s", text)

	var machineOut, machineDiagnostics bytes.Buffer
	code = runCLI("dev", context.Background(), []string{"help", "--format", "json"}, strings.NewReader(""), &machineOut, &machineDiagnostics)
	require.Equal(t, output.ExitSuccess, code, "stderr: %s", machineDiagnostics.String())
	require.Empty(t, machineDiagnostics.String(), "unexpected diagnostics")
	lines := decodeLines(t, machineOut.Bytes())
	require.Len(t, lines, 1, "machine help must print one JSON object: %s", machineOut.String())
	var machine output.Help
	require.NoError(t, json.Unmarshal(lines[0], &machine), "machine help: %s", machineOut.String())
	require.Equal(t, "help", machine.Type, "machine help: %s", machineOut.String())
	require.NotEmpty(t, machine.Usage, "machine help carries no usage text: %s", machineOut.String())
	// The machine envelope carries the help text of the text rendering. Only
	// the trailing newline a text writer may add may differ.
	require.Equal(t, strings.TrimRight(machine.Usage, "\n"), strings.TrimRight(text, "\n"),
		"text help and machine help carry different text")

	// The text help describes the whole command surface.
	require.Contains(t, text, "rhodeck-cli", "help does not name the CLI: %s", text)
	for _, name := range []string{"run", "dry-run", "doctor", "capabilities", "version", "help"} {
		require.Contains(t, text, name, "help does not mention %q: %s", name, text)
	}

	// The run command documents its own flags, including the run-only
	// --dry-run flag.
	var runOut, runDiagnostics bytes.Buffer
	code = runCLI("dev", context.Background(), []string{"run", "--help"}, strings.NewReader(""), &runOut, &runDiagnostics)
	require.Equal(t, output.ExitSuccess, code, "stderr: %s", runDiagnostics.String())
	require.Empty(t, runDiagnostics.String(), "unexpected diagnostics")
	runText := runOut.String()
	require.NotEmpty(t, strings.TrimSpace(runText), "run help printed nothing")
	for _, flag := range []string{"--input", "--worker", "--data-dir", "--dry-run"} {
		require.Contains(t, runText, flag, "run help does not mention %q: %s", flag, runText)
	}

	// The dry-run command does not offer the run-only --dry-run flag.
	var dryOut, dryDiagnostics bytes.Buffer
	code = runCLI("dev", context.Background(), []string{"dry-run", "--help"}, strings.NewReader(""), &dryOut, &dryDiagnostics)
	require.Equal(t, output.ExitSuccess, code, "stderr: %s", dryDiagnostics.String())
	require.Empty(t, dryDiagnostics.String(), "unexpected diagnostics")
	require.NotContains(t, dryOut.String(), "--dry-run", "dry-run help offers the run-only flag: %s", dryOut.String())

	// Both spellings of the machine form succeed and report the run help.
	for _, args := range [][]string{{"help", "run", "--format", "json"}, {"run", "--help", "--format", "json"}} {
		var out, diagnostics bytes.Buffer
		code := runCLI("dev", context.Background(), args, strings.NewReader(""), &out, &diagnostics)
		require.Equal(t, output.ExitSuccess, code, "args %q: stderr %s", args, diagnostics.String())
		require.Empty(t, diagnostics.String(), "args %q: unexpected diagnostics", args)
		lines := decodeLines(t, out.Bytes())
		require.Len(t, lines, 1, "args %q must print one JSON object: %s", args, out.String())
		var help output.Help
		require.NoError(t, json.Unmarshal(lines[0], &help), "args %q: %s", args, out.String())
		require.Equal(t, "help", help.Type, "args %q: %s", args, out.String())
		require.NotEmpty(t, help.Usage, "args %q carries no usage text: %s", args, out.String())
		require.Contains(t, help.Usage, "--input", "args %q does not describe the run flags: %s", args, help.Usage)
	}
}

// TestCapabilitiesCommandListMatchesTheCommandTree keeps the reported
// command list identical to the commands the CLI actually exposes.
func TestCapabilitiesCommandListMatchesTheCommandTree(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), []string{"capabilities"}, strings.NewReader(""), &out, &diagnostics)
	require.Equal(t, output.ExitSuccess, code, "stderr: %s", diagnostics.String())
	require.Empty(t, diagnostics.String(), "unexpected diagnostics")
	lines := decodeLines(t, out.Bytes())
	require.Len(t, lines, 1, "capabilities must print one JSON object: %s", out.String())
	var caps output.Capabilities
	require.NoError(t, json.Unmarshal(lines[0], &caps), "capabilities: %s", out.String())
	require.Equal(t, []string{"run", "dry-run", "doctor", "capabilities", "version", "help"}, caps.Commands,
		"capabilities command list drifted from the command tree")
}

// TestUnknownCommandIsRejected checks that an unknown command name produces
// the stable unknown_command diagnostic and no other output.
func TestUnknownCommandIsRejected(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"help", "bogus"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			code := runCLI("dev", context.Background(), args, strings.NewReader(""), &out, &diagnostics)
			require.Equal(t, output.ExitRejected, code, "args %q: stdout %s stderr %s", args, out.String(), diagnostics.String())
			lines := decodeLines(t, out.Bytes())
			require.Len(t, lines, 1, "args %q must print one JSON object: %s", args, out.String())
			var failure output.Error
			require.NoError(t, json.Unmarshal(lines[0], &failure), "args %q: %s", args, out.String())
			require.Equal(t, "error", failure.Type, "args %q: %s", args, out.String())
			require.Equal(t, "unknown_command", failure.Code, "args %q: %s", args, out.String())
			require.Equal(t, "rhodeck-cli: unknown_command\n", diagnostics.String(), "args %q diagnostic line", args)
		})
	}
}
