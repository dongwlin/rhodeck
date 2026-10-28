package native

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakePrimitives 是可记录调用的假平台原语,在不进入系统 loader 的
// 前提下构造成功与各类失败路径。所有注入都经 loaderPrimitives 接口
// 完成,不替换生产包的全局函数变量。
type fakePrimitives struct {
	openPaths []string // open 收到的路径,按调用顺序记录

	openHandle uintptr // open 成功时返回的句柄
	openErr    error   // open 失败时返回的错误
	zeroHandle bool    // 置位时 open 返回 (0, nil) 的契约破坏情形

	lookupHandles []uintptr // lookup 收到的句柄,按调用顺序记录
	lookupNames   []string  // lookup 收到的符号名,按调用顺序记录

	lookupAddr uintptr // lookup 成功时返回的地址
	lookupErr  error   // lookup 失败时返回的错误
	nullAddr   bool    // 置位时 lookup 返回 (0, nil) 的契约破坏情形

	lookupFailures  map[string]error // 按符号名注入查找失败(优先于 lookupErr)
	lookupNullAddrs map[string]bool  // 按符号名注入 (0, nil) 契约破坏情形(优先于 nullAddr)
}

func (f *fakePrimitives) open(path string) (uintptr, error) {
	f.openPaths = append(f.openPaths, path)
	if f.openErr != nil {
		return 0, f.openErr
	}
	if f.zeroHandle {
		return 0, nil
	}
	return f.openHandle, nil
}

func (f *fakePrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	f.lookupHandles = append(f.lookupHandles, handle)
	f.lookupNames = append(f.lookupNames, name)
	if f.lookupFailures[name] != nil {
		return 0, f.lookupFailures[name]
	}
	if f.lookupNullAddrs[name] {
		return 0, nil
	}
	if f.lookupErr != nil {
		return 0, f.lookupErr
	}
	if f.nullAddr {
		return 0, nil
	}
	return f.lookupAddr, nil
}

// newFakeLoader 构造挂接 fake 原语、使用冻结 ABI 组合标识的加载器。
func newFakeLoader(fake *fakePrimitives) *Loader {
	return newLoader(fake, abiProfile)
}

// TestOpenRejectsInvalidPathBeforeNativeLoad 验证:非法路径在进入
// 原生加载之前即被拒绝,平台原语零调用;错误可经 errors.As 识别为
// *LibraryLoadError、经 errors.Is 识别为 ErrInvalidInput,诊断
// 保留规范化尝试路径、库基名与运行时探测到的目标平台。
func TestOpenRejectsInvalidPathBeforeNativeLoad(t *testing.T) {
	nul := string([]byte{0})
	invalidUTF8 := "/tmp/" + string([]byte{0xff, 0xfe}) + ".so"

	cases := []struct {
		name     string
		raw      string
		wantPath string // 规范化尝试路径
		wantName string // LibraryName
	}{
		{"空路径", "", "", ""},
		{"当前目录相对路径", "./x.so", "x.so", "x.so"},
		{"上级目录相对路径", "../up/x.so", "../up/x.so", "x.so"},
		{"普通相对路径", "lib/libMaaCore.so", "lib/libMaaCore.so", "libMaaCore.so"},
		{"绝对路径内嵌 NUL", "/tmp/libMaa" + nul + "Core.so", "/tmp/libMaa" + nul + "Core.so", "libMaa" + nul + "Core.so"},
		{"非法 UTF-8", invalidUTF8, invalidUTF8, string([]byte{0xff, 0xfe}) + ".so"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePrimitives{}
			lib, err := newFakeLoader(fake).Open(tc.raw)
			if lib != nil {
				t.Fatalf("非法路径 %q 不得返回 Library, got %+v", tc.raw, lib)
			}
			if err == nil {
				t.Fatalf("非法路径 %q 必须返回错误", tc.raw)
			}
			// 拒绝必须发生在进入原生加载之前。
			if len(fake.openPaths) != 0 {
				t.Errorf("open 原语被调用 %d 次, want 0 (calls: %v)", len(fake.openPaths), fake.openPaths)
			}
			var lle *LibraryLoadError
			if !errors.As(err, &lle) {
				t.Fatalf("errors.As 未得到 *LibraryLoadError, got %T: %v", err, err)
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false, want true; err = %v", err)
			}
			wantPath := tc.wantPath
			if wantPath != "" {
				wantPath = filepath.Clean(wantPath)
			}
			if lle.Path != wantPath {
				t.Errorf("Path = %q, want %q", lle.Path, wantPath)
			}
			if lle.LibraryName != tc.wantName {
				t.Errorf("LibraryName = %q, want %q", lle.LibraryName, tc.wantName)
			}
			if lle.GOOS != runtime.GOOS || lle.GOARCH != runtime.GOARCH {
				t.Errorf("GOOS/GOARCH = %s/%s, want %s/%s", lle.GOOS, lle.GOARCH, runtime.GOOS, runtime.GOARCH)
			}
			if lle.Err == nil {
				t.Error("Err 必须非 nil")
			}
			// 错误文本应包含路径与目标平台,便于就地诊断。
			text := err.Error()
			if wantPath != "" && !strings.Contains(text, wantPath) {
				t.Errorf("Error() = %q, want it to contain path %q", text, wantPath)
			}
			if want := runtime.GOOS + "/" + runtime.GOARCH; !strings.Contains(text, want) {
				t.Errorf("Error() = %q, want it to contain target platform %q", text, want)
			}
		})
	}
}

