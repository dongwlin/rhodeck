// 本文件实现加载事务的「解析→注册→发布」核心:按符号清单从已打开的
// 动态库解析适用符号,必需项全部就绪后经注册原语逐项绑定到本次事务自己
// 的临时 API 表,全部成功才一次性发布只读的 APITable 与 Capabilities。
//
// 失败原子性的边界:失败时不发布任何半初始化 API 表——必需符号缺失时
// 整体报告且注册器零调用,注册 panic 时丢弃整张临时表;但不承诺撤回
// loader 已经发生的原生副作用(dlopen 执行的库内构造器、依赖映射与系统
// loader 状态不可逆)。API 表与 capability 发布后只读:字段全部未导出,
// Instance 只经 Runtime 持有该表,不向 worker 导出 raw 函数或函数表;
// 零值 APITable 不可用,只能由加载事务构造。

package native

import (
	"fmt"
	"reflect"
	"runtime"

	"github.com/ebitengine/purego"
)

// symbolRegistrar 是注册原语的注入边界:把已解析的原生地址绑定到
// 目标函数字段。实现不得自行 recover panic——注册边界的 panic 由
// 注册事务统一捕获并转换为结构化错误。
type symbolRegistrar interface {
	registerFunc(target any, addr uintptr)
}

// puregoRegistrar 是生产注册原语:直接以 purego.RegisterFunc 把已
// 解析地址绑定到目标字段。purego.RegisterFunc 的失败(非函数指针、
// 多返回值、零地址、不支持的参数类型等)一律以 panic 表达、不返回
// 错误,因此本类型不做任何错误转换,统一交给注册边界处理。
type puregoRegistrar struct{}

func (puregoRegistrar) registerFunc(target any, addr uintptr) {
	purego.RegisterFunc(target, addr)
}

// Capabilities 是一次成功加载后可选符号能力的只读快照:字段全部
// 未导出,外部只能经方法读取布尔结果,不能改写快照内容。快照是
// 可复制的值类型,副本与原件互不影响。Create 对应 AsstCreate(保留
// 覆盖;公共层首期统一使用 CreateEx,该能力暂无公开入口,仅如实
// 反映符号是否在场);ImageBgr 对应 AsstGetImageBgr;AsyncAttachWindow
// 对应 AsstAsyncAttachWindow,仅 Windows 参与查找,非 Windows 平台
// 恒为 false;MapLevelKey 与 ItemName 对应 AsstCallerExtra.h 的两个
// ASST_WITH_EXTRA_CALLERS 条件实验导出,上游发行包仅在 Apple 构建
// 导出,缺席平台恒为 false。
type Capabilities struct {
	create            bool
	imageBgr          bool
	asyncAttachWindow bool
	mapLevelKey       bool
	itemName          bool
}

// Create 报告 AsstCreate 是否在场。
func (c Capabilities) Create() bool { return c.create }

// ImageBgr 报告 AsstGetImageBgr 是否在场。
func (c Capabilities) ImageBgr() bool { return c.imageBgr }

// AsyncAttachWindow 报告 AsstAsyncAttachWindow 是否在场;非 Windows
// 平台恒为 false。
func (c Capabilities) AsyncAttachWindow() bool { return c.asyncAttachWindow }

// MapLevelKey 报告 AsstGetMapLevelKey 是否在场;实验导出未随本次
// 加载的原生库导出时为 false。
func (c Capabilities) MapLevelKey() bool { return c.mapLevelKey }

// ItemName 报告 AsstGetItemName 是否在场;实验导出未随本次加载的
// 原生库导出时为 false。
func (c Capabilities) ItemName() bool { return c.itemName }

// CapabilitiesForTest 以显式组合构造能力快照,仅供上层测试注入能力
// 门两侧(生产快照只能由加载事务构造,字段未导出);不在生产链路
// 使用,存在即测试接缝的一部分。
func CapabilitiesForTest(create, imageBgr, asyncAttachWindow, mapLevelKey, itemName bool) Capabilities {
	return Capabilities{
		create:            create,
		imageBgr:          imageBgr,
		asyncAttachWindow: asyncAttachWindow,
		mapLevelKey:       mapLevelKey,
		itemName:          itemName,
	}
}

// APITable 是加载事务成功后只读发布的 raw API 表句柄:api 指向本次
// 事务构造并注册完成的临时表,caps 是同一次事务的能力快照。所有
// 字段未导出;Instance 只经 Runtime 持有该表,不向 worker 导出 raw
// 函数或函数表,也不采用可被外部复制的包级全局函数变量。零值不可
// 用,只能由加载事务构造。
type APITable struct {
	api  *api
	caps Capabilities
}

// funcs 返回 raw API 表(供本包内及后续 raw 调用包装使用)。
func (t *APITable) funcs() *api { return t.api }

// Capabilities 返回本次加载的能力快照副本;快照本身不可修改。
func (t *APITable) Capabilities() Capabilities { return t.caps }

// ResolveAndRegister 是加载事务的生产入口:从已打开的 lib 中解析
// 适用符号并注册到临时 API 表,全部成功后一次性发布只读结果。
func ResolveAndRegister(lib *Library) (*APITable, error) {
	return resolveAndRegister(lib, puregoRegistrar{})
}

