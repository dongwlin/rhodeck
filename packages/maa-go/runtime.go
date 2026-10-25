// 本文件实现 Runtime 的配置契约与单进程初始化事务(Open)。
// 执行顺序严格按绑定设计 §8:纯 Go 配置校验与深拷贝快照 → 原子
// 预占单 Runtime 生命周期 → 装载临时 API 表 → 复制 Version/核验
// NullSize → 设置用户目录 → 静态选项 → 资源根 → 连接 extras →
// 发布 ready Runtime。
//
// 生命周期契约(绑定设计 §8):
//   - 每进程单 Runtime:进程级生命周期门串行化全部 Open;已有预占
//     或活动 Runtime 时,并发或重复 Open 返回 ErrRuntimeInUse,不
//     复用已有对象、也不忽略新配置。
//   - 预占即不可回头:预占之后的任何失败(库加载、符号注册、初始
//     化步骤)都使门进入 terminated,后续 Open 一律返回
//     ErrRuntimeTerminated;不以 Dlclose 或热重载恢复,恢复只能来自
//     退出并启动新 worker 进程。
//   - 配置校验发生在触碰生命周期门之前:纯 Go 校验失败不消耗进程
//     生命周期,修正输入后仍可进行首次原生加载。
//   - 配置快照所有权:校验通过后、预占之前,配置被深拷贝冻结;
//     Open 返回后修改原配置不影响已初始化 Runtime。调用者在 Open
//     返回前不得并发修改输入。
//
// 本里程碑不交付 NewInstance/Close/Instance:公开面没有可创建半成品
// Instance 的方法,实例创建与完整关闭由 M3 承接。

package maa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// StaticOption 是一条进程级静态选项:key 为 int32(StaticOptionKey
// 语义,锁定值见绑定设计 §3.0:Invalid=0、CpuOCR=1、GpuOCR=2;key
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