// TestOpenNormalizesAbsolutePath 验证:合法绝对路径按 filepath.Clean
// 规范化后恰好以规范化形式传给平台原语,Library 的 Path/Name 与之一致。
func TestOpenNormalizesAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	sep := string(filepath.Separator)
	raw := dir + sep + sep + "a" + sep + "." + sep + "b" + sep + ".." + sep + "libMaaCore.so"
	normalized := filepath.Join(dir, "a", "libMaaCore.so")
	fake := &fakePrimitives{openHandle: 0x1000}
	lib, err := newFakeLoader(fake).Open(raw)
	if err != nil {
		t.Fatalf("Open(%q) 返回意外错误: %v", raw, err)
	}
	if len(fake.openPaths) != 1 || fake.openPaths[0] != normalized {
		t.Errorf("原语收到的路径 = %v, want 恰好 [%s]", fake.openPaths, normalized)
	}
	if lib.Path() != normalized {
		t.Errorf("Library.Path() = %q, want %q", lib.Path(), normalized)
	}
	if lib.Name() != "libMaaCore.so" {
		t.Errorf("Library.Name() = %q, want %q", lib.Name(), "libMaaCore.so")
	}
}

// TestOpenPrimitiveFailureKeepsUnderlyingCause 验证:open 原语失败时,
// 底层错误原样进入 *LibraryLoadError 的错误链,可沿 errors.Is
// 追溯,且不与输入错误关联。
func TestOpenPrimitiveFailureKeepsUnderlyingCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	sentinel := errors.New("fake dlopen: dependency library missing")
	fake := &fakePrimitives{openErr: sentinel}

	lib, err := newFakeLoader(fake).Open(path)
	if lib != nil {
		t.Fatalf("原语失败不得返回 Library, got %+v", lib)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As 未得到 *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Path != path {
		t.Errorf("Path = %q, want %q", lle.Path, path)
	}
	if lle.LibraryName != "libMaaCore.so" {
		t.Errorf("LibraryName = %q, want %q", lle.LibraryName, "libMaaCore.so")
	}
	if lle.GOOS != runtime.GOOS || lle.GOARCH != runtime.GOARCH {
		t.Errorf("GOOS/GOARCH = %s/%s, want %s/%s", lle.GOOS, lle.GOARCH, runtime.GOOS, runtime.GOARCH)
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is 未能沿错误链到达底层 sentinel")
	}
	if errors.Is(err, ErrInvalidInput) {
		t.Error("底层加载失败不得与 ErrInvalidInput 关联")
	}
}

// TestOpenRejectsNullHandle 验证:open 原语返回 (0, nil) 的契约破坏
// 情形被转换为 *LibraryLoadError,不返回 Library,原因不与输入
// 错误关联。
func TestOpenRejectsNullHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	fake := &fakePrimitives{zeroHandle: true}

	lib, err := newFakeLoader(fake).Open(path)
	if lib != nil {
		t.Fatalf("契约破坏情形不得返回 Library, got %+v", lib)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As 未得到 *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Path != path || lle.LibraryName != "libMaaCore.so" {
		t.Errorf("诊断字段不符: Path=%q LibraryName=%q", lle.Path, lle.LibraryName)
	}
	if lle.Err == nil {
		t.Error("Err 必须非 nil")
	}
	if errors.Is(err, ErrInvalidInput) {
		t.Error("零句柄是系统 loader 契约破坏,不得与 ErrInvalidInput 关联")
	}
}