// resolveAndRegister 按固定的「解析→注册→发布」顺序执行,任一阶段
// 失败都不发布结果。
//
// 每次调用构造自己的临时表 table:= new(api) 与零值能力快照,specs
// 取自 symbolSpecs(table):事务之间不共享任何状态,同一次失败不会
// 污染另一次加载,在同一 lib 上重复发起事务是合法的。
//
// 解析阶段只查找、不注册。遍历 specs,对适用项(platform 为
// allPlatforms,或 windowsOnly 且 runtime.GOOS == "windows")调用
// lib.Lookup:
//   - 失败且 required:把返回的 *SymbolLookupError 原样追加进失败
//     集合(不包装、不改写),继续解析以一次收集全部必需项失败,
//     避免调用方逐次试错;
//   - 失败且 optional:不进失败集合,对应 capability 保持关闭、
//     字段保持 nil,可选项缺失不阻塞其余符号;
//   - 成功:记录地址;optional 项以对 spec.Name 的 switch 显式置位
//     对应 capability(AsstCreate→create、AsstGetImageBgr→imageBgr、
//     AsstAsyncAttachWindow→asyncAttachWindow、AsstGetMapLevelKey→
//     mapLevelKey、AsstGetItemName→itemName),不做反射推断;
//   - 非适用项完全跳过:不查找、不注册、不影响能力。
//
// 解析阶段结束仍有失败则立即返回 (nil, AggregateError(失败集合)):
// 只要存在未满足的必需项就不进入注册,注册器调用次数必为零。
//
// 注册阶段再次遍历 specs,对「适用且已解析出地址」的项逐项注册
// (可选项已解析的同样注册:注册与能力快照相互独立,符号在场即可
// 绑定)。每项注册置于带 defer/recover 的独立边界内:recover 到
// panic 时按 errors.go 的构造契约把 panic 值转换为结构化的
// *FunctionRegistrationError(携带 Symbol 与 Signature)。首个注册
// panic 即中止整个事务并返回该错误,不聚合并继续:注册 panic 意味
// 着声明与注册器的约定被破坏,区别于来自库环境的缺符号(后者应整体
// 收集),继续注册无法假设注册器行为;临时表整体丢弃,已写入的字段
// 不外泄。
//
// 全部注册成功才构造并发布结果;API 表与能力快照发布后只读。
func resolveAndRegister(lib *Library, reg symbolRegistrar) (*APITable, error) {
	table := new(api)
	caps := Capabilities{}
	specs := symbolSpecs(table)

	// addrs/resolved 与 specs 同序:解析阶段记录结果,注册阶段只对
	// 已解析出的项行动,两阶段之间不重复查找。
	addrs := make([]uintptr, len(specs))
	resolved := make([]bool, len(specs))

	var failures []error
	for i, spec := range specs {
		if !specApplicable(spec) {
			continue
		}
		addr, err := lib.Lookup(spec.Name)
		if err != nil {
			if spec.Policy == required {
				failures = append(failures, err)
			}
			continue
		}
		addrs[i] = addr
		resolved[i] = true
		if spec.Policy == optional {
			// 置位集合由 symbols_static_test.go 的
			// TestCapabilitySwitchMatchesOptionalSpecs 与 optional
			// 条目交叉钉死;新增 optional 符号必须同步本 switch 与
			// 该测试的能力字段映射,漏改即静态测试失败。
			switch spec.Name {
			case "AsstCreate":
				caps.create = true
			case "AsstGetImageBgr":
				caps.imageBgr = true
			case "AsstAsyncAttachWindow":
				caps.asyncAttachWindow = true
			case "AsstGetMapLevelKey":
				caps.mapLevelKey = true
			case "AsstGetItemName":
				caps.itemName = true
			}
		}
	}
	if len(failures) > 0 {
		return nil, AggregateError(failures)
	}

	for i, spec := range specs {
		if !specApplicable(spec) || !resolved[i] {
			continue
		}
		if err := registerOne(reg, spec, addrs[i]); err != nil {
			return nil, err
		}
	}

	return &APITable{api: table, caps: caps}, nil
}

// specApplicable 报告符号在当前运行平台是否适用:allPlatforms 恒
// 适用;windowsOnly 仅在 runtime.GOOS == "windows" 时适用。非适用
// 项不查找、不注册、不影响能力。
func specApplicable(spec symbolSpec) bool {
	return spec.Platform == allPlatforms ||
		(spec.Platform == windowsOnly && runtime.GOOS == "windows")
}

// registerOne 注册单个已解析符号,并以其自身栈帧为注册边界:defer/
// recover 只覆盖这一项的注册调用,panic 不会泄漏到事务循环之外。
//
// Signature 经 reflect 取目标字段的函数类型 String()(如
// "func(int32, *uint8) uint8";reflect 不保留参数名,并把 byte 别名
// 渲染为底层类型 uint8)。reflect 只用于把已冻结声明的类型渲染成
// 诊断文本,不做任何 ABI 推断或动态注册决策;具名参数形式由
// symbols_static_test.go 静态钉死。
func registerOne(reg symbolRegistrar, spec symbolSpec, addr uintptr) (err error) {
	signature := reflect.TypeOf(spec.Target).Elem().String()
	defer func() {
		if r := recover(); r != nil {
			err = registrationPanicError(spec.Name, signature, r)
		}
	}()
	reg.registerFunc(spec.Target, addr)
	return nil
}

// registrationPanicError 把注册边界 recover 到的 panic 值按 errors.go
// 的构造契约转换为 *FunctionRegistrationError:panic 值本身是 error
// 时直接作为 Err(不再包一层,错误链上的 errors.Is / errors.As 仍能
// 识别原始原因);非 error 值转换为诊断错误后放入本字段。
func registrationPanicError(symbol, signature string, recovered any) error {
	var cause error
	if e, ok := recovered.(error); ok {
		cause = e
	} else {
		cause = fmt.Errorf("panic recovered during registration: %v", recovered)
	}
	return &FunctionRegistrationError{
		Symbol:    symbol,
		Signature: signature,
		Err:       cause,
	}
}
