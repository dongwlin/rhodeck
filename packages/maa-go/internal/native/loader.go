// 本文件实现 MaaCore 动态库加载器:纯 Go 的路径校验与规范化、
// 可注入的平台打开/查符号原语边界,以及加载与查找失败到结构化
// 错误的转换。本文件平台中立,不含任何平台条件;平台原语的实现
// 见各平台文件(如 loader_linux.go)。
//
// 冻结的加载策略,实现与调用方都不得自行扩展:
//
//   - 只接受调用方显式给出的绝对路径:不推导搜索路径、不扫描附加
//     目录、不修改 LD_LIBRARY_PATH、不预加载任何依赖;
//   - 以 RTLD_NOW|RTLD_LOCAL 打开:全部重定位在加载期完成,缺符号
//     与依赖缺失等链接失败在打开阶段即暴露;库自身符号不进入进程
//     全局符号名字空间,不使用 RTLD_GLOBAL;
//   - 依赖解析完全交给系统 loader:随包目录内的 $ORIGIN RUNPATH
//     与系统 loader 默认搜索范围已覆盖锁定发行包的全部依赖,本层
//     不做任何额外干预;
//   - 库一经加载保持到进程退出,本层不提供 Close/Dlclose,首期不
//     卸载原生库;
//   - dlopen 会执行库内的原生构造器并映射全部依赖;RTLD_LOCAL 只
//     限制符号可见性,不是进程级隔离。原生加载开始之后发生的失败
//     使本进程终止使用,由上层 worker 退出恢复;本层不承诺撤回
//     已经发生的原生副作用。

package native

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// abiProfile 是当前锁定并经证据验证的 ABI 兼容组合标识,随每次
// 符号查找错误一并报告。它只描述本次选定的组合,不声称升级到
// 更新版本后必然兼容。
const abiProfile = "maa-v6.20.1"

// errNullHandle 表示平台原语打开库后返回零句柄且未报告错误:正常
// 失败必须携带错误,该情形意味着系统 loader 的契约已被破坏。它是
// 普通诊断错误,与输入错误(ErrInvalidInput)无关。
var errNullHandle = errors.New("native loader returned a null handle without an error")

// errSymbolNullAddress 表示符号查找在原语层面成功、但解析到的地址
// 为 0。零地址不是可调用的函数地址,与查找失败同样处理。
var errSymbolNullAddress = errors.New("symbol resolved to a null address")

// loaderPrimitives 是平台打开与查符号原语的注入边界。加载器只经
// 该接口触碰系统 loader;测试注入可记录调用的假原语构造失败路径,
// 不替换生产包的全局函数变量,避免并行测试相互污染。
type loaderPrimitives interface {
	// open 打开 path 指向的动态库,返回句柄;失败返回非 nil 错误。
	// 返回零句柄且无错误属于契约破坏,由 Loader 统一转换为错误。
	open(path string) (uintptr, error)

	// lookup 在 handle 指向的已加载库中查找符号 name,返回其地址;
	// 找不到时返回非 nil 错误。返回零地址且无错误同样属于契约破坏。
	lookup(handle uintptr, name string) (uintptr, error)
}

// Loader 按显式绝对路径打开动态库并提供符号查找。它持有平台原语
// 与本次选定的 ABI 组合标识,每个实例可独立注入原语,便于隔离测试。
// 零值不可用,必须经 newLoader 构造。
type Loader struct {
	prims   loaderPrimitives
	profile string
}

// newLoader 以给定的平台原语与 ABI 组合标识构造加载器。
func newLoader(prims loaderPrimitives, profile string) *Loader {
	return &Loader{prims: prims, profile: profile}
}

