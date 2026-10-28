// 本文件是实例配置与连接、任务输入的校验/快照用例:白盒验证错误分类、
// 字段路径、预算边界与深拷贝语义,不依赖 MaaCore 或设备。

package maa

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// TestInstanceConfigValidationRejectsInvalidContent 逐类验证实例
// 配置校验失败:key 为 0(Invalid)或重复、value 非法、项数/字节
// 超预算都返回带字段路径的 ErrInvalidInput,且不返回快照。
func TestInstanceConfigValidationRejectsInvalidContent(t *testing.T) {
	nul := "\x00"
	invalidUTF8 := string([]byte{0xff, 0xfe})

	cases := []struct {
		name      string
		options   []InstanceOption
		wantField string
	}{
		{"key is 0", []InstanceOption{{Key: 1, Value: "ok"}, {Key: 0, Value: "invalid"}}, "Options[1].Key"},
		{"key duplicated", []InstanceOption{{Key: 2, Value: "a"}, {Key: 2, Value: "b"}}, "Options[1]"},
		{"value embedded NUL", []InstanceOption{{Key: 1, Value: "v" + nul}}, "Options[0].Value"},
		{"value invalid UTF-8", []InstanceOption{{Key: 1, Value: invalidUTF8}}, "Options[0].Value"},
		{"single value exceeds InputStringBytes", []InstanceOption{{Key: 1, Value: strings.Repeat("a", 65_537)}}, "Options[0].Value"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := snapshotInstanceConfig(InstanceConfig{Options: tc.options}, defaultLimits())
			if len(snap.options) != 0 {
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

	t.Run("entry count exceeds ConfigEntries", func(t *testing.T) {
		options := make([]InstanceOption, defaultLimits().ConfigEntries+1)
		for i := range options {
			options[i] = InstanceOption{Key: int32(i + 1), Value: "v"}
		}
		_, err := snapshotInstanceConfig(InstanceConfig{Options: options}, defaultLimits())
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "ConfigEntries") {
			t.Fatalf("an entry count over ConfigEntries must be rejected: %v", err)
		}
	})

	t.Run("value total exceeds ConfigBytes", func(t *testing.T) {
		// 65 个恰好 65,536 字节(不超单串预算)的 value 合计
		// 4,259,840,超过默认 ConfigBytes 4,194,304。
		options := make([]InstanceOption, 65)
		for i := range options {
			options[i] = InstanceOption{Key: int32(i + 1), Value: strings.Repeat("a", 65_536)}
		}
		_, err := snapshotInstanceConfig(InstanceConfig{Options: options}, defaultLimits())
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "ConfigBytes") {
			t.Fatalf("a text total over ConfigBytes must be rejected: %v", err)
		}
	})
}

// TestInstanceConfigAcceptsUnknownKeysAndEmptyValues 验证语义保持:
// 未识别的非零 int32 key(含两端边界值)原样接受,不把业务选项
// 封闭为枚举;value 允许空串;合计项数恰为 ConfigEntries 被接受。
func TestInstanceConfigAcceptsUnknownKeysAndEmptyValues(t *testing.T) {
	cfg := InstanceConfig{Options: []InstanceOption{
		{Key: 1, Value: ""},
		{Key: 42, Value: "unknown-but-passed-through"},
		{Key: math.MinInt32, Value: ""},
		{Key: math.MaxInt32, Value: ""},
	}}
	snap, err := snapshotInstanceConfig(cfg, defaultLimits())
	if err != nil {
		t.Fatalf("unknown non-zero keys and empty values should be accepted: %v", err)
	}
	if len(snap.options) != len(cfg.Options) || snap.options[1] != cfg.Options[1] {
		t.Errorf("instance option snapshot mismatch: %+v", snap.options)
	}

	options := make([]InstanceOption, defaultLimits().ConfigEntries)
	for i := range options {
		options[i] = InstanceOption{Key: int32(i + 1), Value: "v"}
	}
	if _, err := snapshotInstanceConfig(InstanceConfig{Options: options}, defaultLimits()); err != nil {
		t.Fatalf("an entry count of exactly ConfigEntries should be accepted: %v", err)
	}
}

// TestInstanceConfigSnapshotIsDeepCopy 验证:快照返回后修改原
// Options 的元素与追加,不影响快照。
func TestInstanceConfigSnapshotIsDeepCopy(t *testing.T) {
	cfg := InstanceConfig{Options: []InstanceOption{{Key: 3, Value: "orig"}}}

	snap, err := snapshotInstanceConfig(cfg, defaultLimits())
	if err != nil {
		t.Fatalf("a valid configuration must not fail validation: %v", err)
	}

	cfg.Options[0].Value = "tampered"
	cfg.Options[0].Key = 4
	cfg.Options = append(cfg.Options, InstanceOption{Key: 9, Value: "extra"})

	if len(snap.options) != 1 || snap.options[0] != (InstanceOption{Key: 3, Value: "orig"}) {
		t.Errorf("instance option snapshot was tampered with: %+v", snap.options)
	}
}

// TestConnectConfigValidationRejectsInvalidInput 逐类验证连接输入
// 校验失败:错误文本包含字段路径。
func TestConnectConfigValidationRejectsInvalidInput(t *testing.T) {
	nul := "\x00"
	invalidUTF8 := string([]byte{0xff, 0xfe})

	cases := []struct {
		name      string
		cfg       ConnectConfig
		wantField string
	}{
		{"ADBPath is empty", ConnectConfig{Address: "127.0.0.1:5555"}, "ADBPath"},
		{"ADBPath relative path", ConnectConfig{ADBPath: "adb", Address: "127.0.0.1:5555"}, "ADBPath"},
		{"ADBPath embedded NUL", ConnectConfig{ADBPath: testAbsolutePath("adb") + nul, Address: "127.0.0.1:5555"}, "ADBPath"},
		{"ADBPath invalid UTF-8", ConnectConfig{ADBPath: testAbsolutePath("adb") + invalidUTF8, Address: "127.0.0.1:5555"}, "ADBPath"},
		{"Address is empty", ConnectConfig{ADBPath: testAbsolutePath("adb")}, "Address"},
		{"Address embedded NUL", ConnectConfig{ADBPath: testAbsolutePath("adb"), Address: "127.0.0.1" + nul}, "Address"},
		{"Config embedded NUL", ConnectConfig{ADBPath: testAbsolutePath("adb"), Address: "127.0.0.1:5555", Config: "{" + nul + "}"}, "Config"},
		{"Config invalid UTF-8", ConnectConfig{ADBPath: testAbsolutePath("adb"), Address: "127.0.0.1:5555", Config: invalidUTF8}, "Config"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := snapshotConnectInput(tc.cfg, defaultLimits())
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

// TestConnectConfigAcceptsEmptyConfigAndBudget 验证:Config 允许
// 空串(将被原样编码为空 C 字符串而非 nil);ADBPath + Address +
// Config 的文本合计超过 ConfigBytes 时拒绝,单串预算仍逐项执行。
func TestConnectConfigAcceptsEmptyConfigAndBudget(t *testing.T) {
	cfg := ConnectConfig{ADBPath: testAbsolutePath("adb"), Address: "127.0.0.1:5555", Config: ""}
	snap, err := snapshotConnectInput(cfg, defaultLimits())
	if err != nil {
		t.Fatalf("an empty Config should be accepted: %v", err)
	}
	if snap.adbPath != cfg.ADBPath || snap.address != cfg.Address || snap.config != "" {
		t.Errorf("connect input snapshot mismatch: %+v", snap)
	}

	// 三串各 40 字节(不超单串预算),合计 120 超过收窄后的
	// ConfigBytes 100。收窄后的整套限制仍满足全部交叉校验。
	limits := defaultLimits()
	limits.InputStringBytes = 64
	limits.InputJSONBytes = 64
	limits.ConfigBytes = 100
	full := ConnectConfig{ADBPath: testAbsolutePathWithLength(40), Address: strings.Repeat("b", 40), Config: strings.Repeat("c", 40)}
	_, err = snapshotConnectInput(full, limits)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "connect input text bytes total") {
		t.Fatalf("a connect input text total over ConfigBytes must be rejected: %v", err)
	}

	// 单串预算独立执行:恰达 65,537 字节的 ADBPath 超默认
	// InputStringBytes。
	_, err = snapshotConnectInput(ConnectConfig{ADBPath: testAbsolutePathWithLength(65_537), Address: "x"}, defaultLimits())
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "InputStringBytes") {
		t.Fatalf("an ADBPath over the single-string budget must be rejected: %v", err)
	}
}

// TestTaskInputRejectsInvalidInputDistinguishably 验证任务输入校验:
// 内嵌 NUL 与非法 JSON 都可辨识地失败——全部包装 ErrInvalidInput,
// 但诊断文本可区分(字段路径与失败原因各自不同)。
func TestTaskInputRejectsInvalidInputDistinguishably(t *testing.T) {
	nul := "\x00"
	invalidUTF8 := string([]byte{0xff, 0xfe})

	cases := []struct {
		name       string
		taskType   string
		params     json.RawMessage
		wantSubstr string
	}{
		{"taskType is empty", "", json.RawMessage(`{}`), "taskType must not be empty"},
		{"taskType embedded NUL", "Fighting" + nul, json.RawMessage(`{}`), "taskType contains an embedded NUL byte"},
		{"taskType invalid UTF-8", invalidUTF8, json.RawMessage(`{}`), "taskType is not valid UTF-8"},
		{"taskType exceeds the single-string budget", strings.Repeat("a", 65_537), json.RawMessage(`{}`), "taskType length"},
		{"params is nil", "Fighting", nil, "params is not valid JSON"},
		{"params is whitespace", "Fighting", json.RawMessage(`   `), "params is not valid JSON"},
		{"params is null", "Fighting", json.RawMessage(`null`), "params must be a JSON object"},
		{"params is an array", "Fighting", json.RawMessage(`[1,2]`), "params must be a JSON object"},
		{"params is a scalar", "Fighting", json.RawMessage(`42`), "params must be a JSON object"},
		{"params truncated", "Fighting", json.RawMessage(`{"a":`), "params is not a complete JSON object"},
		{"params trailing second value", "Fighting", json.RawMessage(`{} {}`), "params has trailing content"},
		{"params literal NUL in a value", "Fighting", json.RawMessage("{\"a\":\"\x00\"}"), "params contains a literal NUL byte"},
		{"params literal NUL in a key", "Fighting", json.RawMessage("{\"a\x00\":1}"), "params contains a literal NUL byte"},
	}

	var representative [3]string
	seen := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskInput(tc.taskType, tc.params, defaultLimits())
			if err == nil {
				t.Fatal("a failed validation must return an error")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error text %q does not contain the distinguishable diagnostic %q", err.Error(), tc.wantSubstr)
			}
			// 收集三个代表性失败,验证诊断彼此可区分。
			switch tc.name {
			case "taskType embedded NUL":
				representative[0] = err.Error()
				seen++
			case "params literal NUL in a value":
				representative[1] = err.Error()
				seen++
			case "params truncated":
				representative[2] = err.Error()
				seen++
			}
		})
	}
	if seen != 3 {
		t.Fatalf("representative cases incompletely collected: %d", seen)
	}
	if representative[0] == representative[1] || representative[1] == representative[2] || representative[0] == representative[2] {
		t.Errorf("diagnostics for embedded NUL and invalid JSON must be distinguishable: %q / %q / %q",
			representative[0], representative[1], representative[2])
	}
}