// Limits 汇集 Open 与后续实例输入的全部容量限制(MAA-03 冻结;
// 单位、默认值与范围见绑定设计 §5.1、§6.2、§7.1)。Limits == nil
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
// GrowthRetries 的范围是 [0, 16]。数值冻结自绑定设计 §5.1、§6.2、§7.1。
const (
	inputStringBytesMax = 1 << 20 // 1,048,576
	inputJSONBytesMax   = 1 << 24 // 16,777,216
	configEntriesMax    = 1 << 12 // 4,096
	configBytesMax      = 1 << 26 // 67,108,864
	imagePNGCapMax      = 1 << 26 // 67,108,864
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

// defaultLimits 返回整套默认限制。数值冻结自绑定设计 §5.1、§6.2、
// §7.1;默认值相互满足全部交叉校验,因此对默认值不重复执行自检。
func defaultLimits() Limits {
	return Limits{
		InputStringBytes:  65_536,
		InputJSONBytes:    1_048_576,
		ConfigEntries:     256,
		ConfigBytes:       4_194_304,
		ImagePNG:          BufferLimits{Initial: 1_048_576, Max: 33_554_432, GrowthRetries: 5},
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

// addChecked 返回 a+b 与「是否未溢出」:预算累计必须用检查溢出的
// 加法,溢出时不产出伪造的部分和。
func addChecked(a, b uint64) (uint64, bool) {
	sum := a + b
	if sum < a {
		return 0, false
	}
	return sum, true
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

// validateStringField 校验单个字符串输入字段:合法 UTF-8、无内嵌
// NUL,且长度不超过单字符串预算;allowEmpty 决定是否允许空串——
// 路径与 extras name 必须非空,option value 允许空串(空值编码为
// 单个 NUL 字节的空 C 字符串,不传 nil)。field 是错误文本中的字段
// 路径(如 LibraryPath、StaticOptions[0].Value)。
func validateStringField(field, value string, max uint64, allowEmpty bool) error {
	switch {
	case value == "" && !allowEmpty:
		return fmt.Errorf("maa: %s must not be empty: %w", field, native.ErrInvalidInput)
	case !utf8.ValidString(value):
		return fmt.Errorf("maa: %s is not valid UTF-8: %w", field, native.ErrInvalidInput)
	case strings.IndexByte(value, '\x00') >= 0:
		return fmt.Errorf("maa: %s contains an embedded NUL byte: %w", field, native.ErrInvalidInput)
	case uint64(len(value)) > max:
		return fmt.Errorf("maa: %s length %d exceeds Limits.InputStringBytes %d: %w", field, len(value), max, native.ErrInvalidInput)
	}
	return nil
}

// validatePathField 校验单个路径输入字段:在非空字符串校验之上追加
// 绝对路径要求。路径不做 shell 展开、不做存在性检查——文件是否
// 存在、原生是否接受,留给加载/调用阶段失败。
func validatePathField(field, value string, max uint64) error {
	if err := validateStringField(field, value, max, false); err != nil {
		return err
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("maa: %s %q is not an absolute path: %w", field, value, native.ErrInvalidInput)
	}
	return nil
}

// validateExtraJSON 校验一条连接 extras 的 JSON 原文:
//  1. 实际 NUL 字节(0x00)直接拒绝;字符串内的 \u0000 转义合法、
//     原样保留,不解码成 C 字符串输入;
//  2. 全文必须是合法 UTF-8;
//  3. 长度不超过单份 JSON 预算;
//  4. 用 json.Decoder 确认恰好一个完整 JSON object:先看首 token,
//     再走 token 计深,null/数组/标量/空白/截断都拒绝,对象之后
//     的尾随第二个值也拒绝;`{}` 允许。
//
// 原文透传:全文只读,不重编码、不经 map[string]any 往返,超过
// 2^53 的整数按原文字节保留。
func validateExtraJSON(field string, raw json.RawMessage, max uint64) error {
	switch {
	case bytes.IndexByte(raw, 0x00) >= 0:
		return fmt.Errorf("maa: %s contains a literal NUL byte: %w", field, native.ErrInvalidInput)
	case !utf8.Valid(raw):
		return fmt.Errorf("maa: %s is not valid UTF-8: %w", field, native.ErrInvalidInput)
	case uint64(len(raw)) > max:
		return fmt.Errorf("maa: %s length %d exceeds Limits.InputJSONBytes %d: %w", field, len(raw), max, native.ErrInvalidInput)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 只做形状检查,不取任何值;UseNumber 使数字保持为 json.Number,
	// 不经 float64 往返。
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("maa: %s is not valid JSON (%v): %w", field, err, native.ErrInvalidInput)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("maa: %s must be a JSON object, got %v: %w", field, tok, native.ErrInvalidInput)
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("maa: %s is not a complete JSON object (%v): %w", field, err, native.ErrInvalidInput)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("maa: %s has trailing content after the JSON object: %w", field, native.ErrInvalidInput)
	}
	return nil
}

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
			// StaticOptionKey 的 Invalid=0(设计 §3.0;MAA-03 冻结向量
			// C10:option key 为 0 → ErrInvalidInput,原生调用计数为
			// 0,纯 Go 校验失败不消耗生命周期,运行归属含 MAA-07)。
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
		if err := validateExtraJSON(fmt.Sprintf("ConnectionExtras[%d].JSON", i), extra.JSON, limits.InputJSONBytes); err != nil {
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
// terminated 使用权已终止。M3 的 Close/创建竞争将在此基础上扩展
// closing/closed 状态与创建 lease(绑定设计 §8),本里程碑的状态
// 迁移只有 idle→reserved→ready 与 reserved→terminated。
type lifecycleState uint8

const (
	lifecycleIdle lifecycleState = iota
	lifecycleReserved
	lifecycleReady
	lifecycleTerminated
)

// lifecycleGate 是进程级单 Runtime 生命周期门:串行化全部 Open 的
// 预占,保证并发 Open 至多一个进入原生初始化;记录原生加载开始后
// 的首个失败原因,terminated 之后供后续 Open 诊断。
type lifecycleGate struct {
	mu       sync.Mutex
	state    lifecycleState
	active   *Runtime // 已发布的活动 Runtime;M3 的 Close/创建竞争将经此扩展
	firstErr error    // 原生加载开始后的首个失败原因,terminated 后供后续 Open 诊断
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
		if g.firstErr != nil {
			return fmt.Errorf("maa: runtime lifecycle already terminated (first failure: %v): %w",
				g.firstErr, native.ErrRuntimeTerminated)
		}
		return native.ErrRuntimeTerminated
	default:
		// lifecycleState 只有四个值,其余分支不可达;到达此处意味着
		// 私有状态机的内部不变量被破坏,按编程错误处理,不允许静默
		// 继续污染生命周期。
		panic(fmt.Sprintf("maa: lifecycle gate in unknown state %d", g.state))
	}
}

// terminate 记录原生加载开始后的首个失败原因并进入 terminated 终态。
// 同一道内 terminate 至多被调用一次(任何后续 Open 都在 reserve 处
// 被拒绝),保留首个失败,不覆盖。
func (g *lifecycleGate) terminate(cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.firstErr == nil {
		g.firstErr = cause
	}
	g.state = lifecycleTerminated
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
	// 假会话可返回 nil(本里程碑的用例不触达 raw 调用)。
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
// (绑定设计 §6):GetNullSize 必须返回该值,否则输出缓冲区容量协议
// 无法成立,视为协议错误。核验通过后作为元数据保存,供外部核验。
const profileNullSize = math.MaxUint64

// versionBytesLimit 是 Version 字符串的冻结字节上限(绑定设计 §5.1
// VersionBytes 行:固定 4,096,不提供覆盖字段)。
const versionBytesLimit = 4096

// cString 把 Go 字符串编码为带 NUL 终止的独立 Go buffer,返回首
// 地址与底层 buffer。编码即拷贝:输入是已通过校验(非空、合法
// UTF-8、无内嵌 NUL)的快照内容;buffer 在 raw 调用期间经
// runtime.KeepAlive 保活,调用期间不得被并发修改。空串编码为单个
// NUL 字节,传空 C 字符串而不是 nil。
func cString(s string) (*byte, []byte) {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	return &buf[0], buf
}

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
// 唯一门。执行顺序严格按绑定设计 §8:
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
// M3 的实例创建经它调用 raw 函数),caps/version/nullSize/limits 是
// 初始化时复制的不可变元数据与限制快照。
type Runtime struct {
	table    *native.APITable
	caps     native.Capabilities
	version  string
	nullSize uint64
	limits   Limits
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
