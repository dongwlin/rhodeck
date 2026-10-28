// 本文件是 Runtime 配置契约的用例:纯 Go 校验的范围/交叉/预算失败
// 与快照深拷贝。校验发生在触碰生命周期门之前,这里直接对私有的
// snapshotConfig/validateLimits 白盒验证错误分类与字段路径;校验
// 失败不消耗进程生命周期的集成断言见 runtime_test.go。

package maa

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// validLimits 返回一份合法的显式 Limits(整套默认值的副本),供
// 逐字段突变构造失败用例。
func validLimits() Limits { return defaultLimits() }

// testAbsolutePath 返回当前测试目录下的绝对路径。fake native 不要求
// 文件存在,路径仍使用宿主平台的卷名与分隔符以覆盖真实的路径校验。
func testAbsolutePath(parts ...string) string {
	path, err := filepath.Abs(filepath.Join(append([]string{"testdata", "paths"}, parts...)...))
	if err != nil {
		panic(err)
	}
	return path
}

// testAbsolutePathWithLength 构造恰好 n 字节的绝对路径,供预算边界
// 用例使用。Windows 保留卷名前缀,不将无卷名的 / 当作绝对路径。
func testAbsolutePathWithLength(n int) string {
	root := filepath.VolumeName(testAbsolutePath()) + string(filepath.Separator)
	if n < len(root) {
		panic("test absolute path budget is shorter than its root")
	}
	return root + strings.Repeat("r", n-len(root))
}

