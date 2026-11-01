package native

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{"empty path", "", "", ""},
		{"current-directory relative path", "./x.so", "x.so", "x.so"},
		{"parent-directory relative path", "../up/x.so", "../up/x.so", "x.so"},
		{"plain relative path", "lib/libMaaCore.so", "lib/libMaaCore.so", "libMaaCore.so"},
		{"NUL embedded in absolute path", "/tmp/libMaa" + nul + "Core.so", "/tmp/libMaa" + nul + "Core.so", "libMaa" + nul + "Core.so"},
		{"invalid UTF-8", invalidUTF8, invalidUTF8, string([]byte{0xff, 0xfe}) + ".so"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePrimitives{}
			lib, err := newFakeLoader(fake).Open(tc.raw)
			require.Nil(t, lib, "invalid path %q must not return a Library, got %+v", tc.raw, lib)
			require.Error(t, err, "invalid path %q must return an error", tc.raw)
			// 拒绝必须发生在进入原生加载之前。
			assert.Empty(t, fake.openPaths, "the open primitive was called %d times, want 0 (calls: %v)", len(fake.openPaths), fake.openPaths)
			var lle *LibraryLoadError
			require.ErrorAs(t, err, &lle, "errors.As did not yield a *LibraryLoadError, got %T: %v", err, err)
			assert.ErrorIs(t, err, ErrInvalidInput, "errors.Is(err, ErrInvalidInput) = false, want true; err = %v", err)
			wantPath := tc.wantPath
			if wantPath != "" {
				wantPath = filepath.Clean(wantPath)
			}
			assert.Equal(t, wantPath, lle.Path, "Path = %q, want %q", lle.Path, wantPath)
			assert.Equal(t, tc.wantName, lle.LibraryName, "LibraryName = %q, want %q", lle.LibraryName, tc.wantName)
			assert.Equal(t, runtime.GOOS, lle.GOOS, "GOOS = %s, want %s", lle.GOOS, runtime.GOOS)
			assert.Equal(t, runtime.GOARCH, lle.GOARCH, "GOARCH = %s, want %s", lle.GOARCH, runtime.GOARCH)
			assert.Error(t, lle.Err, "Err must be non-nil")
			// 错误文本应包含路径与目标平台,便于就地诊断。
			text := err.Error()
			if wantPath != "" {
				assert.Contains(t, text, wantPath, "Error() = %q, want it to contain path %q", text, wantPath)
			}
			assert.Contains(t, text, runtime.GOOS+"/"+runtime.GOARCH, "Error() = %q, want it to contain target platform %q", text, runtime.GOOS+"/"+runtime.GOARCH)
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
	require.NoError(t, err, "Open(%q) returned an unexpected error", raw)
	assert.Equal(t, []string{normalized}, fake.openPaths, "path received by the primitive = %v, want exactly [%s]", fake.openPaths, normalized)
	assert.Equal(t, normalized, lib.Path(), "Library.Path() = %q, want %q", lib.Path(), normalized)
	assert.Equal(t, "libMaaCore.so", lib.Name(), "Library.Name() = %q, want %q", lib.Name(), "libMaaCore.so")
}

// TestOpenPrimitiveFailureKeepsUnderlyingCause 验证:open 原语失败时,
// 底层错误原样进入 *LibraryLoadError 的错误链,可沿 errors.Is
// 追溯,且不与输入错误关联。
func TestOpenPrimitiveFailureKeepsUnderlyingCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	sentinel := errors.New("fake dlopen: dependency library missing")
	fake := &fakePrimitives{openErr: sentinel}

	lib, err := newFakeLoader(fake).Open(path)
	require.Nil(t, lib, "a primitive failure must not return a Library, got %+v", lib)
	var lle *LibraryLoadError
	require.ErrorAs(t, err, &lle, "errors.As did not yield a *LibraryLoadError, got %T: %v", err, err)
	assert.Equal(t, path, lle.Path, "Path = %q, want %q", lle.Path, path)
	assert.Equal(t, "libMaaCore.so", lle.LibraryName, "LibraryName = %q, want %q", lle.LibraryName, "libMaaCore.so")
	assert.Equal(t, runtime.GOOS, lle.GOOS, "GOOS = %s, want %s", lle.GOOS, runtime.GOOS)
	assert.Equal(t, runtime.GOARCH, lle.GOARCH, "GOARCH = %s, want %s", lle.GOARCH, runtime.GOARCH)
	assert.ErrorIs(t, err, sentinel, "errors.Is did not reach the underlying sentinel along the error chain")
	assert.NotErrorIs(t, err, ErrInvalidInput, "an underlying load failure must not be associated with ErrInvalidInput")
}

