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

// StaticOption 是一条进程级静态选项:key 是 int32 的 StaticOptionKey
// (Invalid=0、CpuOCR=1、GpuOCR=2);key 为 0 在纯 Go 校验阶段拒绝并返回
// ErrInvalidInput,未识别的非零 key 原样透传,不把业务选项封闭为枚举。
// value 允许空串,key 不得重复。
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

// Limits 汇集 Open 与后续实例输入的全部容量限制。Limits == nil 时使用
// 整套默认值;非 nil 表示完整显式配置,不逐字段把 0 补成默认值。容量/计数
// 以 uint64、扩容次数以 uint32 表达,在任何转换/乘加/分配之前直接按 uint64
// 检查范围。
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
	// 总字节上限。
	InternalEvents uint64
	InternalBytes  uint64
	// PublicEvents/PublicBytes 是公开 Events 缓冲的事件数与 JSON
	// 总字节上限,按未确认接收的事件计入。
	PublicEvents uint64
	PublicBytes  uint64
	// PendingOperations 是未结算 Operation 数上限,含门禁前提交预留。
	PendingOperations uint64
	// EarlyResults 是 raw 返回 ID 前的早到完成数上限,固定 1,其他值
	// 一律拒绝。
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

const (
	inputStringBytesMax = 1 << 20
	inputJSONBytesMax   = 1 << 24
	configEntriesMax    = 1 << 12
	configBytesMax      = 1 << 26
	imagePNGCapMax      = 1 << 26
	imageBGRCapMax      = 1 << 27
	uuidCapMax          = 1 << 16
	tasksCapMax         = 1 << 20
	callbackBytesMax    = 1 << 24
	eventsCountMax      = 1 << 16
	internalBytesMax    = 1 << 27
	pendingOpsMax       = 1 << 12
	earlyBytesMax       = 1 << 24
	eventBytesTotalMax  = 1 << 28
	growthRetriesMax    = 16
)

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
		return fmt.Errorf("maa: Limits.EarlyResults must be 1 (fixed), got %d: %w", l.EarlyResults, native.ErrInvalidInput)
	case l.EarlyBytes < 1 || l.EarlyBytes > earlyBytesMax:
		return outOfRange("EarlyBytes", 1, earlyBytesMax)
	}

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

type configSnapshot struct {
	libraryPath      string
	userDir          string
	staticOptions    []StaticOption
	resourceRoots    []string
	connectionExtras []ConnectionExtra
	limits           Limits
}

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
		snap.connectionExtras[i] = ConnectionExtra{Name: extra.Name, JSON: bytes.Clone(extra.JSON)}
	}
	return snap, nil
}

type lifecycleState uint8

const (
	lifecycleIdle lifecycleState = iota
	lifecycleReserved
	lifecycleReady
	lifecycleTerminated
)

type instanceSlotState uint8

const (
	slotNone instanceSlotState = iota
	slotCreating
	slotActive
)

type lifecycleGate struct {
	mu       sync.Mutex
	state    lifecycleState
	active   *Runtime
	firstErr error

	slot               instanceSlotState
	creationDone       chan struct{}
	creationDoneClosed bool
	closeRequested     bool
	activeInstance     *instance
}

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
		panic(fmt.Sprintf("maa: lifecycle gate in unknown state %d", g.state))
	}
}

func (g *lifecycleGate) terminatedErrLocked() error {
	if g.firstErr != nil {
		return fmt.Errorf("maa: runtime lifecycle already terminated (first failure: %v): %w",
			g.firstErr, native.ErrRuntimeTerminated)
	}
	return native.ErrRuntimeTerminated
}

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
		return err
	}
	g.slot = slotActive
	g.activeInstance = in
	return nil
}

func (g *lifecycleGate) releaseCreationLease() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.creationDone == nil || g.creationDoneClosed {
		panic("maa: creation lease released without a reservation or more than once")
	}
	g.creationDoneClosed = true
	close(g.creationDone)
}

func (g *lifecycleGate) waitCreationLease() {
	g.mu.Lock()
	ch := g.creationDone
	g.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

func (g *lifecycleGate) beginRuntimeClose() (*instance, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeRequested = true
	return g.activeInstance, g.creationDone
}

func (g *lifecycleGate) runtimeCloseResult() (error, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != lifecycleTerminated {
		return nil, false
	}
	return g.firstErr, true
}

func (g *lifecycleGate) closeCreationAdmission() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closeRequested {
		return false
	}
	g.closeRequested = true
	return true
}

func (g *lifecycleGate) terminate(cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.firstErr == nil {
		g.firstErr = cause
	}
	g.state = lifecycleTerminated
	g.activeInstance = nil
}

func (g *lifecycleGate) publish(rt *Runtime) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != lifecycleReserved {
		panic(fmt.Sprintf("maa: lifecycle gate publish from state %d", g.state))
	}
	g.state = lifecycleReady
	g.active = rt
}

type initSession interface {
	capabilities() native.Capabilities
	table() *native.APITable
	version() *byte
	nullSize() uint64
	setUserDir(path *byte) uint8
	setStaticOption(key int32, value *byte) uint8
	loadResource(path *byte) uint8
	setConnectionExtras(name *byte, extras *byte)
}

type initEnv interface {
	loadLibrary(path string) (initSession, error)
}