// validRuntimeConfig 返回一份合法的最小配置,供逐字段突变构造
// 失败用例。
func validRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		LibraryPath:   testAbsolutePath("libMaaCore.so"),
		UserDir:       testAbsolutePath("user"),
		ResourceRoots: []string{testAbsolutePath("resource")},
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
		{"LibraryPath is empty", func(c *RuntimeConfig) { c.LibraryPath = "" }, "LibraryPath"},
		{"LibraryPath relative path", func(c *RuntimeConfig) { c.LibraryPath = "lib/libMaaCore.so" }, "LibraryPath"},
		{"LibraryPath embedded NUL", func(c *RuntimeConfig) { c.LibraryPath += nul }, "LibraryPath"},
		{"LibraryPath invalid UTF-8", func(c *RuntimeConfig) { c.LibraryPath += invalidUTF8 }, "LibraryPath"},
		{"UserDir relative path", func(c *RuntimeConfig) { c.UserDir = "relative/dir" }, "UserDir"},
		{"UserDir embedded NUL", func(c *RuntimeConfig) { c.UserDir += nul }, "UserDir"},
		{"ResourceRoots is empty", func(c *RuntimeConfig) { c.ResourceRoots = nil }, "ResourceRoots"},
		{"resource root is an empty string", func(c *RuntimeConfig) { c.ResourceRoots = []string{""} }, "ResourceRoots[0]"},
		{"resource root relative path", func(c *RuntimeConfig) { c.ResourceRoots = []string{"res"} }, "ResourceRoots[0]"},
		{"resource root embedded NUL", func(c *RuntimeConfig) { c.ResourceRoots = []string{testAbsolutePath("resource") + nul} }, "ResourceRoots[0]"},
		{"static option key duplicated", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 1, Value: "a"}, {Key: 1, Value: "b"}}
		}, "StaticOptions[1]"},
		{"static option key is 0", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 0, Value: "v"}}
		}, "StaticOptions[0].Key"},
		{"static option value embedded NUL", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 1, Value: "v" + nul}}
		}, "StaticOptions[0].Value"},
		{"static option value invalid UTF-8", func(c *RuntimeConfig) {
			c.StaticOptions = []StaticOption{{Key: 1, Value: invalidUTF8}}
		}, "StaticOptions[0].Value"},
		{"extras name is empty", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "", JSON: json.RawMessage(`{}`)}}
		}, "ConnectionExtras[0].Name"},
		{"extras name duplicated", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{
				{Name: "n", JSON: json.RawMessage(`{}`)},
				{Name: "n", JSON: json.RawMessage(`{}`)},
			}
		}, "ConnectionExtras[1]"},
		{"extras name embedded NUL", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n" + nul, JSON: json.RawMessage(`{}`)}}
		}, "ConnectionExtras[0].Name"},
		{"extras JSON is null", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`null`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON is an array", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`[1,2]`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON is a scalar string", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`"text"`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON is a number", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`42`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON is whitespace", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`   `)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON is empty", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: nil}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON truncated", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`{"a":`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON trailing second value", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`{} {}`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON trailing garbage", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage(`{}}`)}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON contains a literal NUL byte", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage("{\"a\":\"\x00\"}")}}
		}, "ConnectionExtras[0].JSON"},
		{"extras JSON contains a literal NUL byte (in a key)", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{Name: "n", JSON: json.RawMessage("{\"a\x00\":1}")}}
		}, "ConnectionExtras[0].JSON"},
		{"single string exceeds the InputStringBytes budget", func(c *RuntimeConfig) {
			c.UserDir = strings.Repeat("a", 65_537) // 默认 InputStringBytes 65,536
		}, "UserDir"},
		{"extras JSON exceeds the InputJSONBytes budget", func(c *RuntimeConfig) {
			c.ConnectionExtras = []ConnectionExtra{{
				Name: "n",
				JSON: json.RawMessage(`{"a":"` + strings.Repeat("x", 1_048_576) + `"}}`), // > 1,048,576
			}}
		}, "ConnectionExtras[0].JSON"},
		{"text total exceeds the ConfigBytes budget", func(c *RuntimeConfig) {
			// 每根恰好 65,536 字节(不超单串预算),65 根合计 4,259,840
			// 超过默认 ConfigBytes 4,194,304。
			c.ResourceRoots = make([]string, 65)
			for i := range c.ResourceRoots {
				c.ResourceRoots[i] = testAbsolutePathWithLength(65_536)
			}
		}, "ConfigBytes"},
		{"entry count exceeds the ConfigEntries budget", func(c *RuntimeConfig) {
			c.ResourceRoots = make([]string, 257) // 默认 ConfigEntries 256
			for i := range c.ResourceRoots {
				c.ResourceRoots[i] = testAbsolutePath("resource")
			}
		}, "ConfigEntries"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validRuntimeConfig()
			tc.mutate(&cfg)
			snap, err := snapshotConfig(cfg)
			if len(snap.staticOptions) != 0 || len(snap.resourceRoots) != 0 || len(snap.connectionExtras) != 0 || snap.limits != (Limits{}) {
				t.Fatalf("a failed validation must not return a snapshot, got %+v", snap)
			}
			if err == nil {
				t.Fatal("a failed validation must return an error")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error text %q does not contain field path %q", err.Error(), tc.wantField)
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
		{"zero-value Limits rejected as a whole", func(l *Limits) { *l = Limits{} }, "Limits.InputStringBytes"},
		{"InputStringBytes is 0", func(l *Limits) { l.InputStringBytes = 0 }, "Limits.InputStringBytes"},
		{"InputStringBytes above the upper bound", func(l *Limits) { l.InputStringBytes = 1<<20 + 1 }, "Limits.InputStringBytes"},
		{"InputJSONBytes is 0", func(l *Limits) { l.InputJSONBytes = 0 }, "Limits.InputJSONBytes"},
		{"InputJSONBytes above the upper bound", func(l *Limits) { l.InputJSONBytes = 1<<24 + 1 }, "Limits.InputJSONBytes"},
		{"ConfigEntries is 0", func(l *Limits) { l.ConfigEntries = 0 }, "Limits.ConfigEntries"},
		{"ConfigEntries above the upper bound", func(l *Limits) { l.ConfigEntries = 4097 }, "Limits.ConfigEntries"},
		{"ConfigBytes is 0", func(l *Limits) { l.ConfigBytes = 0 }, "Limits.ConfigBytes"},
		{"ConfigBytes above the upper bound", func(l *Limits) { l.ConfigBytes = 1<<26 + 1 }, "Limits.ConfigBytes"},
		{"ImagePNG.Initial is 0", func(l *Limits) { l.ImagePNG.Initial = 0 }, "Limits.ImagePNG.Initial"},
		{"ImagePNG.Initial above the upper bound", func(l *Limits) { l.ImagePNG.Initial = 1<<26 + 1 }, "Limits.ImagePNG.Initial"},
		{"ImagePNG.Max is 0", func(l *Limits) { l.ImagePNG.Max = 0 }, "Limits.ImagePNG.Max"},
		{"ImagePNG.GrowthRetries above the upper bound", func(l *Limits) { l.ImagePNG.GrowthRetries = 17 }, "Limits.ImagePNG.GrowthRetries"},
		{"ImagePNG.Initial > Max", func(l *Limits) { l.ImagePNG = BufferLimits{Initial: 100, Max: 50} }, "Limits.ImagePNG.Initial"},
		{"UUID.Initial is 0", func(l *Limits) { l.UUID.Initial = 0 }, "Limits.UUID.Initial"},
		{"UUID.Max above the upper bound", func(l *Limits) { l.UUID.Max = 65537 }, "Limits.UUID.Max"},
		{"UUID.GrowthRetries above the upper bound", func(l *Limits) { l.UUID.GrowthRetries = 17 }, "Limits.UUID.GrowthRetries"},
		{"UUID.Initial > Max", func(l *Limits) { l.UUID = BufferLimits{Initial: 4096, Max: 64} }, "Limits.UUID.Initial"},
		{"Tasks.Initial is 0", func(l *Limits) { l.Tasks.Initial = 0 }, "Limits.Tasks.Initial"},
		{"Tasks.Max above the upper bound", func(l *Limits) { l.Tasks.Max = 1<<20 + 1 }, "Limits.Tasks.Max"},
		{"Tasks.GrowthRetries above the upper bound", func(l *Limits) { l.Tasks.GrowthRetries = 17 }, "Limits.Tasks.GrowthRetries"},
		{"Tasks.Initial > Max", func(l *Limits) { l.Tasks = BufferLimits{Initial: 128, Max: 64} }, "Limits.Tasks.Initial"},
		{"CallbackBytes is 0", func(l *Limits) { l.CallbackBytes = 0 }, "Limits.CallbackBytes"},
		{"CallbackBytes above the upper bound", func(l *Limits) { l.CallbackBytes = 1<<24 + 1 }, "Limits.CallbackBytes"},
		{"InternalEvents is 0", func(l *Limits) { l.InternalEvents = 0 }, "Limits.InternalEvents"},
		{"InternalEvents above the upper bound", func(l *Limits) { l.InternalEvents = 65537 }, "Limits.InternalEvents"},
		{"InternalBytes is 0", func(l *Limits) { l.InternalBytes = 0 }, "Limits.InternalBytes"},
		{"InternalBytes above the upper bound", func(l *Limits) { l.InternalBytes = 1<<27 + 1 }, "Limits.InternalBytes"},
		{"PublicEvents is 0", func(l *Limits) { l.PublicEvents = 0 }, "Limits.PublicEvents"},
		{"PublicEvents above the upper bound", func(l *Limits) { l.PublicEvents = 65537 }, "Limits.PublicEvents"},
		{"PublicBytes is 0", func(l *Limits) { l.PublicBytes = 0 }, "Limits.PublicBytes"},
		{"PublicBytes above the upper bound", func(l *Limits) { l.PublicBytes = 1<<27 + 1 }, "Limits.PublicBytes"},
		{"PendingOperations is 0", func(l *Limits) { l.PendingOperations = 0 }, "Limits.PendingOperations"},
		{"PendingOperations above the upper bound", func(l *Limits) { l.PendingOperations = 4097 }, "Limits.PendingOperations"},
		{"EarlyResults is 0", func(l *Limits) { l.EarlyResults = 0 }, "Limits.EarlyResults"},
		{"EarlyResults is not 1", func(l *Limits) { l.EarlyResults = 2 }, "Limits.EarlyResults"},
		{"EarlyBytes is 0", func(l *Limits) { l.EarlyBytes = 0 }, "Limits.EarlyBytes"},
		{"EarlyBytes above the upper bound", func(l *Limits) { l.EarlyBytes = 1<<24 + 1 }, "Limits.EarlyBytes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := validLimits()
			tc.mutate(&l)
			err := validateLimits(l)
			if err == nil {
				t.Fatal("an out-of-range configuration must return an error")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error text %q does not contain field path %q", err.Error(), tc.wantField)
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
		{"InputStringBytes exceeds ConfigBytes", func(l *Limits) {
			l.InputStringBytes = 1 << 20
			l.ConfigBytes = 1 << 16
		}, "Limits.InputStringBytes"},
		{"InputJSONBytes exceeds ConfigBytes", func(l *Limits) {
			l.InputJSONBytes = 1 << 24
			l.ConfigBytes = 1 << 20
		}, "Limits.InputJSONBytes"},
		{"CallbackBytes exceeds InternalBytes", func(l *Limits) {
			l.CallbackBytes = 1 << 24
			l.InternalBytes = 1 << 20
		}, "Limits.CallbackBytes"},
		{"CallbackBytes exceeds PublicBytes", func(l *Limits) {
			l.CallbackBytes = 1 << 24
			l.PublicBytes = 1 << 20
		}, "Limits.CallbackBytes"},
		{"CallbackBytes exceeds EarlyBytes", func(l *Limits) {
			l.CallbackBytes = 1 << 24
			l.EarlyBytes = 1 << 20
		}, "Limits.CallbackBytes"},
		{"event bytes total exceeds 268435456", func(l *Limits) {
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
				t.Fatal("a cross-field contradictory configuration must return an error")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error text %q does not contain %q", err.Error(), tc.wantSubstr)
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
		ImageBGR:          BufferLimits{Initial: 1 << 27, Max: 1 << 27, GrowthRetries: 16},
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
		t.Fatalf("boundary values must be allowed: %v", err)
	}
}

// TestConfigEntriesExactUpperBoundAccepted 验证配置项数的上界:
// 合计项数恰为 ConfigEntries(默认 256)被接受,再多一项拒绝。
// 固定字段若被误计入项数,恰达
// 上界的配置将变成 258 项而被拒,故接受侧同时钉死「固定字段与
// Limits 自身不计入项数」。
func TestConfigEntriesExactUpperBoundAccepted(t *testing.T) {
	cfg := validRuntimeConfig()
	cfg.ResourceRoots = make([]string, defaultLimits().ConfigEntries)
	for i := range cfg.ResourceRoots {
		cfg.ResourceRoots[i] = testAbsolutePath("resource")
	}
	snap, err := snapshotConfig(cfg)
	if err != nil {
		t.Fatalf("a configuration with an entry count of exactly ConfigEntries should be accepted: %v", err)
	}
	if len(snap.resourceRoots) != len(cfg.ResourceRoots) {
		t.Fatalf("snapshot resource root count = %d, want %d", len(snap.resourceRoots), len(cfg.ResourceRoots))
	}

	// 多一项即拒绝,与恰达上界构成边界对。
	cfg.ResourceRoots = append(cfg.ResourceRoots, testAbsolutePath("resource"))
	if _, err := snapshotConfig(cfg); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("an entry count one over ConfigEntries must be rejected, got %v", err)
	}
}

// TestConfigBytesExactUpperBoundAccepted 验证配置总字节数的上界:
// 全部文本字节合计(含固定路径)恰为 ConfigBytes(默认 4,194,304)
// 被接受,再多 1 字节拒绝;各单串
// 均不超 InputStringBytes,超的只是总预算。
func TestConfigBytesExactUpperBoundAccepted(t *testing.T) {
	// 用资源根凑足文本:每根取 min(余量, InputStringBytes),末根取
	// 余量,使固定路径 + 各根合计恰为默认 ConfigBytes。
	cfg := validRuntimeConfig()
	remaining := defaultLimits().ConfigBytes - uint64(len(cfg.LibraryPath)+len(cfg.UserDir))
	cfg.ResourceRoots = nil
	for remaining > 0 {
		n := remaining
		if n > defaultLimits().InputStringBytes {
			n = defaultLimits().InputStringBytes
		}
		cfg.ResourceRoots = append(cfg.ResourceRoots, testAbsolutePathWithLength(int(n)))
		remaining -= n
	}
	total := uint64(len(cfg.LibraryPath) + len(cfg.UserDir))
	for _, root := range cfg.ResourceRoots {
		total += uint64(len(root))
	}
	if total != defaultLimits().ConfigBytes {
		t.Fatalf("test construction is off: text total %d, want exactly %d", total, defaultLimits().ConfigBytes)
	}

	snap, err := snapshotConfig(cfg)
	if err != nil {
		t.Fatalf("a configuration with a text total of exactly ConfigBytes should be accepted: %v", err)
	}
	if len(snap.resourceRoots) != len(cfg.ResourceRoots) {
		t.Fatalf("snapshot resource root count = %d, want %d", len(snap.resourceRoots), len(cfg.ResourceRoots))
	}

	// 末根再多 1 字节仍在 InputStringBytes 之内,被拒的只是总预算。
	last := len(cfg.ResourceRoots) - 1
	cfg.ResourceRoots[last] += "x"
	if _, err := snapshotConfig(cfg); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("a text total one byte over ConfigBytes must be rejected, got %v", err)
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
			t.Fatalf("extras %q should be accepted: %v", raw, err)
		}
		if got := string(snap.connectionExtras[0].JSON); got != raw {
			t.Errorf("extras raw JSON was rewritten: got %q, want %q", got, raw)
		}
	}
}

// TestStaticOptionValueMayBeEmpty 验证 option value 允许空串
// (路径与 extras name 仍必须非空):空串贡献 0 字节预算。
func TestStaticOptionValueMayBeEmpty(t *testing.T) {
	cfg := validRuntimeConfig()
	cfg.StaticOptions = []StaticOption{{Key: 1, Value: ""}, {Key: 2, Value: "x"}}
	snap, err := snapshotConfig(cfg)
	if err != nil {
		t.Fatalf("an empty option value should be accepted: %v", err)
	}
	if snap.staticOptions[0].Value != "" || snap.staticOptions[1].Value != "x" {
		t.Errorf("static option snapshot mismatch: %+v", snap.staticOptions)
	}
}

// TestStaticOptionZeroKeyRejected 验证 option key 为 0 时被拒绝:
// key 0(StaticOptionKey 的 Invalid)在纯 Go 校验阶段被拒,错误含
// 字段路径;校验发生在触碰生命周期门之前,
// 原生调用计数为 0,同一 gate 修正输入后仍可成功 Open。
func TestStaticOptionZeroKeyRejected(t *testing.T) {
	cfg := validRuntimeConfig()
	// 无效 key 放在合法项之后:逐元素校验不因前面有合法项而短路。
	cfg.StaticOptions = []StaticOption{{Key: 1, Value: "ok"}, {Key: 0, Value: "invalid"}}

	snap, err := snapshotConfig(cfg)
	if err == nil {
		t.Fatal("an option key of 0 must return an error")
	}
	if len(snap.staticOptions) != 0 || len(snap.resourceRoots) != 0 || len(snap.connectionExtras) != 0 || snap.limits != (Limits{}) {
		t.Fatalf("a failed validation must not return a snapshot, got %+v", snap)
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "StaticOptions[1].Key") {
		t.Errorf("error text %q does not contain field path StaticOptions[1].Key", err.Error())
	}

	// 经注入边界的 open 复核拒绝路径:原生调用计数为 0,
	// 校验失败不消耗进程生命周期。
	env := &fakeEnv{session: newValidFakeSession()}
	gate := &lifecycleGate{}
	rt, err := open(cfg, env, gate)
	if rt != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("open must fail at the validation stage: rt=%v err=%v", rt, err)
	}
	if n := env.loadCount(); n != 0 {
		t.Errorf("loadLibrary called %d times, want 0 (the native call count must be 0)", n)
	}
	if _, err := open(validRuntimeConfig(), env, gate); err != nil {
		t.Fatalf("the same gate must still Open successfully after a failed validation: %v", err)
	}
}

