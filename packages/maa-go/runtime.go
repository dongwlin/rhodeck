// 本文件实现 Runtime 的配置契约与单进程初始化事务(Open)。执行顺序:
// 纯 Go 配置校验与深拷贝快照 → 原子预占单 Runtime 生命周期 → 装载临时
// API 表 → 复制 Version/核验 NullSize → 设置用户目录 → 静态选项 →
// 资源根 → 连接 extras → 发布 ready Runtime。
//
// 生命周期契约:
//   - 每进程单 Runtime:进程级生命周期门串行化全部 Open;已有预占或
//     活动 Runtime 时,并发或重复 Open 返回 ErrRuntimeInUse,不复用已有
//     对象、也不忽略新配置。
//   - 预占即不可回头:预占之后的任何失败(库加载、符号注册、初始化
//     步骤)都使门进入 terminated,后续 Open 一律返回
//     ErrRuntimeTerminated;不以 Dlclose 或热重载恢复,恢复只能来自退出
//     并启动新 worker 进程。
//   - 配置校验发生在触碰生命周期门之前:纯 Go 校验失败不消耗进程生命
//     周期,修正输入后仍可进行首次原生加载。
//   - 配置快照所有权:校验通过后、预占之前,配置被深拷贝冻结;Open
//     返回后修改原配置不影响已初始化 Runtime。调用者在 Open 返回前不得
//     并发修改输入。
//
// 实例创建在同一状态门内预留唯一实例名额并登记创建 lease:每 Runtime
// 至多一个实例,名额预占后的创建失败或实例关闭都使 Runtime 进入终止
// 使用状态,不允许同进程重建实例或重试资源组合。
//
// Runtime.Close 在同一状态门内先关闭创建准入,再等待已登记创建
// lease——未发布实例的统一回滚由创建者完成,Close 不重复 Destroy——然后
// 把已发布活动实例交给唯一关闭者(与 Instance.Close 同一单次关闭流程),
// 最后终止进程级使用状态。

package maa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"sync"
	"unicode/utf8"
	"unsafe"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// StaticOption 是一条进程级静态选项:key 为 int32(StaticOptionKey
// 语义,锁定值见:Invalid=0、CpuOCR=1、GpuOCR=2;key
// 为 0 即 Invalid,在纯 Go 校验阶段拒绝并返回 ErrInvalidInput;
// 未识别的非零 key 原样透传,不把业务选项封闭为枚举),value 允许
// 空串。key 不得重复。
type StaticOption struct {
	Key   int32
	Value string
}

// ConnectionExtra 是一条按 name 唯一的连接扩展配置:JSON 必须是
// 完整、非空白的 JSON object 原文,原样透传给原生 SetConnectionExtras,
// 不重编码、不经 map[string]any 往返;字符串内的 \u0000 转义合法并
// 原样保留。C 侧为 void 调用,原生不提供成功确认。
type ConnectionExtra struct {
	Name string
	JSON json.RawMessage
}

// BufferLimits 描述一个输出缓冲区的容量探测策略:Initial 是首次
// 分配容量,Max 是探测上限(Initial 不得超过 Max),GrowthRetries 是
// 首次调用之后至多追加的探测次数(允许 0)。单位随使用字段而定
// (字节或元素),见 Limits 字段注释。
type BufferLimits struct {
	Initial       uint64
	Max           uint64
	GrowthRetries uint32
}

// Limits 汇集 Open 与后续实例输入的全部容量限制(冻结;
// 单位、默认值与范围见、)。Limits == nil
// 时使用整套默认值(defaults);非 nil 表示完整显式配置,不逐字段
// 把 0 补成默认值。容量/计数以 uint64、扩容次数以 uint32 表达,
// 在任何转换/乘加/分配之前直接按 uint64 检查范围。
type Limits struct {
	// InputStringBytes 是单个字符串输入(路径、option value、extras
	// name 等)的 UTF-8 字节上限,不含终止 NUL。
	InputStringBytes uint64
	// InputJSONBytes 是单份原始 JSON(extras、任务 params 等)的
	// 字节上限。
	InputJSONBytes uint64
	// ConfigEntries 是每份配置中 option、resource root、extras 的
	// 合计项数上限,不含固定字段与 Limits 自身。
	ConfigEntries uint64
	// ConfigBytes 是每份配置或单次方法输入的全部字符串/JSON 字节
	// 之和上限,包含固定路径、option value、extras name/JSON 等
	// 实际文本。
	ConfigBytes uint64

	// ImagePNG 是 PNG 图像缓冲区策略,容量单位为字节。
	ImagePNG BufferLimits
	// ImageBGR 是 BGR 像素缓冲区策略,容量单位为字节;仅在能力快照
	// 报告 ImageBgr 在场时使用,能力缺席时读取方法直接返回
	// ErrUnsupported,不消费本策略。
	ImageBGR BufferLimits
	// UUID 是设备 UUID 缓冲区策略,容量单位为字节。
	UUID BufferLimits
	// Tasks 是任务列表缓冲区策略,容量单位为 int32 元素(每元素
	// 4 字节)。
	Tasks BufferLimits

	// CallbackBytes 是单条 callback details JSON 的字节上限,不含 NUL。
	CallbackBytes uint64
	// InternalEvents/InternalBytes 是内部事件队列的事件数与 JSON
	// 总字节上限,含复制预留与 dispatcher 在途项。
	InternalEvents uint64
	InternalBytes  uint64
	// PublicEvents/PublicBytes 是公开 Events 缓冲的事件数与 JSON
	// 总字节上限,按未确认接收的保守计费。
	PublicEvents uint64
	PublicBytes  uint64
	// PendingOperations 是未结算 Operation 数上限,含门禁前提交预留。
	PendingOperations uint64
	// EarlyResults 是 raw 返回 ID 前的早到完成数上限,固定 1:同一
	// 时刻至多一个「已进入 raw、尚未接合返回 ID」的窗口。显式配置
	// 也只能为 1,其他值一律拒绝。
	EarlyResults uint64
	// EarlyBytes 是早到结果原始 JSON 的总字节上限。
	EarlyBytes uint64
}

// RuntimeConfig 是 Open 的输入:显式库路径、用户目录、有序静态
// 选项、至少一个有序资源根、按 name 唯一的连接 extras,以及完整
// 的限制配置(Limits == nil 时使用整套默认值)。所有 slice 与
// RawMessage 在校验通过后被深拷贝冻结,见 configSnapshot。
type RuntimeConfig struct {
	LibraryPath      string
	UserDir          string
	StaticOptions    []StaticOption
	ResourceRoots    []string
	ConnectionExtras []ConnectionExtra
	Limits           *Limits
}