type nativeEnv struct{}

var _ initEnv = nativeEnv{}

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

const profileNullSize = math.MaxUint64

const versionBytesLimit = 4096

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

	version, err := readVersion(sess.version())
	if err != nil {
		gate.terminate(err)
		return nil, err
	}
	ns := sess.nullSize()
	if ns != profileNullSize {
		err := fmt.Errorf("maa: GetNullSize returned %#x, want the ABI sentinel (%#x): %w",
			ns, uint64(profileNullSize), native.ErrProtocol)
		gate.terminate(err)
		return nil, err
	}
	dir, dirBuf := cString(snap.userDir)
	ret := sess.setUserDir(dir)
	runtime.KeepAlive(dirBuf)
	if ret == 0 {
		err := fmt.Errorf("maa: SetUserDir(%q) returned failure: %w", snap.userDir, native.ErrNativeRejected)
		gate.terminate(err)
		return nil, err
	}
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

var processLifecycle lifecycleGate

// Open 执行单进程初始化事务:校验并复制配置、预占进程级单 Runtime
// 生命周期,再完成原生加载与初始化。全部成功才返回 *Runtime;任何初始化
// 失败都不发布公开面可获得的半成品对象。
//
// 进程级契约(详见包文档):
//   - 并发或重复 Open:已有预占或活动 Runtime 时返回 ErrRuntimeInUse,
//     不复用已有对象、也不忽略新配置;
//   - 原生加载开始后的失败:该进程进入 terminated 使用状态,后续 Open 返回
//     包装 ErrRuntimeTerminated 的错误;失败本身以原始错误返回,保留结构化
//     错误类型与错误链;
//   - 配置校验失败:发生在预占之前,返回带字段路径的 ErrInvalidInput,
//     不消耗进程生命周期,修正输入后仍可进行首次原生加载;
//   - 配置快照:校验通过后深拷贝冻结,Open 返回后修改原配置不影响已初始化
//     Runtime;调用者在 Open 返回前不得并发修改输入。
func Open(cfg RuntimeConfig) (*Runtime, error) {
	return open(cfg, nativeEnv{}, &processLifecycle)
}

// Capabilities 是 internal/native 只读能力快照的类型别名:可选符号
// (AsstCreate、AsstGetImageBgr、AsstAsyncAttachWindow)缺失只关闭对应
// 能力,不阻塞初始化。
type Capabilities = native.Capabilities

// Runtime 表示一次成功初始化的进程级 MaaCore 绑定。全部字段未导出且在
// 初始化时冻结,发布后只读;零值不可用,只能经 Open 返回的指针使用,不按值
// 复制。
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

// NullSize 返回初始化时核验并保存的 NullSize 哨兵值(输出缓冲区协议
// 约定为 math.MaxUint64)。只读不可变元数据,不触发任何原生调用;
// 并发安全。
func (rt *Runtime) NullSize() uint64 { return rt.nullSize }

// NewInstance 创建一个带 callback 的实例。全部步骤成功才返回 *Instance;
// 任何失败都不向调用者返回半初始化实例,也不返回尚可重试的假象。
//
// 错误契约:
//   - 预占前纯 Go 配置校验失败:返回带字段路径的 ErrInvalidInput,不消耗
//     Runtime,不改名额与生命周期;
//   - 预留被拒:Runtime 已终止使用返回包装 ErrRuntimeTerminated 的错误;
//     名额已被占用返回包装 ErrRuntimeInUse 的错误;关闭准入已关闭返回包装
//     ErrClosing 的错误;
//   - 预占后失败(CreateEx=0、选项被原生拒绝、早到故障、发布被 closing
//     抢先):统一清理并终止 Runtime 使用,后续 Open/NewInstance 返回包装
//     ErrRuntimeTerminated 的错误,不允许同进程重建实例或重试资源组合。
//
// 配置快照:校验通过后、预留名额前深拷贝冻结,方法返回后修改原配置不影响
// 实例;调用者在方法返回前不得并发修改输入。
func (rt *Runtime) NewInstance(cfg InstanceConfig) (*Instance, error) {
	return createInstance(rt, cfg,
		apiTableCreator{table: rt.table},
		apiTableCloser{table: rt.table},
		apiTableCaller{table: rt.table})
}

// Close 同步完成本 Runtime 绑定使用的关闭:幂等,不卸载原生库,不重置
// 进程级状态——恢复仍只能来自退出并启动新 worker 进程。Close 关闭创建
// 准入并等待进行中的创建事务收尾(未发布实例由创建者完成回滚,Close 不
// 重复 Destroy),再把已发布的活动实例交给唯一关闭者,最后终止进程级使用
// 状态。
//
// 多次或并发的 Runtime.Close 与 Instance.Close 收到同一稳定最终结果;
// 终止后的重复 Close 直接返回已存储结果,不再进入 native。Close 不接受
// 强制回收超时,也不承诺在 native 卡住时返回(由 control 对 worker 执行
// 有期限的退出监督)。
func (rt *Runtime) Close() error {
	active, lease := rt.gate.beginRuntimeClose()
	if lease != nil {
		<-lease
	}
	if err, terminated := rt.gate.runtimeCloseResult(); terminated {
		return err
	}
	var err error
	if active != nil {
		err = active.close()
	}
	rt.gate.terminate(err)
	return err
}
