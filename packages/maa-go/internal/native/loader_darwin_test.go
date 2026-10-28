//go:build darwin

package native

import "testing"

func systemLoaderProbe() (string, string) {
	return "/usr/lib/libSystem.B.dylib", "getpid"
}

func TestDarwinLoadFlags(t *testing.T) {
	if loadFlags != 0x6 {
		t.Fatalf("loadFlags = %#x, want RTLD_NOW|RTLD_LOCAL (0x6)", loadFlags)
	}
}
