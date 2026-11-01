//go:build linux

package native

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ebitengine/purego"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件是与真实系统 loader 的冒烟测试:只执行打开与符号查找,
// 不调用任何函数地址,也不加载 MaaCore,因此不需要 C 编译器。
// 因环境缺失而跳过(t.Skip)的用例在 go test 结果中计为 SKIP 而
// 非 PASS,不冒充真实环境验收通过。

// TestLoadFlagsFrozen 把冻结的加载标志数值钉死:RTLD_NOW|RTLD_LOCAL
// 在 Linux 上恰为 0x2,且不得混入 RTLD_GLOBAL 或 RTLD_LAZY。
func TestLoadFlagsFrozen(t *testing.T) {
	assert.True(t, loadFlags == 0x2, "loadFlags = %#x, want 0x2 (RTLD_NOW|RTLD_LOCAL)", loadFlags)
	assert.True(t, loadFlags&purego.RTLD_GLOBAL == 0,
		"loadFlags = %#x, must not contain RTLD_GLOBAL (frozen policy: do not widen symbol visibility)", loadFlags)
	assert.True(t, loadFlags&purego.RTLD_LAZY == 0,
		"loadFlags = %#x, must not contain RTLD_LAZY (frozen policy: complete all relocations at load time)", loadFlags)
}

// TestOpenMissingLibraryReportsSystemCause 用真实系统 loader 打开
// 不存在的库:错误必须是 *LibraryLoadError,底层原因非 nil 且
// 文本包含系统 loader 的「文件不存在」短语(glibc 与 musl 的
// dlerror 文本均含该短语),外层文本保留路径与目标平台。
func TestOpenMissingLibraryReportsSystemCause(t *testing.T) {
	const missing = "/nonexistent/libMaaCore.so"

	lib, err := newLoader(dlopenPrimitives{}, abiProfile).Open(missing)
	require.Nil(t, lib, "opening a nonexistent library must not return a Library, got %+v", lib)
	var lle *LibraryLoadError
	require.ErrorAs(t, err, &lle, "errors.As did not yield a *LibraryLoadError, got %T: %v", err, err)
	require.Error(t, lle.Err, "the underlying load error must be preserved")
	assert.Contains(t, lle.Err.Error(), "No such file or directory",
		"underlying error text = %q, want it to contain %q", lle.Err.Error(), "No such file or directory")
	assert.Equal(t, missing, lle.Path, "Path = %q, want %q", lle.Path, missing)
	assert.True(t, lle.GOOS == runtime.GOOS && lle.GOARCH == runtime.GOARCH,
		"GOOS/GOARCH = %s/%s, want %s/%s", lle.GOOS, lle.GOARCH, runtime.GOOS, runtime.GOARCH)
	text := err.Error()
	assert.Contains(t, text, runtime.GOOS+"/"+runtime.GOARCH,
		"Error() = %q, want it to contain target platform %q", text, runtime.GOOS+"/"+runtime.GOARCH)
	assert.Contains(t, text, missing, "Error() = %q, want it to contain path %q", text, missing)
}

// TestOpenRejectsInvalidELF 用真实系统 loader 打开非 ELF 文件:
// 失败必须是 *LibraryLoadError 且保留非 nil 的底层原因;具体
// 文本随 libc 实现而异,不作断言。
func TestOpenRejectsInvalidELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-an-elf.so")
	err := os.WriteFile(path, []byte("this file is not an ELF object, just plain bytes"), 0o755)
	require.NoError(t, err, "failed to write the fixture file: %v", err)

	lib, err := newLoader(dlopenPrimitives{}, abiProfile).Open(path)
	require.Nil(t, lib, "opening a non-ELF file must not return a Library, got %+v", lib)
	var lle *LibraryLoadError
	require.ErrorAs(t, err, &lle, "errors.As did not yield a *LibraryLoadError, got %T: %v", err, err)
	assert.Error(t, lle.Err, "the underlying load error must be preserved")
}

// TestOpenAndLookupRealSystemLibrary 打开本机真实存在的 libm.so.6
// 并查找符号:合法符号得到非零地址,不存在的符号得到带 ABI 组合
// 标识的 *SymbolLookupError。候选路径都不存在时跳过(计为
// SKIP,不计为通过)。全程不调用任何函数地址。
func TestOpenAndLookupRealSystemLibrary(t *testing.T) {
	candidates := []string{
		"/lib64/libm.so.6",
		"/usr/lib64/libm.so.6",
		"/lib/x86_64-linux-gnu/libm.so.6",
	}
	found := ""
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			found = c
			break
		}
	}
	if found == "" {
		t.Skip("no candidate libm.so.6 exists on this machine; skipping the real loader smoke test; a skip does not count as a pass")
	}

	lib, err := newLoader(dlopenPrimitives{}, abiProfile).Open(found)
	require.NoError(t, err, "failed to open the real system library %s: %v", found, err)
	assert.Equal(t, found, lib.Path(), "Library.Path() = %q, want %q", lib.Path(), found)
	assert.Equal(t, "libm.so.6", lib.Name(), "Library.Name() = %q, want %q", lib.Name(), "libm.so.6")

	sqrtAddr, err := lib.Lookup("sqrt")
	require.NoError(t, err, "Lookup(sqrt) returned an unexpected error: %v", err)
	require.NotZero(t, sqrtAddr, "Lookup(sqrt) returned a null address")

	const absent = "AsstNoSuchSymbolRhodeck"
	_, err = lib.Lookup(absent)
	var sle *SymbolLookupError
	require.ErrorAs(t, err, &sle, "errors.As did not yield a *SymbolLookupError, got %T: %v", err, err)
	assert.True(t, sle.Symbol == absent && sle.LibraryPath == found && sle.Profile == abiProfile,
		"diagnostic fields mismatch: %+v", sle)
}

// TestNewLoaderProductionWiring 验证生产构造入口 NewLoader 接线正确:
// 使用冻结 ABI 组合标识与真实平台原语。用真实系统 loader 打开不存在
// 的库,失败必须是携带系统「文件不存在」原因的 *LibraryLoadError,
// 证明请求确实进入了 dlopenPrimitives,而非某个假原语。全程不调用
// 任何函数地址。
func TestNewLoaderProductionWiring(t *testing.T) {
	const missing = "/nonexistent/rhodeck-libMaaCore.so"

	ldr := NewLoader()
	require.NotNil(t, ldr, "NewLoader must return the production loader")
	assert.Equal(t, abiProfile, ldr.profile, "production loader ABI profile = %q, want %q", ldr.profile, abiProfile)

	lib, err := ldr.Open(missing)
	require.Nil(t, lib, "opening a nonexistent library must not return a Library, got %+v", lib)
	var lle *LibraryLoadError
	require.ErrorAs(t, err, &lle, "errors.As did not yield a *LibraryLoadError, got %T: %v", err, err)
	assert.Equal(t, missing, lle.Path, "Path = %q, want %q", lle.Path, missing)
	assert.True(t, lle.Err != nil && strings.Contains(lle.Err.Error(), "No such file or directory"),
		"underlying error = %v, want the system loader's \"No such file or directory\" cause preserved", lle.Err)
}