// 各限制字段的冻结范围上界(含边界),下界一律为 1(容量 0 拒绝);
// GrowthRetries 的范围是 [0, 16]。数值冻结自、。
const (
	inputStringBytesMax = 1 << 20 // 1,048,576
	inputJSONBytesMax   = 1 << 24 // 16,777,216
	configEntriesMax    = 1 << 12 // 4,096
	configBytesMax      = 1 << 26 // 67,108,864
	imagePNGCapMax      = 1 << 26 // 67,108,864
	imageBGRCapMax      = 1 << 27 // 134,217,728
	uuidCapMax          = 1 << 16 // 65,536
	tasksCapMax         = 1 << 20 // 1,048,576
	callbackBytesMax    = 1 << 24 // 16,777,216
	eventsCountMax      = 1 << 16 // 65,536
	internalBytesMax    = 1 << 27 // 134,217,728
	pendingOpsMax       = 1 << 12 // 4,096
	earlyBytesMax       = 1 << 24 // 16,777,216
	eventBytesTotalMax  = 1 << 28 // 268,435,456
	growthRetriesMax    = 16
)

// defaultLimits 返回整套默认限制。数值冻结自、
// 默认值相互满足全部交叉校验,因此对默认值不重复执行自检。
func defaultLimits() Limits {
	return Limits{
		InputStringBytes:  65_536,
		InputJSONBytes:    1_048_576,
		ConfigEntries:     256,
		ConfigBytes:       4_194_304,
		ImagePNG:          BufferLimits{Initial: 1_048_576, Max: 33_554_432, GrowthRetries: 5},
		ImageBGR:          BufferLimits{Initial: 4_194_304, Max: 67_108_864, GrowthRetries: 5},
		UUID:              BufferLimits{Initial: 64, Max: 4_096, GrowthRetries: 6},
		Tasks:             BufferLimits{Initial: 64, Max: 65_536, GrowthRetries: 10},
		CallbackBytes:     1_048_576,
		InternalEvents:    256,
		InternalBytes:     16_777_216,
		PublicEvents:      256,
		PublicBytes:       16_777_216,
		PendingOperations: 128,
		EarlyResults:      1,
		EarlyBytes:        1_048_576,
	}
}

// validateLimits 校验显式 Limits:先逐字段范围(越界、0 容量一律
// 拒绝,不静默截断或夹取),再校验每组 Initial <= Max,最后校验
// 交叉字段。所有失败都返回带字段路径的 ErrInvalidInput。
func validateLimits(l Limits) error {
	outOfRange := func(field string, min, max uint64) error {
		return fmt.Errorf("maa: Limits.%s out of range [%d, %d]: %w", field, min, max, native.ErrInvalidInput)
	}
	checkBuffer := func(field string, b BufferLimits, capMax uint64) error {
		switch {
		case b.Initial < 1 || b.Initial > capMax:
			return fmt.Errorf("maa: Limits.%s.Initial out of range [1, %d]: %w", field, capMax, native.ErrInvalidInput)
		case b.Max < 1 || b.Max > capMax:
			return fmt.Errorf("maa: Limits.%s.Max out of range [1, %d]: %w", field, capMax, native.ErrInvalidInput)
		case b.GrowthRetries > growthRetriesMax:
			return fmt.Errorf("maa: Limits.%s.GrowthRetries out of range [0, %d]: %w", field, uint64(growthRetriesMax), native.ErrInvalidInput)
		case b.Initial > b.Max:
			return fmt.Errorf("maa: Limits.%s.Initial (%d) exceeds Limits.%s.Max (%d): %w", field, b.Initial, field, b.Max, native.ErrInvalidInput)
		}
		return nil
	}

	switch {
	case l.InputStringBytes < 1 || l.InputStringBytes > inputStringBytesMax:
		return outOfRange("InputStringBytes", 1, inputStringBytesMax)
	case l.InputJSONBytes < 1 || l.InputJSONBytes > inputJSONBytesMax:
		return outOfRange("InputJSONBytes", 1, inputJSONBytesMax)
	case l.ConfigEntries < 1 || l.ConfigEntries > configEntriesMax:
		return outOfRange("ConfigEntries", 1, configEntriesMax)
	case l.ConfigBytes < 1 || l.ConfigBytes > configBytesMax:
		return outOfRange("ConfigBytes", 1, configBytesMax)
	}
	if err := checkBuffer("ImagePNG", l.ImagePNG, imagePNGCapMax); err != nil {
		return err
	}
	if err := checkBuffer("ImageBGR", l.ImageBGR, imageBGRCapMax); err != nil {
		return err
	}
	if err := checkBuffer("UUID", l.UUID, uuidCapMax); err != nil {
		return err
	}
	if err := checkBuffer("Tasks", l.Tasks, tasksCapMax); err != nil {
		return err
	}
	switch {
	case l.CallbackBytes < 1 || l.CallbackBytes > callbackBytesMax:
		return outOfRange("CallbackBytes", 1, callbackBytesMax)
	case l.InternalEvents < 1 || l.InternalEvents > eventsCountMax:
		return outOfRange("InternalEvents", 1, eventsCountMax)
	case l.InternalBytes < 1 || l.InternalBytes > internalBytesMax:
		return outOfRange("InternalBytes", 1, internalBytesMax)
	case l.PublicEvents < 1 || l.PublicEvents > eventsCountMax:
		return outOfRange("PublicEvents", 1, eventsCountMax)
	case l.PublicBytes < 1 || l.PublicBytes > internalBytesMax:
		return outOfRange("PublicBytes", 1, internalBytesMax)
	case l.PendingOperations < 1 || l.PendingOperations > pendingOpsMax:
		return outOfRange("PendingOperations", 1, pendingOpsMax)
	case l.EarlyResults != 1:
		// EarlyResults 固定为 1:同一时刻至多一个「已进入 raw、尚未
		// 接合返回 ID」的窗口,显式配置也只能为 1。
		return fmt.Errorf("maa: Limits.EarlyResults must be 1 (fixed), got %d: %w", l.EarlyResults, native.ErrInvalidInput)
	case l.EarlyBytes < 1 || l.EarlyBytes > earlyBytesMax:
		return outOfRange("EarlyBytes", 1, earlyBytesMax)
	}

	// 交叉字段:单项上限先于总和检查;总和用检查溢出的加法。
	switch {
	case l.InputStringBytes > l.ConfigBytes:
		return fmt.Errorf("maa: Limits.InputStringBytes (%d) exceeds Limits.ConfigBytes (%d): %w", l.InputStringBytes, l.ConfigBytes, native.ErrInvalidInput)
	case l.InputJSONBytes > l.ConfigBytes:
		return fmt.Errorf("maa: Limits.InputJSONBytes (%d) exceeds Limits.ConfigBytes (%d): %w", l.InputJSONBytes, l.ConfigBytes, native.ErrInvalidInput)
	case l.CallbackBytes > l.InternalBytes:
		return fmt.Errorf("maa: Limits.CallbackBytes (%d) exceeds Limits.InternalBytes (%d): %w", l.CallbackBytes, l.InternalBytes, native.ErrInvalidInput)
	case l.CallbackBytes > l.PublicBytes:
		return fmt.Errorf("maa: Limits.CallbackBytes (%d) exceeds Limits.PublicBytes (%d): %w", l.CallbackBytes, l.PublicBytes, native.ErrInvalidInput)
	case l.CallbackBytes > l.EarlyBytes:
		return fmt.Errorf("maa: Limits.CallbackBytes (%d) exceeds Limits.EarlyBytes (%d): %w", l.CallbackBytes, l.EarlyBytes, native.ErrInvalidInput)
	}
	sum, ok := addChecked(l.InternalBytes, l.PublicBytes)
	if !ok {
		return fmt.Errorf("maa: Limits.InternalBytes + PublicBytes overflows uint64: %w", native.ErrInvalidInput)
	}
	sum, ok = addChecked(sum, l.EarlyBytes)
	if !ok {
		return fmt.Errorf("maa: Limits.InternalBytes + PublicBytes + EarlyBytes overflows uint64: %w", native.ErrInvalidInput)
	}
	if sum > eventBytesTotalMax {
		return fmt.Errorf("maa: Limits.InternalBytes + PublicBytes + EarlyBytes (%d) exceeds %d: %w", sum, uint64(eventBytesTotalMax), native.ErrInvalidInput)
	}
	return nil
}