// TestOpenRejectsNullHandle 验证:open 原语返回 (0, nil) 的契约破坏
// 情形被转换为 *LibraryLoadError,不返回 Library,原因不与输入
// 错误关联。
func TestOpenRejectsNullHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	fake := &fakePrimitives{zeroHandle: true}

	lib, err := newFakeLoader(fake).Open(path)
	require.Nil(t, lib, "a contract-violating case must not return a Library, got %+v", lib)
	var lle *LibraryLoadError
	require.ErrorAs(t, err, &lle, "errors.As did not yield a *LibraryLoadError, got %T: %v", err, err)
	assert.Equal(t, path, lle.Path, "diagnostic fields mismatch: Path=%q LibraryName=%q", lle.Path, lle.LibraryName)
	assert.Equal(t, "libMaaCore.so", lle.LibraryName, "diagnostic fields mismatch: Path=%q LibraryName=%q", lle.Path, lle.LibraryName)
	assert.Error(t, lle.Err, "Err must be non-nil")
	assert.NotErrorIs(t, err, ErrInvalidInput, "a null handle is a system loader contract violation and must not be associated with ErrInvalidInput")
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
	require.NoError(t, err, "Open(%q) returned an unexpected error", path)

	got, err := lib.Lookup("AsstCreateEx")
	require.NoError(t, err, "Lookup returned an unexpected error")
	assert.Equal(t, addr, got, "Lookup address = %#x, want %#x", got, addr)
	assert.Equal(t, []string{"AsstCreateEx"}, fake.lookupNames, "symbol names received by lookup = %v, want [AsstCreateEx]", fake.lookupNames)
	assert.Equal(t, []uintptr{handle}, fake.lookupHandles, "handles received by lookup = %v, want [%#x]", fake.lookupHandles, handle)
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
	require.NoError(t, err, "Open(%q) returned an unexpected error", path)

	addr, err := lib.Lookup(symbol)
	assert.Zero(t, addr, "address on failure = %#x, want 0", addr)
	var sle *SymbolLookupError
	require.ErrorAs(t, err, &sle, "errors.As did not yield a *SymbolLookupError, got %T: %v", err, err)
	assert.Equal(t, path, sle.LibraryPath, "LibraryPath = %q, want %q", sle.LibraryPath, path)
	assert.Equal(t, symbol, sle.Symbol, "Symbol = %q, want %q", sle.Symbol, symbol)
	assert.Equal(t, abiProfile, sle.Profile, "Profile = %q, want %q", sle.Profile, abiProfile)
	assert.ErrorIs(t, err, sentinel, "errors.Is did not reach the underlying sentinel along the error chain")
}

// TestLookupNullAddressNotCallable 验证:原语返回 (0, nil) 时,零地址
// 不会被当作可调用的函数地址返回,而是得到可识别空地址原因的
// *SymbolLookupError。
func TestLookupNullAddressNotCallable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	fake := &fakePrimitives{openHandle: 0x2000, nullAddr: true}
	lib, err := newFakeLoader(fake).Open(path)
	require.NoError(t, err, "Open(%q) returned an unexpected error", path)

	addr, err := lib.Lookup("AsstCreateEx")
	require.Error(t, err, "a null address must not be returned as a successful (0, nil) result")
	assert.Zero(t, addr, "address on failure = %#x, want 0", addr)
	var sle *SymbolLookupError
	require.ErrorAs(t, err, &sle, "errors.As did not yield a *SymbolLookupError, got %T: %v", err, err)
	assert.ErrorIs(t, err, errSymbolNullAddress, "errors.Is did not identify the null-address cause")
	assert.Equal(t, "AsstCreateEx", sle.Symbol, "diagnostic fields mismatch: %+v", sle)
	assert.Equal(t, path, sle.LibraryPath, "diagnostic fields mismatch: %+v", sle)
	assert.Equal(t, abiProfile, sle.Profile, "diagnostic fields mismatch: %+v", sle)
}