// TestSnapshotDeepCopiesConfig 验证快照深拷贝:snapshotConfig 返回
// 后修改原配置的 slice 元素、RawMessage 底层字节与追加,不影响快照。
func TestSnapshotDeepCopiesConfig(t *testing.T) {
	cfg := validRuntimeConfig()
	cfg.StaticOptions = []StaticOption{{Key: 3, Value: "orig"}}
	cfg.ResourceRoots = []string{testAbsolutePath("resource")}
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
		t.Fatalf("a valid configuration must not fail validation: %v", err)
	}

	// Open 返回后修改原配置的一切可变内容。
	cfg.StaticOptions[0].Value = "tampered"
	cfg.StaticOptions = append(cfg.StaticOptions, StaticOption{Key: 9, Value: "extra"})
	cfg.ResourceRoots[0] = testAbsolutePath("tampered")
	cfg.ResourceRoots = append(cfg.ResourceRoots, testAbsolutePath("tampered2"))
	cfg.ConnectionExtras[0].Name = "tampered"
	cfg.ConnectionExtras[0].JSON[0] = 'X'
	cfg.ConnectionExtras = append(cfg.ConnectionExtras, ConnectionExtra{Name: "t2", JSON: json.RawMessage(`{}`)})
	cfg.Limits.InputStringBytes = 1

	if snap.staticOptions[0].Value != "orig" || len(snap.staticOptions) != 1 {
		t.Errorf("static option snapshot was tampered with: %+v", snap.staticOptions)
	}
	if snap.resourceRoots[0] != testAbsolutePath("resource") || len(snap.resourceRoots) != 1 {
		t.Errorf("resource root snapshot was tampered with: %v", snap.resourceRoots)
	}
	if snap.connectionExtras[0].Name != "sink" || len(snap.connectionExtras) != 1 {
		t.Fatalf("extras snapshot was tampered with: %+v", snap.connectionExtras)
	}
	if got := string(snap.connectionExtras[0].JSON); got != `{"orig":1}` {
		t.Errorf("extras JSON snapshot was tampered with: %q", got)
	}
	if snap.limits != wantLimits || snap.limits.InputStringBytes != 12_345 {
		t.Errorf("Limits snapshot was tampered with: got %+v, want %+v", snap.limits, wantLimits)
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
		t.Error("addChecked(MaxUint64, 1) must detect overflow")
	}
	if _, ok := addChecked(1, math.MaxUint64); ok {
		t.Error("addChecked(1, MaxUint64) must detect overflow")
	}
	if got, ok := addChecked(math.MaxUint64, math.MaxUint64); ok || got != 0 {
		t.Errorf("addChecked(MaxUint64, MaxUint64) = (%d, %v), want (0, false)", got, ok)
	}
}

// 编译期锁定:配置校验失败的哨兵必须与公开错误面同一错误值。
var _ error = native.ErrInvalidInput
