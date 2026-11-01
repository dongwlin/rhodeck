//go:build darwin

package native

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func systemLoaderProbe() (string, string) {
	return "/usr/lib/libSystem.B.dylib", "getpid"
}

func TestDarwinLoadFlags(t *testing.T) {
	require.True(t, loadFlags == 0x6, "loadFlags = %#x, want RTLD_NOW|RTLD_LOCAL (0x6)", loadFlags)
}