// TestTaskInputPreservesUnknownAndBigIntegerVerbatim 验证前向兼容:
// 未知任务类型不作枚举校验原样接受;params 的未知字段、\u0000
// 转义与超过 2^53 的整数按原文字节保留,不经 float64 往返。
func TestTaskInputPreservesUnknownAndBigIntegerVerbatim(t *testing.T) {
	taskType := "FutureTask@Unknown-Scheme"
	raw := json.RawMessage(`{"unknown_field":{"nested":[1,{"x":9007199254740993}]},"esc":"\u0000","big":12345678901234567890}`)

	if err := validateTaskInput(taskType, raw, defaultLimits()); err != nil {
		t.Fatalf("unknown task types and big-integer params should be accepted: %v", err)
	}
	snap, err := snapshotTaskInput(taskType, raw, defaultLimits())
	if err != nil {
		t.Fatalf("snapshot must not fail: %v", err)
	}
	if snap.taskType != taskType {
		t.Errorf("taskType was rewritten: %q", snap.taskType)
	}
	if string(snap.params) != string(raw) {
		t.Errorf("params raw JSON was rewritten:\n got %q\nwant %q", string(snap.params), string(raw))
	}

	// 重复校验与快照不改变原输入。
	if err := validateTaskInput(taskType, raw, defaultLimits()); err != nil {
		t.Errorf("repeated validation must not fail: %v", err)
	}
	if string(raw) != `{"unknown_field":{"nested":[1,{"x":9007199254740993}]},"esc":"\u0000","big":12345678901234567890}` {
		t.Errorf("the original input was rewritten: %q", string(raw))
	}
}

