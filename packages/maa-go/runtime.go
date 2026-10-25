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
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// StaticOption 是一条进程级静态选项:key 为 int32(StaticOptionKey
// 语义,锁定值见绑定设计 §3.0:Invalid=0、CpuOCR=1、GpuOCR=2;
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

// validateStringField 校验单个字符串输入字段:非空、合法 UTF-8、
// 无内嵌 NUL,且长度不超过单字符串预算。field 是错误文本中的字段
// 路径(如 LibraryPath、StaticOptions[0].Value)。
func validateStringField(field, value string, max uint64) error {
	switch {
	case value == "":
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

// validatePathField 校验单个路径输入字段:在字符串校验之上追加
// 绝对路径要求。路径不做 shell 展开、不做存在性检查——文件是否
// 存在、原生是否接受,留给加载/调用阶段失败。
func validatePathField(field, value string, max uint64) error {
	if err := validateStringField(field, value, max); err != nil {
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
		if seenKey[opt.Key] {
			return fmt.Errorf("maa: StaticOptions[%d] duplicates key %d: %w", i, opt.Key, native.ErrInvalidInput)
		}
		seenKey[opt.Key] = true
		if err := validateStringField(fmt.Sprintf("StaticOptions[%d].Value", i), opt.Value, limits.InputStringBytes); err != nil {
			return err
		}
	}
	seenName := make(map[string]bool, len(cfg.ConnectionExtras))
	for i, extra := range cfg.ConnectionExtras {
		if err := validateStringField(fmt.Sprintf("ConnectionExtras[%d].Name", i), extra.Name, limits.InputStringBytes); err != nil {
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