// TestLookupSuccess 验证:查找成功返回原语给出的地址,且查找恰好
// 使用打开阶段返回的句柄。
func TestLookupSuccess(t *testing.T) {
	const (
		handle = uintptr(0x2000)
		addr   = uintptr(0xabcdef)
	)
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	fake := &fakePrimitives{openHandle: handle, lookupAddr: addr}
	lib, err := newFakeLoader(fake).Open(path)
	if err != nil {
		t.Fatalf("Open(%q) 返回意外错误: %v", path, err)
	}

	got, err := lib.Lookup("AsstCreateEx")
	if err != nil {
		t.Fatalf("Lookup 返回意外错误: %v", err)
	}
	if got != addr {
		t.Errorf("Lookup 地址 = %#x, want %#x", got, addr)
	}
	if len(fake.lookupNames) != 1 || fake.lookupNames[0] != "AsstCreateEx" {
		t.Errorf("lookup 收到的符号名 = %v, want [AsstCreateEx]", fake.lookupNames)
	}
	if len(fake.lookupHandles) != 1 || fake.lookupHandles[0] != handle {
		t.Errorf("lookup 收到的句柄 = %v, want [%#x]", fake.lookupHandles, handle)
	}
}

// TestLookupFailureKeepsUnderlyingCause 验证:查找失败返回
// *SymbolLookupError,逐字段保留库路径、符号名与 ABI 组合标识,
// 底层原因可沿 errors.Is 追溯,且不返回可用地址。
func TestLookupFailureKeepsUnderlyingCause(t *testing.T) {
	const symbol = "AsstNoSuchSymbol"
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	sentinel := errors.New("fake dlsym: undefined symbol")
	fake := &fakePrimitives{openHandle: 0x2000, lookupErr: sentinel}
	lib, err := newFakeLoader(fake).Open(path)
	if err != nil {
		t.Fatalf("Open(%q) 返回意外错误: %v", path, err)
	}

	addr, err := lib.Lookup(symbol)
	if addr != 0 {
		t.Errorf("失败时地址 = %#x, want 0", addr)
	}
	var sle *SymbolLookupError
	if !errors.As(err, &sle) {
		t.Fatalf("errors.As 未得到 *SymbolLookupError, got %T: %v", err, err)
	}
	if sle.LibraryPath != path {
		t.Errorf("LibraryPath = %q, want %q", sle.LibraryPath, path)
	}
	if sle.Symbol != symbol {
		t.Errorf("Symbol = %q, want %q", sle.Symbol, symbol)
	}
	if sle.Profile != abiProfile {
		t.Errorf("Profile = %q, want %q", sle.Profile, abiProfile)
	}
	if !errors.Is(err, sentinel) {
		t.Error("errors.Is 未能沿错误链到达底层 sentinel")
	}
}

// TestLookupNullAddressNotCallable 验证:原语返回 (0, nil) 时,零地址
// 不会被当作可调用的函数地址返回,而是得到可识别空地址原因的
// *SymbolLookupError。
func TestLookupNullAddressNotCallable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	fake := &fakePrimitives{openHandle: 0x2000, nullAddr: true}
	lib, err := newFakeLoader(fake).Open(path)
	if err != nil {
		t.Fatalf("Open(%q) 返回意外错误: %v", path, err)
	}

	addr, err := lib.Lookup("AsstCreateEx")
	if err == nil {
		t.Fatal("零地址不得作为成功结果返回 (0, nil)")
	}
	if addr != 0 {
		t.Errorf("失败时地址 = %#x, want 0", addr)
	}
	var sle *SymbolLookupError
	if !errors.As(err, &sle) {
		t.Fatalf("errors.As 未得到 *SymbolLookupError, got %T: %v", err, err)
	}
	if !errors.Is(err, errSymbolNullAddress) {
		t.Error("errors.Is 未能识别空地址原因")
	}
	if sle.Symbol != "AsstCreateEx" || sle.LibraryPath != path || sle.Profile != abiProfile {
		t.Errorf("诊断字段不符: %+v", sle)
	}
}