// Open 校验并规范化 path 后,经平台原语打开动态库。
//
// 校验在任何原语调用之前完成:空路径、非法 UTF-8、内嵌 NUL 与非
// 绝对路径一律拒绝;文件是否存在不在此检查,允许留到加载阶段失败。
// 校验失败返回 *LibraryLoadError,其错误链可经 errors.Is 识别
// ErrInvalidInput,诊断保留规范化尝试路径与运行时探测到的目标
// OS/架构;此类失败不进入原生加载,不产生任何进程级副作用。
//
// 校验通过后调用 open 原语。原语失败时,底层错误原样保留在
// *LibraryLoadError 的错误链上(不截断、不改写),依赖库缺失等
// 系统级原因可沿 errors.Is/errors.As 追溯。原语返回零句柄且无错误
// 属于系统 loader 契约被破坏,同样返回 *LibraryLoadError,但其
// 原因是普通诊断错误,不与输入错误关联。
//
// 成功返回的 *Library 在进程内保持加载,没有对应的关闭方法。
func (ldr *Loader) Open(path string) (*Library, error) {
	normalized, verr := validateLibraryPath(path)
	if verr != nil {
		return nil, libraryLoadError(normalized, verr)
	}
	handle, err := ldr.prims.open(normalized)
	if err != nil {
		return nil, libraryLoadError(normalized, err)
	}
	if handle == 0 {
		return nil, libraryLoadError(normalized, errNullHandle)
	}
	return &Library{ldr: ldr, path: normalized, handle: handle}, nil
}

// validateLibraryPath 纯 Go 校验并规范化库路径:拒绝空路径、非法
// UTF-8、内嵌 NUL 与非绝对路径,规范化为 filepath.Clean,空路径的
// 规范化结果记为空串。返回值始终是规范化后的尝试路径,供错误诊断
// 引用;校验失败的原因以 fmt.Errorf 包装 ErrInvalidInput 后返回。
func validateLibraryPath(path string) (string, error) {
	normalized := ""
	if path != "" {
		normalized = filepath.Clean(path)
	}
	switch {
	case path == "":
		return normalized, fmt.Errorf("library path is empty: %w", ErrInvalidInput)
	case !utf8.ValidString(path):
		return normalized, fmt.Errorf("library path is not valid UTF-8: %w", ErrInvalidInput)
	case strings.IndexByte(path, '\x00') >= 0:
		return normalized, fmt.Errorf("library path contains an embedded NUL byte: %w", ErrInvalidInput)
	case !filepath.IsAbs(normalized):
		return normalized, fmt.Errorf("library path %q is not absolute: %w", normalized, ErrInvalidInput)
	}
	return normalized, nil
}

// libraryLoadError 汇集规范化路径、运行时探测到的目标平台与底层
// 原因,构造加载失败错误。
func libraryLoadError(path string, cause error) *LibraryLoadError {
	return &LibraryLoadError{
		LibraryName: libraryBaseName(path),
		Path:        path,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Err:         cause,
	}
}

// libraryBaseName 返回路径的基名作为库名;空路径的基名记为空串,
// 不回退为 "."。
func libraryBaseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

// Library 表示一次成功打开的原生动态库。它持有构造它的加载器引用、
// 规范化路径与系统 loader 句柄;库保持加载到进程退出,本类型不提供
// Close 或任何卸载语义。
type Library struct {
	ldr    *Loader
	path   string
	handle uintptr
}

// Path 返回打开时使用的规范化路径。
func (lib *Library) Path() string { return lib.path }

// Name 返回库的基名,即规范化路径的最后一个元素。
func (lib *Library) Name() string { return libraryBaseName(lib.path) }

// Lookup 在已加载的库中查找符号 name,返回其地址。
//
// 查找失败与「原语成功但地址为 0」都返回 *SymbolLookupError,
// 其中包含库路径、符号名、加载器选定的 ABI 组合标识与底层原因;
// 任何情况下都不返回 (0, nil),零地址不得被当作可调用的函数地址。
func (lib *Library) Lookup(name string) (uintptr, error) {
	addr, err := lib.ldr.prims.lookup(lib.handle, name)
	if err != nil {
		return 0, lib.lookupError(name, err)
	}
	if addr == 0 {
		return 0, lib.lookupError(name, errSymbolNullAddress)
	}
	return addr, nil
}

// lookupError 汇集库路径、符号名与本次选定的 ABI 组合标识,构造
// 符号查找失败错误。
func (lib *Library) lookupError(name string, cause error) *SymbolLookupError {
	return &SymbolLookupError{
		LibraryPath: lib.path,
		Symbol:      name,
		Profile:     lib.ldr.profile,
		Err:         cause,
	}
}