// addChecked、validateStringField、validatePathField、
// validateJSONObject(原 extras 专用校验的泛化形式)与 cString 已
// 整理到 snapshot.go 的共享辅助层,Runtime 配置与实例/连接/任务
// 输入共用同一套校验与复制逻辑,不维护两套实现。

// validateConfigContent 校验路径、静态选项与连接 extras 的内容约束:
// 全部为纯 Go 校验,发生在触碰原生库与生命周期门之前。字段路径写
// 进错误文本,便于调用方定位。
func validateConfigContent(cfg RuntimeConfig, limits Limits) error {
	if err := validatePathField("LibraryPath", cfg.LibraryPath, limits.InputStringBytes); err != nil {
		return err
	}
	if err := validatePathField("UserDir", cfg.UserDir, limits.InputStringBytes); err != nil {
		return err
	}
	if len(cfg.ResourceRoots) == 0 {
		return fmt.Errorf("maa: ResourceRoots must contain at least one root: %w", native.ErrInvalidInput)
	}
	for i, root := range cfg.ResourceRoots {
		if err := validatePathField(fmt.Sprintf("ResourceRoots[%d]", i), root, limits.InputStringBytes); err != nil {
			return err
		}
	}
	seenKey := make(map[int32]bool, len(cfg.StaticOptions))
	for i, opt := range cfg.StaticOptions {
		if opt.Key == 0 {
			// StaticOptionKey 的 Invalid=0,在纯 Go 校验中返回
			// ErrInvalidInput,不触发原生调用、不消耗生命周期。
			// 不得透传给原生:锁定 Core 对 key 0 走 default 返回失败,
			// 真实库上只会以 ErrNativeRejected 烧掉进程生命周期。
			return fmt.Errorf("maa: StaticOptions[%d].Key must not be 0 (Invalid): %w", i, native.ErrInvalidInput)
		}
		if seenKey[opt.Key] {
			return fmt.Errorf("maa: StaticOptions[%d] duplicates key %d: %w", i, opt.Key, native.ErrInvalidInput)
		}
		seenKey[opt.Key] = true
		if err := validateStringField(fmt.Sprintf("StaticOptions[%d].Value", i), opt.Value, limits.InputStringBytes, true); err != nil {
			return err
		}
	}
	seenName := make(map[string]bool, len(cfg.ConnectionExtras))
	for i, extra := range cfg.ConnectionExtras {
		if err := validateStringField(fmt.Sprintf("ConnectionExtras[%d].Name", i), extra.Name, limits.InputStringBytes, false); err != nil {
			return err
		}
		if seenName[extra.Name] {
			return fmt.Errorf("maa: ConnectionExtras[%d] duplicates name %q: %w", i, extra.Name, native.ErrInvalidInput)
		}
		seenName[extra.Name] = true
		if err := validateJSONObject(fmt.Sprintf("ConnectionExtras[%d].JSON", i), extra.JSON, limits.InputJSONBytes); err != nil {
			return err
		}
	}
	return nil
}

// validateConfigBudget 校验预算:单字符串/单份 JSON 预算已在内容
// 校验中逐项执行,这里用检查溢出的加法累计全部文本字节(LibraryPath
// + UserDir + 各资源根 + 各 option value + 各 extras name/JSON 的
// len 之和),并核对项数合计(StaticOptions + ResourceRoots +
// ConnectionExtras 的 len 之和)。终止 NUL 的每串 1 字节不计入本
// 预算,由分配方在分配前另行检查。
func validateConfigBudget(cfg RuntimeConfig, limits Limits) error {
	parts := make([]uint64, 0, 2*len(cfg.ConnectionExtras)+len(cfg.StaticOptions)+len(cfg.ResourceRoots)+2)
	parts = append(parts, uint64(len(cfg.LibraryPath)), uint64(len(cfg.UserDir)))
	for _, root := range cfg.ResourceRoots {
		parts = append(parts, uint64(len(root)))
	}
	for _, opt := range cfg.StaticOptions {
		parts = append(parts, uint64(len(opt.Value)))
	}
	for _, extra := range cfg.ConnectionExtras {
		parts = append(parts, uint64(len(extra.Name)), uint64(len(extra.JSON)))
	}
	total := uint64(0)
	for _, n := range parts {
		var ok bool
		total, ok = addChecked(total, n)
		if !ok {
			return fmt.Errorf("maa: config text bytes total overflows uint64: %w", native.ErrInvalidInput)
		}
	}
	if total > limits.ConfigBytes {
		return fmt.Errorf("maa: config text bytes total %d exceeds Limits.ConfigBytes %d: %w", total, limits.ConfigBytes, native.ErrInvalidInput)
	}
	entries := uint64(len(cfg.StaticOptions) + len(cfg.ResourceRoots) + len(cfg.ConnectionExtras))
	if entries > limits.ConfigEntries {
		return fmt.Errorf("maa: config entries %d exceeds Limits.ConfigEntries %d: %w", entries, limits.ConfigEntries, native.ErrInvalidInput)
	}
	return nil
}