// TestTaskInputSnapshotIsDeepCopyAndBudget 验证:快照返回后原地
// 改写原 RawMessage 底层字节不影响快照;taskType + params 文本合计
// 超 ConfigBytes 时拒绝(两串各自不超单串/单份预算)。
func TestTaskInputSnapshotIsDeepCopyAndBudget(t *testing.T) {
	raw := json.RawMessage(`{"orig":9007199254740993}`)
	snap, err := snapshotTaskInput("Fighting", raw, defaultLimits())
	if err != nil {
		t.Fatalf("valid input must not fail: %v", err)
	}

	for i := range raw {
		raw[i] = 'X'
	}
	if string(snap.params) != `{"orig":9007199254740993}` {
		t.Errorf("task input snapshot was tampered with: %q", string(snap.params))
	}

	// 60 + 60 = 120 字节(各自不超收窄后的单串/单份预算 64),
	// 合计超过收窄后的 ConfigBytes 100。
	limits := defaultLimits()
	limits.InputStringBytes = 64
	limits.InputJSONBytes = 64
	limits.ConfigBytes = 100
	err = validateTaskInput(strings.Repeat("a", 60), json.RawMessage(`{"b":"`+strings.Repeat("c", 52)+`"}`), limits)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "task input text bytes total") {
		t.Fatalf("a task input text total over ConfigBytes must be rejected: %v", err)
	}
}

// 编译期锁定:任务输入校验失败的哨兵必须与公开错误面同一错误值。
var _ error = native.ErrInvalidInput
