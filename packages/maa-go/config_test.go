// 本文件是 Runtime 配置契约的用例:纯 Go 校验的范围/交叉/预算失败
// 与快照深拷贝。校验发生在触碰生命周期门之前,这里直接对私有的
// snapshotConfig/validateLimits 白盒验证错误分类与字段路径;校验
// 失败不消耗进程生命周期的集成断言见 runtime_test.go。

package maa

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// validLimits 返回一份合法的显式 Limits(整套默认值的副本),供
// 逐字段突变构造失败用例。
func validLimits() Limits { return defaultLimits() }

// validRuntimeConfig 返回一份合法的最小配置,供逐字段突变构造
// 失败用例。
func validRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		LibraryPath:   "/opt/maa/libMaaCore.so",
		UserDir:       "/home/tester/.maa",
		ResourceRoots: []string{"/opt/maa/resource"},
	}
}

// TestConfigValidationRejectsInvalidContent 逐类验证内容校验失败:
// 返回 ErrInvalidInput,错误文本包含字段路径。
func TestConfigValidationRejectsInvalidContent(t *testing.T) {
	nul := "\x00"
	invalidUTF8 := string([]byte{0xff, 0xfe})

	cases := []struct {
		name      string
		mutate    func(cfg *RuntimeConfig)
		wantField string
	}{
		{"LibraryPath 为空", func(c *RuntimeConfig) { c.LibraryPath = "" }, "LibraryPath"},
		{"LibraryPath 相对路径", func(c *RuntimeConfig) { c.LibraryPath = "lib/libMaaCore.so" }, "LibraryPath"},
		{"LibraryPath 内嵌 NUL", func(c *RuntimeConfig) { c.LibraryPath = "/opt/lib" + nul + ".so" }, "LibraryPath"},
		{"LibraryPath 非法 UTF-8", func(c *RuntimeConfig) { c.LibraryPath = "/opt/" + invalidUTF8 + ".so" }, "LibraryPath"},
		{"UserDir 相对路径", func(c *RuntimeConfig) { c.UserDir = "relative/dir" }, "UserDir"},
		{"UserDir 内嵌 NUL", func(c *RuntimeConfig) { c.UserDir = "/ho" + nul + "me" }, "UserDir"},
		{"ResourceRoots 为空", func(c *RuntimeConfig) { c.ResourceRoots = nil }, "ResourceRoots"},
		{"资源根为空串", func(c *RuntimeConfig) { c.ResourceRoots = []string{""} }, "ResourceRoots[0]"},
		{"资源根相对路径", func(c *RuntimeConfig) { c.ResourceRoots = []string{"res"} }, "ResourceRoots[0]"},
		{"资源根内嵌 NUL", func(c *RuntimeConfig) { c.ResourceRoots = []string{"/o" + nul + "pt/res"} }, "ResourceRoots[0]"},
		{"静态选项 key 重复", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 1, Value: "a"}, {Key: 1, Value: "b"}}
		}, "StaticOptions[1]"},
		{"静态选项 value 内嵌 NUL", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 1, Value: "v" + nul}}
		}, "StaticOptions[0].Value"},
		{"静态选项 value 非法 UTF-8", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 1, Value: invalidUTF8}}
		}, "StaticOptions[0].Value"},
		{"extras name 为空", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "", JSON: json.RawMessage(`{}`)}}
		}, "ConnectionExtras[0].Name"},
		{"extras name 重复", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{
				{Name: "n", JSON: json.RawMessage(`{}`)},
				{Name: "n", JSON: json.RawMessage(`{}`)},
			}
		}, "ConnectionExtras[1]"},
		{"extras name 内嵌 NUL", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n" + nul, JSON: json.RawMessage(`{}`)}}
		}, "ConnectionExtras[0].Name"},
		{"extras JSON 为 null", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`null`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 为数组", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`[1,2]`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 为标量字符串", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`"text"`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 为数字", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`42`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 为空白", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`   `)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 为空", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: nil}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 截断", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`{"a":`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 尾随第二个值", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`{} {}`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 尾随垃圾", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`{}}`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 含实际 NUL 字节", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage("{\"a\":\"\x00\"}")}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON 含实际 NUL 字节(键内)", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage("{\"a\x00\":1}")}}
		}, "ConnectionExtras[0].JSON"},
		{"单字符串超 InputStringBytes 预算", func(c *RuntimeConfig) {
			c.UserDir = strings.Repeat("a", 65_537) // 默认 InputStringBytes 65,536
		}, "UserDir"},
		{"extras JSON 超 InputJSONBytes 预算", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{
				Name: "n",
				JSON: json.RawMessage(`{"a":"` + strings.Repeat("x", 1_048_576) + `"}}`), // > 1,048,576
			}}
		}, "ConnectionExtras[0].JSON"},
		{"文本合计超 ConfigBytes 预算", func(c *RuntimeConfig) {
			// 每根恰好 65,536 字节(不超单串预算),65 根合计 4,259,840
			// 超过默认 ConfigBytes 4,194,304。
			c.ResourceRoots = make([]string, 65)
			for i := range c.ResourceRoots {
				c.ResourceRoots[i] = "/" + strings.Repeat("r", 65_535)
			}
		}, "ConfigBytes"},
		{"项数合计超 ConfigEntries 预算", func(c *RuntimeConfig) {
			c.ResourceRoots = make([]string, 257) // 默认 ConfigEntries 256
			for i := range c.ResourceRoots {
				c.ResourceRoots[i] = "/opt/res"
			}
		}, "ConfigEntries"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validRuntimeConfig()
			tc.mutate(&cfg)
			snap, err := snapshotConfig(cfg)
			if len(snap.staticOptions) != 0 || len(snap.resourceRoots) != 0 || len(snap.connectionExtras) != 0 || snap.limits != (Limits{}) {
				t.Fatalf("校验失败不得返回快照, got %+v", snap)
			}
			if err == nil {
				t.Fatal("校验失败必须返回错误")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("错误文本 %q 未包含字段路径 %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestLimitsValidationRejectsOutOfRange 逐字段验证显式 Limits 的
// 范围校验:越界、0 容量、Initial > Max、EarlyResults != 1 都返回
// 带字段路径的 ErrInvalidInput,不把 0 补成默认值。
func TestLimitsValidationRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(l *Limits)
		wantField string
	}{
		{"零值 Limits 整体拒绝", func(l *Limits) { *l = Limits{} }, "Limits.InputStringBytes"},
		{"InputStringBytes 为 0", func(l *Limits) { l.InputStringBytes = 0 }, "Limits.InputStringBytes"},
		{"InputStringBytes 超上限", func(l *Limits) { l.InputStringBytes = 1<<20 + 1 }, "Limits.InputStringBytes"},
		{"InputJSONBytes 为 0", func(l *Limits) { l.InputJSONBytes = 0 }, "Limits.InputJSONBytes"},
		{"InputJSONBytes 超上限", func(l *Limits) { l.InputJSONBytes = 1<<24 + 1 }, "Limits.InputJSONBytes"},
		{"ConfigEntries 为 0", func(l *Limits) { l.ConfigEntries = 0 }, "Limits.ConfigEntries"},
		{"ConfigEntries 超上限", func(l *Limits) { l.ConfigEntries = 4097 }, "Limits.ConfigEntries"},
		{"ConfigBytes 为 0", func(l *Limits) { l.ConfigBytes = 0 }, "Limits.ConfigBytes"},
		{"ConfigBytes 超上限", func(l *Limits) { l.ConfigBytes = 1<<26 + 1 }, "Limits.ConfigBytes"},
		{"ImagePNG.Initial 为 0", func(l *Limits) { l.ImagePNG.Initial = 0 }, "Limits.ImagePNG.Initial"},
		{"ImagePNG.Initial 超上限", func(l *Limits) { l.ImagePNG.Initial = 1<<26 + 1 }, "Limits.ImagePNG.Initial"},
		{"ImagePNG.Max 为 0", func(l *Limits) { l.ImagePNG.Max = 0 }, "Limits.ImagePNG.Max"},
		{"ImagePNG.GrowthRetries 超上限", func(l *Limits) { l.ImagePNG.GrowthRetries = 17 }, "Limits.ImagePNG.GrowthRetries"},
		{"ImagePNG.Initial > Max", func(l *Limits) { l.ImagePNG = BufferLimits{Initial: 100, Max: 50} }, "Limits.ImagePNG.Initial"},
		{"UUID.Initial 为 0", func(l *Limits) { l.UUID.Initial = 0 }, "Limits.UUID.Initial"},
		{"UUID.Max 超上限", func(l *Limits) { l.UUID.Max = 65537 }, "Limits.UUID.Max"},
		{"UUID.GrowthRetries 超上限", func(l *Limits) { l.UUID.GrowthRetries = 17 }, "Limits.UUID.GrowthRetries"},
		{"UUID.Initial > Max", func(l *Limits) { l.UUID = BufferLimits{Initial: 4096, Max: 64} }, "Limits.UUID.Initial"},
		{"Tasks.Initial 为 0", func(l *Limits) { l.Tasks.Initial = 0 }, "Limits.Tasks.Initial"},
		{"Tasks.Max 超上限", func(l *Limits) { l.Tasks.Max = 1<<20 + 1 }, "Limits.Tasks.Max"},
		{"Tasks.GrowthRetries 超上限", func(l *Limits) { l.Tasks.GrowthRetries = 17 }, "Limits.Tasks.GrowthRetries"},
		{"Tasks.Initial > Max", func(l *Limits) { l.Tasks = BufferLimits{Initial: 128, Max: 64} }, "Limits.Tasks.Initial"},
		{"CallbackBytes 为 0", func(l *Limits) { l.CallbackBytes = 0 }, "Limits.CallbackBytes"},
		{"CallbackBytes 超上限", func(l *Limits) { l.CallbackBytes = 1<<24 + 1 }, "Limits.CallbackBytes"},
		{"InternalEvents 为 0", func(l *Limits) { l.InternalEvents = 0 }, "Limits.InternalEvents"},
		{"InternalEvents 超上限", func(l *Limits) { l.InternalEvents = 65537 }, "Limits.InternalEvents"},
		{"InternalBytes 为 0", func(l *Limits) { l.InternalBytes = 0 }, "Limits.InternalBytes"},
		{"InternalBytes 超上限", func(l *Limits) { l.InternalBytes = 1<<27 + 1 }, "Limits.InternalBytes"},
		{"PublicEvents 为 0", func(l *Limits) { l.PublicEvents = 0 }, "Limits.PublicEvents"},
		{"PublicEvents 超上限", func(l *Limits) { l.PublicEvents = 65537 }, "Limits.PublicEvents"},
		{"PublicBytes 为 0", func(l *Limits) { l.PublicBytes = 0 }, "Limits.PublicBytes"},
		{"PublicBytes 超上限", func(l *Limits) { l.PublicBytes = 1<<27 + 1 }, "Limits.PublicBytes"},
		{"PendingOperations 为 0", func(l *Limits) { l.PendingOperations = 0 }, "Limits.PendingOperations"},
		{"PendingOperations 超上限", func(l *Limits) { l.PendingOperations = 4097 }, "Limits.PendingOperations"},
		{"EarlyResults 为 0", func(l *Limits) { l.EarlyResults = 0 }, "Limits.EarlyResults"},
		{"EarlyResults 非 1", func(l *Limits) { l.EarlyResults = 2 }, "Limits.EarlyResults"},
		{"EarlyBytes 为 0", func(l *Limits) { l.EarlyBytes = 0 }, "Limits.EarlyBytes"},
		{"EarlyBytes 超上限", func(l *Limits) { l.EarlyBytes = 1<<24 + 1 }, "Limits.EarlyBytes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := validLimits()
			tc.mutate(&l)
			err := validateLimits(l)
			if err == nil {
				t.Fatal("越界配置必须返回错误")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("错误文本 %q 未包含字段路径 %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestLimitsValidationRejectsCrossFieldConflicts 验证交叉字段校验:
// 单字段均合法但相互矛盾时拒绝。
func TestLimitsValidationRejectsCrossFieldConflicts(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(l *Limits)
		wantSubstr string
	}{
		{"InputStringBytes 超过 ConfigBytes", func(l *Limits) {
			l.InputStringBytes = 1 << 20
			l.ConfigBytes = 1 << 16
		}, "Limits.InputStringBytes"},
		{"InputJSONBytes 超过 ConfigBytes", func(l *Limits) {
			l.InputJSONBytes = 1 << 24
			l.ConfigBytes = 1 << 20
		}, "Limits.InputJSONBytes"},
		{"CallbackBytes 超过 InternalBytes", func(l *Limits) {
			l.CallbackBytes = 1 << 24
			l.InternalBytes = 1 << 20
		}, "Limits.CallbackBytes"},
		{"CallbackBytes 超过 PublicBytes", func(l *Limits) {
			l.CallbackBytes = 1 << 24
			l.PublicBytes = 1 << 20
		}, "Limits.CallbackBytes"},
		{"CallbackBytes 超过 EarlyBytes", func(l *Limits) {
			l.CallbackBytes = 1 << 24
			l.EarlyBytes = 1 << 20
		}, "Limits.CallbackBytes"},
		{"事件字节总和超过 268435456", func(l *Limits) {
			// 各字段均在其范围内且 CallbackBytes 不超过任何一方,但
			// InternalBytes + PublicBytes + EarlyBytes 恰好越界
			// (恰好达到上限允许,再加入即失败)。
			l.InternalBytes = 1 << 27
			l.PublicBytes = 1 << 27
			l.EarlyBytes = 1 << 20
		}, "exceeds 268435456"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := validLimits()
			tc.mutate(&l)
			err := validateLimits(l)
			if err == nil {
				t.Fatal("交叉矛盾配置必须返回错误")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("错误文本 %q 未包含 %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestLimitsValidationAcceptsBoundaries 验证恰好达到各上限(含交叉
// 总和恰好等于 268,435,456)与 GrowthRetries 边界值允许通过。
func TestLimitsValidationAcceptsBoundaries(t *testing.T) {
	l := Limits{
		InputStringBytes:  1 << 20,
		InputJSONBytes:    1 << 24,
		ConfigEntries:     1 << 12,
		ConfigBytes:       1 << 26,
		ImagePNG:          BufferLimits{Initial: 1 << 26, Max: 1 << 26, GrowthRetries: 0},
		UUID:              BufferLimits{Initial: 1 << 16, Max: 1 << 16, GrowthRetries: 16},
		Tasks:             BufferLimits{Initial: 1 << 20, Max: 1 << 20, GrowthRetries: 16},
		CallbackBytes:     1 << 24,
		InternalEvents:    1 << 16,
		InternalBytes:     1 << 27,
		PublicEvents:      1 << 16,
		PublicBytes:       117_440_512, // 与 InternalBytes、EarlyBytes 合计恰为 268,435,456
		PendingOperations: 1 << 12,
		EarlyResults:      1,
		EarlyBytes:        1 << 24,
	}
	if err := validateLimits(l); err != nil {
		t.Fatalf("边界值必须允许: %v", err)
	}
}

// TestConnectionExtraJSONAcceptsObjectForms 验证合法 extras JSON 被
// 接受且原文逐字节保留:{} 允许、\u0000 转义合法并原样保留、大
// 整数不经 float64 往返、尾随空白允许。
func TestConnectionExtraJSONAcceptsObjectForms(t *testing.T) {
	raws := []string{
		`{}`,
		`{"a":"\u0000","big":9007199254740993}`,
		`{"nested":{"arr":[1,{"b":2}]},"esc":"line\nbreak"}`,
		`{"a":1}   `,
	}
	for _, raw := range raws {
		cfg := validRuntimeConfig()
		cfg.ConnectionExtras = []ConnectionExtra{{Name: "sink", JSON: json.RawMessage(raw)}}
		snap, err := snapshotConfig(cfg)
		if err != nil {
			t.Fatalf("extras %q 应当被接受: %v", raw, err)
		}
		if got := string(snap.connectionExtras[0].JSON); got != raw {
			t.Errorf("extras 原文被改写: got %q, want %q", got, raw)
		}
	}
}

// TestSnapshotDeepCopiesConfig 验证快照深拷贝:snapshotConfig 返回
// 后修改原配置的 slice 元素、RawMessage 底层字节与追加,不影响快照。
func TestSnapshotDeepCopiesConfig(t *testing.T) {
	cfg := validRuntimeConfig()
	cfg.StaticOptions = []StaticOption{{Key: 3, Value: "orig"}}
	cfg.ResourceRoots = []string{"/opt/maa/resource"}
	cfg.ConnectionExtras = []ConnectionExtra{{Name: "sink", JSON: json.RawMessage(`{"orig":1}`)}}
	explicit := validLimits()
	explicit.InputStringBytes = 12_345
	cfg.Limits = &explicit
	// wantLimits 是显式 Limits 的值复制,作为期望值;cfg.Limits 与
	// explicit 共享同一变量,下面的原地修改会改写 explicit 但不得
	// 改写快照。
	wantLimits := explicit

	snap, err := snapshotConfig(cfg)
	if err != nil {
		t.Fatalf("合法配置不得校验失败: %v", err)
	}

	// Open 返回后修改原配置的一切可变内容。
	cfg.StaticOptions[0].Value = "tampered"
	cfg.StaticOptions = append(cfg.StaticOptions, StaticOption{Key: 9, Value: "extra"})
	cfg.ResourceRoots[0] = "/tampered"
	cfg.ResourceRoots = append(cfg.ResourceRoots, "/tampered2")
	cfg.ConnectionExtras[0].Name = "tampered"
	cfg.ConnectionExtras[0].JSON[0] = 'X'
	cfg.ConnectionExtras = append(cfg.ConnectionExtras, ConnectionExtra{Name: "t2", JSON: json.RawMessage(`{}`)})
	cfg.Limits.InputStringBytes = 1

	if snap.staticOptions[0].Value != "orig" || len(snap.staticOptions) != 1 {
		t.Errorf("静态选项快照被改写: %+v", snap.staticOptions)
	}
	if snap.resourceRoots[0] != "/opt/maa/resource" || len(snap.resourceRoots) != 1 {
		t.Errorf("资源根快照被改写: %v", snap.resourceRoots)
	}
	if snap.connectionExtras[0].Name != "sink" || len(snap.connectionExtras) != 1 {
		t.Fatalf("extras 快照被改写: %+v", snap.connectionExtras)
	}
	if got := string(snap.connectionExtras[0].JSON); got != `{"orig":1}` {
		t.Errorf("extras JSON 快照被改写: %q", got)
	}
	if snap.limits != wantLimits || snap.limits.InputStringBytes != 12_345 {
		t.Errorf("Limits 快照被改写: got %+v, want %+v", snap.limits, wantLimits)
	}
}

// TestAddCheckedDetectsOverflow 直接验证检查溢出的加法:普通加法
// 正确,溢出返回 false 且不产出伪造的部分和。
func TestAddCheckedDetectsOverflow(t *testing.T) {
	if got, ok := addChecked(1, 2); !ok || got != 3 {
		t.Errorf("addChecked(1, 2) = (%d, %v), want (3, true)", got, ok)
	}
	if got, ok := addChecked(0, math.MaxUint64); !ok || got != math.MaxUint64 {
		t.Errorf("addChecked(0, MaxUint64) = (%d, %v), want (MaxUint64, true)", got, ok)
	}
	if got, ok := addChecked(math.MaxUint64, 0); !ok || got != math.MaxUint64 {
		t.Errorf("addChecked(MaxUint64, 0) = (%d, %v), want (MaxUint64, true)", got, ok)
	}
	if _, ok := addChecked(math.MaxUint64, 1); ok {
		t.Error("addChecked(MaxUint64, 1) 必须检出溢出")
	}
	if _, ok := addChecked(1, math.MaxUint64); ok {
		t.Error("addChecked(1, MaxUint64) 必须检出溢出")
	}
	if got, ok := addChecked(math.MaxUint64, math.MaxUint64); ok || got != 0 {
		t.Errorf("addChecked(MaxUint64, MaxUint64) = (%d, %v), want (0, false)", got, ok)
	}
}

// 编译期锁定:配置校验失败的哨兵必须与公开错误面同一错误值。
var _ error = native.ErrInvalidInput