// configSnapshot 是校验通过后、预占生命周期之前冻结的配置快照:
// 全部 slice/RawMessage 已深拷贝(RawMessage 逐字节 clone),Limits
// 为值复制。初始化只使用快照,调用者返回后修改原配置不影响快照;
// 只读 Go string 可按语言语义共享。
type configSnapshot struct {
	libraryPath      string
	userDir          string
	staticOptions    []StaticOption
	resourceRoots    []string
	connectionExtras []ConnectionExtra
	limits           Limits
}

// snapshotConfig 依次执行显式 Limits 校验 → 内容校验 → 预算校验 →
// 深拷贝冻结。任何失败都返回带字段路径的 ErrInvalidInput;该失败
// 发生在触碰生命周期门之前,不消耗进程生命周期。
func snapshotConfig(cfg RuntimeConfig) (configSnapshot, error) {
	limits := defaultLimits()
	if cfg.Limits != nil {
		if err := validateLimits(*cfg.Limits); err != nil {
			return configSnapshot{}, err
		}
		limits = *cfg.Limits
	}
	if err := validateConfigContent(cfg, limits); err != nil {
		return configSnapshot{}, err
	}
	if err := validateConfigBudget(cfg, limits); err != nil {
		return configSnapshot{}, err
	}

	snap := configSnapshot{
		libraryPath:      cfg.LibraryPath,
		userDir:          cfg.UserDir,
		staticOptions:    make([]StaticOption, len(cfg.StaticOptions)),
		resourceRoots:    make([]string, len(cfg.ResourceRoots)),
		connectionExtras: make([]ConnectionExtra, len(cfg.ConnectionExtras)),
		limits:           limits,
	}
	copy(snap.staticOptions, cfg.StaticOptions)
	copy(snap.resourceRoots, cfg.ResourceRoots)
	for i, extra := range cfg.ConnectionExtras {
		// RawMessage 必须逐字节 clone:调用者可能在 Open 返回后原地
		// 改写底层字节。
		snap.connectionExtras[i] = ConnectionExtra{Name: extra.Name, JSON: bytes.Clone(extra.JSON)}
	}
	return snap, nil
}

// lifecycleState 是进程级 Runtime 生命周期的阶段:idle 尚未预占、
// reserved 已预占(初始化进行中)、ready 已发布活动 Runtime、
// terminated 使用权已终止。状态迁移:idle→reserved→ready 与
// reserved→terminated(Open 事务);ready→terminated(实例关闭、
// 创建失败回滚、Runtime.Close)。
type lifecycleState uint8

const (
	lifecycleIdle lifecycleState = iota
	lifecycleReserved
	lifecycleReady
	lifecycleTerminated
)

// instanceSlotState 是 Runtime 内唯一实例名额的占用阶段(首期每
// 进程单 Runtime、单活动 Instance;Runtime 必须看见尚在 initializing
// 的实例):none 尚无实例、creating 名额已预留且创建事务进行中
// (创建 lease 已登记)、active 已发布活动实例。名额预占后的创建失
// 败与实例关闭都使整个生命周期门进入 terminated,名额不复用、不重
// 置——同进程不重建实例。
type instanceSlotState uint8

const (
	slotNone     instanceSlotState = iota
	slotCreating                   // 名额已预留,创建事务进行中(创建 lease 已登记)
	slotActive                     // ready 实例已发布
)

// lifecycleGate 是进程级单 Runtime 生命周期门:串行化全部 Open 的
// 预占,保证并发 Open 至多一个进入原生初始化;记录原生加载开始后
// 的首个失败原因,terminated 之后供后续 Open 诊断。 在同一状
// 态门内承载唯一实例名额与创建 lease; Runtime.Close 经
// beginRuntimeClose(关闭创建准入并快照)与 runtimeCloseResult(终
// 止后的稳定最终结果)在同一门内收束关闭与创建竞争。
//
// ready Runtime 发布之后的 terminate 只来自实例关闭、创建失败回滚与
// Runtime.Close,三者携带同一份关闭/回滚最终错误(多次 Close
// 返回同一终态)——因此 firstErr 在该阶段即稳定最终结果,正常关闭
// 保持 nil。
//
// 锁序:lifecycleGate.mu → instanceState.mu(publishInstance 持门锁
// 调用状态锁内发布);不存在反向获取——实例状态的方法绝不获取本门。
type lifecycleGate struct {
	mu       sync.Mutex
	state    lifecycleState
	active   *Runtime // 已发布的活动 Runtime
	firstErr error    // 原生加载开始后的首个失败原因,terminated 后供后续 Open 诊断

	// 唯一实例名额:slot 非 none 时名额已占用;creating 期间
	// creationDone 是创建 lease——登记于 reserveInstance,由创建者在
	// 事务结束(发布或回滚完成)后经 releaseCreationLease 关闭一次;
	// creationDoneClosed 防重复关闭。activeInstance 是已发布的活动实
	// 例(Runtime 必须能找到它; Runtime.Close 经此关
	// 闭)。closeRequested 表示创建准入已关闭(Runtime.Close 已请求):
	// 此后预留与发布都被拒绝,已 closing 时由创建者完成回滚。
	slot               instanceSlotState
	creationDone       chan struct{}
	creationDoneClosed bool
	closeRequested     bool
	activeInstance     *instance
}

// reserve 原子预占生命周期:idle→reserved 成功;reserved/ready 拒绝
// 并返回 ErrRuntimeInUse(不复用已有对象、不忽略新配置);terminated
// 拒绝并返回包装 ErrRuntimeTerminated 的错误,附首个失败原因文本作
// 诊断(%v,不建立错误链——首个失败已在其原始错误中保留)。预占即
// 不可回头:成功之后的失败必须经 terminate 进入 terminated。
func (g *lifecycleGate) reserve() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.state {
	case lifecycleIdle:
		g.state = lifecycleReserved
		return nil
	case lifecycleReserved, lifecycleReady:
		return native.ErrRuntimeInUse
	case lifecycleTerminated:
		return g.terminatedErrLocked()
	default:
		// lifecycleState 只有四个值,其余分支不可达;到达此处意味着
		// 私有状态机的内部不变量被破坏,按编程错误处理,不允许静默
		// 继续污染生命周期。
		panic(fmt.Sprintf("maa: lifecycle gate in unknown state %d", g.state))
	}
}

