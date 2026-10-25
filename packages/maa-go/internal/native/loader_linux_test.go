//go:build linux

package native

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ebitengine/purego"
)

// 本文件是与真实系统 loader 的冒烟测试:只执行打开与符号查找,
// 不调用任何函数地址,也不加载 MaaCore,因此不需要 C 编译器。
// 因环境缺失而跳过(t.Skip)的用例在 go test 结果中计为 SKIP 而
// 非 PASS,不冒充真实环境验收通过。

// TestLoadFlagsFrozen 把冻结的加载标志数值钉死:RTLD_NOW|RTLD_LOCAL
// 在 Linux 上恰为 0x2,且不得混入 RTLD_GLOBAL 或 RTLD_LAZY。
func TestLoadFlagsFrozen(t *testing.T) {
	if loadFlags != 0x2 {
		t.Errorf("loadFlags = %#x, want 0x2 (RTLD_NOW|RTLD_LOCAL)", loadFlags)
	}
	if loadFlags&purego.RTLD_GLOBAL != 0 {
		t.Errorf("loadFlags = %#x, 不得包含 RTLD_GLOBAL (冻结策略:不扩大符号可见性)", loadFlags)
	}
	if loadFlags&purego.RTLD_LAZY != 0 {
		t.Errorf("loadFlags = %#x, 不得包含 RTLD_LAZY (冻结策略:加载期完成全部重定位)", loadFlags)
	}
}

// TestOpenMissingLibraryReportsSystemCause 用真实系统 loader 打开
// 不存在的库:错误必须是 *LibraryLoadError,底层原因非 nil 且
// 文本包含系统 loader 的「文件不存在」短语(glibc 与 musl 的
// dlerror 文本均含该短语),外层文本保留路径与目标平台。
func TestOpenMissingLibraryReportsSystemCause(t *testing.T) {
	const missing = "/nonexistent/libMaaCore.so"

	lib, err := newLoader(dlopenPrimitives{}, abiProfile).Open(missing)
	if lib != nil {
		t.Fatalf("打开不存在的库不得返回 Library, got %+v", lib)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As 未得到 *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Err == nil {
		t.Fatal("必须保留底层加载错误")
	}
	if !strings.Contains(lle.Err.Error(), "No such file or directory") {
		t.Errorf("底层错误文本 = %q, want it to contain %q", lle.Err.Error(), "No such file or directory")
	}
	if lle.Path != missing {
		t.Errorf("Path = %q, want %q", lle.Path, missing)
	}
	if lle.GOOS != runtime.GOOS || lle.GOARCH != runtime.GOARCH {
		t.Errorf("GOOS/GOARCH = %s/%s, want %s/%s", lle.GOOS, lle.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	text := err.Error()
	if want := runtime.GOOS + "/" + runtime.GOARCH; !strings.Contains(text, want) {
		t.Errorf("Error() = %q, want it to contain target platform %q", text, want)
	}
	if !strings.Contains(text, missing) {
		t.Errorf("Error() = %q, want it to contain path %q", text, missing)
	}
}

// TestOpenRejectsInvalidELF 用真实系统 loader 打开非 ELF 文件:
// 失败必须是 *LibraryLoadError 且保留非 nil 的底层原因;具体
// 文本随 libc 实现而异,不作断言。
func TestOpenRejectsInvalidELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-an-elf.so")
	if err := os.WriteFile(path, []byte("this file is not an ELF object, just plain bytes"), 0o755); err != nil {
		t.Fatalf("写入夹具文件失败: %v", err)
	}

	lib, err := newLoader(dlopenPrimitives{}, abiProfile).Open(path)
	if lib != nil {
		t.Fatalf("打开非 ELF 文件不得返回 Library, got %+v", lib)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As 未得到 *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Err == nil {
		t.Error("必须保留底层加载错误")
	}
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
		t.Skip("本机不存在候选 libm.so.6,跳过真实 loader 冒烟测试;跳过不计为通过")
	}

	lib, err := newLoader(dlopenPrimitives{}, abiProfile).Open(found)
	if err != nil {
		t.Fatalf("打开真实系统库 %s 失败: %v", found, err)
	}
	if lib.Path() != found {
		t.Errorf("Library.Path() = %q, want %q", lib.Path(), found)
	}
	if lib.Name() != "libm.so.6" {
		t.Errorf("Library.Name() = %q, want %q", lib.Name(), "libm.so.6")
	}

	sqrtAddr, err := lib.Lookup("sqrt")
	if err != nil {
		t.Fatalf("Lookup(sqrt) 返回意外错误: %v", err)
	}
	if sqrtAddr == 0 {
		t.Fatal("Lookup(sqrt) 返回零地址")
	}

	const absent = "AsstNoSuchSymbolRhodeck"
	_, err = lib.Lookup(absent)
	var sle *SymbolLookupError
	if !errors.As(err, &sle) {
		t.Fatalf("errors.As 未得到 *SymbolLookupError, got %T: %v", err, err)
	}
	if sle.Symbol != absent || sle.LibraryPath != found || sle.Profile != abiProfile {
		t.Errorf("诊断字段不符: %+v", sle)
	}
}

// TestNewLoaderProductionWiring 验证生产构造入口 NewLoader 接线正确:
// 使用冻结 ABI 组合标识与真实平台原语。用真实系统 loader 打开不存在
// 的库,失败必须是携带系统「文件不存在」原因的 *LibraryLoadError,
// 证明请求确实进入了 dlopenPrimitives,而非某个假原语。全程不调用
// 任何函数地址。
func TestNewLoaderProductionWiring(t *testing.T) {
	const missing = "/nonexistent/rhodeck-libMaaCore.so"

	ldr := NewLoader()
	if ldr == nil {
		t.Fatal("NewLoader 必须返回生产加载器")
	}
	if ldr.profile != abiProfile {
		t.Errorf("生产加载器的 ABI 组合标识 = %q, want %q", ldr.profile, abiProfile)
	}

	lib, err := ldr.Open(missing)
	if lib != nil {
		t.Fatalf("打开不存在的库不得返回 Library, got %+v", lib)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As 未得到 *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Path != missing {
		t.Errorf("Path = %q, want %q", lle.Path, missing)
	}
	if lle.Err == nil || !strings.Contains(lle.Err.Error(), "No such file or directory") {
		t.Errorf("底层错误 = %v, want 保留系统 loader 的「文件不存在」原因", lle.Err)
	}
}