// terminatedErrLocked 返回 terminated 状态下的统一拒绝错误:附首个
// 失败原因文本作诊断(%v,不建立错误链——首个失败已在其原始错误中
// 保留);正常关闭/回滚终止(无失败原因)返回裸哨兵。要求持有 mu。
func (g *lifecycleGate) terminatedErrLocked() error {
	if g.firstErr != nil {
		return fmt.Errorf("maa: runtime lifecycle already terminated (first failure: %v): %w",
			g.firstErr, native.ErrRuntimeTerminated)
	}
	return native.ErrRuntimeTerminated
}

// reserveInstance 在 Runtime 状态门内预留唯一实例名额并登记创建
// lease(NewInstance 第 2 步:在 Runtime 中预留唯一实例名额并登
// 记创建 lease)。成功前置:Runtime 已发布(ready)、名额空闲且创建
// 准入未关闭;成功即 slot=creating 并建立创建 lease。拒绝语义:
//   - terminated:包装 ErrRuntimeTerminated(附首个失败原因),预占
//     名额后的创建失败与实例关闭都到达此处——同进程不重建实例、不
//     重试资源组合;
//   - 非 ready(reserved 等,公开面不可达,防御拒绝):ErrRuntimeInUse;
//   - 创建准入已关闭(Runtime.Close 已请求):包装 ErrClosing(
//     错误契约:Runtime.Close 期间创建者不得再发布实例);
//   - 名额已占用(creating 或 active):包装 ErrRuntimeInUse——唯一
//     实例名额已消耗,不复用、不排队。
func (g *lifecycleGate) reserveInstance() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.state == lifecycleTerminated:
		return g.terminatedErrLocked()
	case g.state != lifecycleReady:
		return fmt.Errorf("maa: runtime is not ready, instance creation rejected: %w", native.ErrRuntimeInUse)
	case g.closeRequested:
		return fmt.Errorf("maa: runtime close has been requested, instance creation rejected: %w", native.ErrClosing)
	case g.slot != slotNone:
		return fmt.Errorf("maa: the runtime allows a single instance, instance slot is %d: %w",
			g.slot, native.ErrRuntimeInUse)
	}
	g.slot = slotCreating
	g.creationDone = make(chan struct{})
	g.creationDoneClosed = false
	return nil
}

// publishInstance 在 Runtime 状态门内复核可发布性并发布 ready 实例
// (创建者在 Runtime 的同一状态门内决定发布;ready 发布与
// Runtime 状态复核在同一状态门内):创建准入未关闭且实例状态无早到
// 故障时,在门锁内完成实例状态发布(tryPublishReady,锁序:门锁 →
// 状态锁)并把名额转为 active、登记活动实例;否则不发布并返回拒绝
// 原因,由创建者完成同一单次关闭/回滚流程后释放 lease(Runtime.
// Close 等待该流程,不重复 Destroy):
//   - 创建准入已关闭:包装 ErrClosing(不发布,不产生 ready 实例);
//   - 早到故障:返回首因(tryPublishReady 在状态锁内原子复核——故障
//     在选项应用后、发布前的任意时点到达都只能得到「不发布」一种结
//     论,不存在 faulted 仍发布的窗口)。
//
// 调用契约:仅由创建者在 reserveInstance 成功之后、释放 lease 之前
// 调用一次;slot 非 creating 或状态非 ready 到达此处即内部不变量破
// 坏,panic(创建失败回滚已使门 terminated,发布不可达)。
func (g *lifecycleGate) publishInstance(in *instance) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != lifecycleReady || g.slot != slotCreating {
		panic(fmt.Sprintf("maa: lifecycle gate publishes instance in state %d with slot %d", g.state, g.slot))
	}
	if g.closeRequested {
		return fmt.Errorf("maa: runtime close has been requested, the instance is not published: %w", native.ErrClosing)
	}
	if err := in.state.tryPublishReady(); err != nil {
		// 早到故障(callback 已可接收,早到故障会使创建失败并按既
		// 定规则清理):不发布,名额保持 creating,回滚统一收尾。
		return err
	}
	g.slot = slotActive
	g.activeInstance = in
	return nil
}

// releaseCreationLease 释放创建 lease:由创建者在事务结束(全部步骤
// 成功发布 ready,或统一回滚完成)后调用一次(由创建者完成同一
// 单次关闭/回滚流程后释放 lease;Runtime.Close 等待该流程)。未预留
// 即释放或重复释放即内部不变量破坏,panic。
func (g *lifecycleGate) releaseCreationLease() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.creationDone == nil || g.creationDoneClosed {
		panic("maa: creation lease released without a reservation or more than once")
	}
	g.creationDoneClosed = true
	close(g.creationDone)
}

// waitCreationLease 等待已登记的创建 lease 释放(Runtime.Close 先
// 关闭创建准入,再等待已登记的创建 lease,不能因尚无 ready Instance
// 而提前完成):创建事务结束(发布或回滚完成)后返回。尚未预留名额
// 时立即返回(无可等待的 lease);Runtime.Close 经 beginRuntimeClose
// 的同锁段快照取 lease 通道后自行等待,本方法保留为最小接口(门契约
// 用例的防挂死等待使用)。
func (g *lifecycleGate) waitCreationLease() {
	g.mu.Lock()
	ch := g.creationDone
	g.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// beginRuntimeClose 是 Runtime.Close 的第一步(Runtime.Close 先
// 关闭创建准入):在同一状态门锁段内置位创建准入关闭标志并快照当前
// 活动实例与创建 lease 通道。原子性保证:标志置位后 reserveInstance
// 与 publishInstance 一律被拒,快照之后不会再出现新的创建 lease、也
// 不会有实例再发布为 active——快照结果即「需要收尾的实例」的最终集
// 合;之后 activeInstance 只会被 terminate 清空(并发关闭者已代为终
// 结,快照到的指针仍可安全幂等关闭)。返回的 lease 可能为 nil(从未
// 登记创建事务)或已关闭(事务已收尾),两种情况下等待都立即返回。
func (g *lifecycleGate) beginRuntimeClose() (*instance, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeRequested = true
	return g.activeInstance, g.creationDone
}

// runtimeCloseResult 返回生命周期已终止时的稳定最终结果,未终止时
// 第二返回值为 false。依据见 lifecycleGate 文档:ready Runtime 发布
// 之后的 terminate 只携带同一份关闭/回滚最终错误,firstErr 因此即
// 多次/并发 Close 共享的稳定终态(多次 Close 返回同一终态),
// 正常关闭为 nil。要求在等待创建 lease 结束之后调用:创建回滚先
// terminate 再释放 lease(Runtime.Close 不得在回滚完成前返回)。
func (g *lifecycleGate) runtimeCloseResult() (error, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != lifecycleTerminated {
		return nil, false
	}
	return g.firstErr, true
}

// closeCreationAdmission 关闭创建准入(Runtime.Close 先关闭创建准
// 入):此后 reserveInstance 与 publishInstance 一律拒绝,正在进行的
// 创建事务不受影响、由创建者自行完成(发布或回滚)。返回本次调用是
// 否为首个请求(后续调用幂等返回 false)。Runtime.Close 使用合并了
// 快照的 beginRuntimeClose;本方法保留为最小准入接口(门契约用例与
// 手动准入注入使用)。
func (g *lifecycleGate) closeCreationAdmission() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closeRequested {
		return false
	}
	g.closeRequested = true
	return true
}

// terminate 记录原生加载开始后的首个失败原因并进入 terminated 终态。
// 同一道内 terminate 至多被调用一次(任何后续 Open 都在 reserve 处
// 被拒绝),保留首个失败,不覆盖;终止使用同时解除活动实例登记的可
// 达引用(实例关闭或创建失败回滚后,后续 Open/NewInstance 一律
// 被拒,已终止的实例不再被生命周期门引用)。
func (g *lifecycleGate) terminate(cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.firstErr == nil {
		g.firstErr = cause
	}
	g.state = lifecycleTerminated
	g.activeInstance = nil
}

// publish 在全部初始化步骤成功后发布 ready Runtime 并记录活动对象。
// 调用契约:publish 只能在同一次 open 的 reserve 成功之后调用;其他
// 状态到达此处意味着私有状态机内部不变量被破坏,按编程错误处理,
// 不允许静默继续。
func (g *lifecycleGate) publish(rt *Runtime) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != lifecycleReserved {
		panic(fmt.Sprintf("maa: lifecycle gate publish from state %d", g.state))
	}
	g.state = lifecycleReady
	g.active = rt
}

// initSession 是初始化步骤的注入边界:包装一次成功加载的临时 API
// 表。测试注入假会话构造失败路径,不替换生产包的全局函数变量,
// 避免并行测试相互污染。
type initSession interface {
	// capabilities 返回本次加载的能力快照。
	capabilities() native.Capabilities
	// table 返回本次加载发布的临时 API 表;生产实现返回真实表,
	// 假会话可返回 nil(本用例的用例不触达 raw 调用)。
	table() *native.APITable
	// version 借用返回原生版本字符串指针;原生内存,必须立即有界
	// 复制,不 free、不外泄视图。
	version() *byte
	// nullSize 返回输出缓冲区协议的失败哨兵值。
	nullSize() uint64
	// setUserDir 设置进程级用户目录,返回 0 表示原生拒绝。
	setUserDir(path *byte) uint8
	// setStaticOption 设置静态选项,返回 0 表示原生拒绝。
	setStaticOption(key int32, value *byte) uint8
	// loadResource 加载资源根,返回 0 表示原生拒绝。
	loadResource(path *byte) uint8
	// setConnectionExtras 配置连接 extras;C void 调用,没有成功码。
	setConnectionExtras(name *byte, extras *byte)
}

// initEnv 是库加载阶段的注入边界:打开 path 指向的动态库并解析
// 注册临时 API 表,返回初始化会话。
type initEnv interface {
	loadLibrary(path string) (initSession, error)
}

// nativeEnv 是生产环境:经生产 loader(NewLoader)打开库,再经
// ResolveAndRegister 执行两阶段符号注册事务。
type nativeEnv struct{}

var _ initEnv = nativeEnv{}

// loadLibrary 打开 path 指向的动态库并解析注册全部适用符号。打开
// 失败(*LibraryLoadError)、缺符号(AggregateError)与注册异常
// (*FunctionRegistrationError)都原样返回,保留结构化错误类型与
// 错误链,由 open 统一交给生命周期门。
func (nativeEnv) loadLibrary(path string) (initSession, error) {
	lib, err := native.NewLoader().Open(path)
	if err != nil {
		return nil, err
	}
	table, err := native.ResolveAndRegister(lib)
	if err != nil {
		return nil, err
	}
	return nativeSession{apiTable: table}, nil
}

// nativeSession 包装生产 API 表,把初始化步骤转发到 calls.go 的
// raw 调用包装;*byte 的编码/解释/复制由 open 完成,本类型不解释。
// 字段名 apiTable 与接口方法 table() 区分,避免字段/方法同名冲突。
type nativeSession struct {
	apiTable *native.APITable
}

var _ initSession = nativeSession{}

func (s nativeSession) capabilities() native.Capabilities { return s.apiTable.Capabilities() }

func (s nativeSession) table() *native.APITable { return s.apiTable }

func (s nativeSession) version() *byte { return s.apiTable.GetVersion() }

func (s nativeSession) nullSize() uint64 { return s.apiTable.GetNullSize() }

func (s nativeSession) setUserDir(path *byte) uint8 { return s.apiTable.SetUserDir(path) }

func (s nativeSession) setStaticOption(key int32, value *byte) uint8 {
	return s.apiTable.SetStaticOption(key, value)
}

func (s nativeSession) loadResource(path *byte) uint8 { return s.apiTable.LoadResource(path) }

func (s nativeSession) setConnectionExtras(name *byte, extras *byte) {
	s.apiTable.SetConnectionExtras(name, extras)
}

// profileNullSize 是锁定 profile(maa-v6.20.1)的 NullSize 失败哨兵
// GetNullSize 必须返回该值,否则输出缓冲区容量协议
// 无法成立,视为协议错误。核验通过后作为元数据保存,供外部核验。
const profileNullSize = math.MaxUint64

// versionBytesLimit 是 Version 字符串的冻结字节上限(
// VersionBytes 行:固定 4,096,不提供覆盖字段)。
const versionBytesLimit = 4096

// readVersion 从原生返回的借用指针读取 Version:在至多
// versionBytesLimit+1 字节范围内寻找 NUL,复制为 Go string。空指针、
// 界内(含 4,097 字节扫描范围)无 NUL、空串与非法 UTF-8 都是协议
// 错误,不得把矛盾证据伪装成成功。成功时复制为 Go 拥有的数据,
// 原生静态字符串不 free。
func readVersion(p *byte) (string, error) {
	if p == nil {
		return "", fmt.Errorf("maa: GetVersion returned a null pointer: %w", native.ErrProtocol)
	}
	span := unsafe.Slice(p, versionBytesLimit+1)
	end := bytes.IndexByte(span, 0)
	switch {
	case end < 0:
		return "", fmt.Errorf("maa: GetVersion string has no NUL within %d bytes: %w", versionBytesLimit+1, native.ErrProtocol)
	case end == 0:
		return "", fmt.Errorf("maa: GetVersion string is empty: %w", native.ErrProtocol)
	case !utf8.ValidString(string(span[:end])):
		return "", fmt.Errorf("maa: GetVersion string is not valid UTF-8: %w", native.ErrProtocol)
	}
	return string(span[:end]), nil
}

// open 是初始化事务的私有入口:env 与 gate 可注入,便于测试构造
// 失败路径与独立生命周期门;生产入口 Open 使用生产 env 与进程级
// 唯一门。执行顺序严格按:
//
//  1. 纯 Go 校验 + 快照(失败不触碰 gate,不消耗进程生命周期);
//  2. gate.reserve():预占即不可回头;
//  3. env.loadLibrary:打开库并解析注册临时 API 表;
//  4. 会话步骤 version → nullSize → userDir → 静态选项(按配置
//     顺序)→ 资源根(按配置顺序)→ extras(按配置顺序),逐步
//     保留原始失败原因;
//  5. 全部成功后构造 Runtime 并发布。
//
// 步骤 3 起的任何失败都以原始错误返回(保留结构化错误类型与错误
// 链,不包 ErrRuntimeTerminated),同时使 gate 进入 terminated:后续
// Open 返回包装 ErrRuntimeTerminated 的错误。失败时不发布 Runtime,
// 公开面拿不到半初始化对象。
func open(cfg RuntimeConfig, env initEnv, gate *lifecycleGate) (*Runtime, error) {
	snap, err := snapshotConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := gate.reserve(); err != nil {
		return nil, err
	}

	sess, err := env.loadLibrary(snap.libraryPath)
	if err != nil {
		gate.terminate(err)
		return nil, err
	}

	// a. Version:借用原生内存,有界复制为 Go string。
	version, err := readVersion(sess.version())
	if err != nil {
		gate.terminate(err)
		return nil, err
	}
	// b. NullSize:必须等于锁定 profile 的失败哨兵,否则输出缓冲区
	// 容量协议无法成立。
	ns := sess.nullSize()
	if ns != profileNullSize {
		err := fmt.Errorf("maa: GetNullSize returned %#x, want the locked profile sentinel (%#x): %w",
			ns, uint64(profileNullSize), native.ErrProtocol)
		gate.terminate(err)
		return nil, err
	}
	// c. 用户目录:返回 0 表示原生拒绝,无结构化原因,不编造
	// LastError。
	dir, dirBuf := cString(snap.userDir)
	ret := sess.setUserDir(dir)
	runtime.KeepAlive(dirBuf)
	if ret == 0 {
		err := fmt.Errorf("maa: SetUserDir(%q) returned failure: %w", snap.userDir, native.ErrNativeRejected)
		gate.terminate(err)
		return nil, err
	}
	// d. 静态选项:按配置顺序,先于资源加载(资源初始化前配置)。
	for _, opt := range snap.staticOptions {
		value, valueBuf := cString(opt.Value)
		ret := sess.setStaticOption(opt.Key, value)
		runtime.KeepAlive(valueBuf)
		if ret == 0 {
			err := fmt.Errorf("maa: SetStaticOption(key=%d) returned failure: %w", opt.Key, native.ErrNativeRejected)
			gate.terminate(err)
			return nil, err
		}
	}
	// e. 资源根:按配置顺序加载。
	for _, root := range snap.resourceRoots {
		path, pathBuf := cString(root)
		ret := sess.loadResource(path)
		runtime.KeepAlive(pathBuf)
		if ret == 0 {
			err := fmt.Errorf("maa: LoadResource(%q) returned failure: %w", root, native.ErrNativeRejected)
			gate.terminate(err)
			return nil, err
		}
	}
	// f. 连接 extras:按配置顺序;C void 调用,没有成功码可查,不
	// 伪造成功也不失败。
	for _, extra := range snap.connectionExtras {
		name, nameBuf := cString(extra.Name)
		payload, payloadBuf := cString(string(extra.JSON))
		sess.setConnectionExtras(name, payload)
		runtime.KeepAlive(nameBuf)
		runtime.KeepAlive(payloadBuf)
	}

	rt := &Runtime{
		table:    sess.table(),
		caps:     sess.capabilities(),
		version:  version,
		nullSize: ns,
		limits:   snap.limits,
		gate:     gate,
		logger:   apiTableLogger{table: sess.table()},
		querier:  apiTableQuery{table: sess.table()},
	}
	gate.publish(rt)
	return rt, nil
}

// processLifecycle 是进程级唯一的 Runtime 生命周期门:首期每进程
// 单 Runtime,全部 Open 经它串行化预占。
var processLifecycle lifecycleGate

// Open 执行单进程初始化事务:校验并复制配置 → 预占进程级单 Runtime
// 生命周期 → 加载/解析/注册临时 API 表 → 复制 Version/核验 NullSize
// → 设置用户目录与静态选项 → 加载资源根 → 配置连接 extras → 发布
// ready Runtime。全部成功才返回 *Runtime;任何初始化失败都不发布
// 公开面可获得的半成品对象。
//
// 进程级契约(详见包文档):
//   - 并发或重复 Open:已有预占或活动 Runtime 时返回 ErrRuntimeInUse,
//     不复用已有对象、也不忽略新配置;
//   - 原生加载开始后的失败:该进程进入 terminated 使用状态,后续
//     Open 返回包装 ErrRuntimeTerminated 的错误;失败本身以原始错误
//     返回,保留结构化错误类型与错误链;
//   - 配置校验失败:发生在预占之前,返回带字段路径的 ErrInvalidInput,
//     不消耗进程生命周期,修正输入后仍可进行首次原生加载;
//   - 配置快照:校验通过后深拷贝冻结,Open 返回后修改原配置不影响
//     已初始化 Runtime;调用者在 Open 返回前不得并发修改输入。
func Open(cfg RuntimeConfig) (*Runtime, error) {
	return open(cfg, nativeEnv{}, &processLifecycle)
}

// Capabilities 与错误面同一模式,经类型别名再导出 internal/native
// 的只读能力快照类型:可选符号(AsstCreate、AsstGetImageBgr、
// AsstAsyncAttachWindow)缺失只关闭对应能力,不阻塞初始化。
type Capabilities = native.Capabilities

// Runtime 表示一次成功初始化的进程级 MaaCore 绑定。全部字段未导出
// 且在初始化时冻结,发布后只读;零值不可用,只能经 Open 返回的
// 指针使用,不按值复制。table 是本次加载事务发布的临时 API 表(供
// 实例创建与同步业务调用经它调用 raw 函数),caps/version/nullSize/
// limits 是初始化时复制的不可变元数据与限制快照;gate 是本 Runtime
// 的生命周期门(唯一实例名额的预留/复核、终止使用与全局 Log 的生
// 命周期检查经它进行);logger 是全局日志的 native 接缝;querier 是
// 进程级实验查询(MapLevelKey/ItemName)的 native 接缝。
type Runtime struct {
	table    *native.APITable
	caps     native.Capabilities
	version  string
	nullSize uint64
	limits   Limits
	gate     *lifecycleGate
	logger   runtimeLogger
	querier  runtimeQuerier
}

// Version 返回初始化时复制的 Core 版本字符串。只读不可变元数据,
// 不触发任何原生调用;并发安全。
func (rt *Runtime) Version() string { return rt.version }

// Capabilities 返回初始化时复制的能力快照副本。只读不可变元数据,
// 不触发任何原生调用;并发安全。
func (rt *Runtime) Capabilities() Capabilities { return rt.caps }

// NullSize 返回初始化时核验并保存的 NullSize 哨兵值(锁定 profile
// 约定为 math.MaxUint64)。它是输出缓冲区容量协议的锚点,供外部
// 核验;只读不可变元数据,不触发任何原生调用;并发安全。
func (rt *Runtime) NullSize() uint64 { return rt.nullSize }

// NewInstance 创建一个带 callback 的实例:校验并复制实例配置 → 预留
// 唯一实例名额并登记创建 lease → 建立实例状态、事件入口、callback
// 门与 token → CreateEx → 按序应用实例选项 → 在 Runtime 同一状态门
// 内复核可发布性与早到故障并发布 ready Instance(初始化第 2 阶
// 段)。全部步骤成功才返回 *Instance;任何失败都不向调用者返回半初
// 始化实例,也不返回尚可重试的假象。
//
// 步骤与错误契约(详见、):
//   - 预占前纯 Go 配置校验失败:返回带字段路径的 ErrInvalidInput,
//     不消耗 Runtime,不改名额与生命周期;
//   - 预留被拒:Runtime 已终止使用(实例关闭、创建失败回滚或初始
//     化失败之后)返回包装 ErrRuntimeTerminated 的错误;名额已被占
//     用(creating/active)返回包装 ErrRuntimeInUse 的错误;Runtime
//     关闭准入已关闭返回包装 ErrClosing 的错误;
//   - 预占后失败(CreateEx=0、选项被原生拒绝、早到故障、发布被
//     closing 抢先):统一走单次关闭/回滚流程清理,Runtime 终止使用
//     (后续 Open/NewInstance 返回包装 ErrRuntimeTerminated 的错误),
//     不允许同进程重建实例或重试资源组合;
//   - CreateEx 之前 token、callback 门与事件队列已可用:创建期间的
//     早到消息照常准入入队,不遗失状态;已发布实例的事件缓冲
//     中因此可能带有创建期间收到的早到消息。
//
// 配置快照:校验通过后、预留名额前深拷贝冻结,方法返回后
// 修改原配置不影响实例;调用者在方法返回前不得并发修改输入。
func (rt *Runtime) NewInstance(cfg InstanceConfig) (*Instance, error) {
	return createInstance(rt, cfg,
		apiTableCreator{table: rt.table},
		apiTableCloser{table: rt.table},
		apiTableCaller{table: rt.table})
}

// Close 同步完成本 Runtime 绑定使用的关闭:幂等,不卸载原
// 生库,不重置进程级状态——恢复仍只能来自退出并启动新 worker 进程。
// 顺序固定按 Close 契约:
//
//  1. 关闭创建准入(Runtime 状态门内):此后 reserveInstance 与
//     publishInstance 一律被拒,进行中的创建事务不被打断;
//  2. 等待已登记的创建 lease:没有 ready 实例时同样检查 initializing
//     实例——未发布的创建实例由创建者完成统一回滚(已取得 handle:
//     一次 Stop 一次 Destroy;CreateEx=0:只清理 Go 桥接状态)并释放
//     lease,回滚完成之前 Close 不返回,也不重复 Destroy(由创
//     建者完成同一单次关闭/回滚流程,Close 等待该流程);
//  3. 复核生命周期状态:等待期间创建回滚或并发的 Instance.Close 可
//     能已终止使用,其存储的最终结果即本次 Close 的稳定最终结果;
//  4. 已发布的活动实例交给唯一关闭者:委托与 Instance.Close 同一单
//     次关闭流程(复用边界,不建立第二套 Stop/Destroy 所有者),
//     并发的 Instance.Close 共享同一 closeDone 与最终错误;
//  5. 终止进程级使用状态:后续 Open 与 NewInstance 一律返回包装
//     ErrRuntimeTerminated 的错误(一个实例关闭后 Runtime 进入
//     终止态,不再允许 NewInstance/LoadResource)。
//
// 多次或并发的 Runtime.Close 与 Instance.Close 收到同一稳定最终结果
// (多次 Close 返回同一终态);终止后的重复 Close 直接返回已存
// 储结果,不再进入 native。Close 不接受强制回收超时,也不承诺在
// native 卡住时返回(由 control 对 worker 执行有期限的退出监督)。
func (rt *Runtime) Close() error {
	// 1. 关闭创建准入并快照(同一状态门锁段, 第 1 步的创建准入侧)。
	active, lease := rt.gate.beginRuntimeClose()
	// 2. 等待已登记创建 lease(不能因尚无 ready Instance 而提前
	//    完成;lease 为 nil 或已关闭时立即返回)。
	if lease != nil {
		<-lease
	}
	// 3. 复核:终止后的 firstErr 即关闭/回滚的稳定最终结果。
	if err, terminated := rt.gate.runtimeCloseResult(); terminated {
		return err
	}
	// 4. 活动实例交给唯一关闭者(Runtime.Close 委托同一流程,
	//    不重复 Destroy;并发 Instance.Close 共享同一 closeDone)。
	var err error
	if active != nil {
		err = active.close()
	}
	// 5. 终止使用(terminate 保留首个原因;并发终止者携带同一最终结
	//    果,不互相覆盖)。
	rt.gate.terminate(err)
	return err
}
