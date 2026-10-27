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
		{"key 为 0", []InstanceOption{{Key: 1, Value: "ok"}, {Key: 0, Value: "invalid"}}, "Options[1].Key"},
		{"key 重复", []InstanceOption{{Key: 2, Value: "a"}, {Key: 2, Value: "b"}}, "Options[1]"},
		{"value 内嵌 NUL", []InstanceOption{{Key: 1, Value: "v" + nul}}, "Options[0].Value"},
		{"value 非法 UTF-8", []InstanceOption{{Key: 1, Value: invalidUTF8}}, "Options[0].Value"},
		{"单 value 超 InputStringBytes", []InstanceOption{{Key: 1, Value: strings.Repeat("a", 65_537)}}, "Options[0].Value"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := snapshotInstanceConfig(InstanceConfig{Options: tc.options}, defaultLimits())
			if len(snap.options) != 0 {
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

	t.Run("项数超 ConfigEntries", func(t *testing.T) {
		options := make([]InstanceOption, defaultLimits().ConfigEntries+1)
		for i := range options {
			options[i] = InstanceOption{Key: int32(i + 1), Value: "v"}
		}
		_, err := snapshotInstanceConfig(InstanceConfig{Options: options}, defaultLimits())
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "ConfigEntries") {
			t.Fatalf("项数超 ConfigEntries 必须拒绝: %v", err)
		}
	})

	t.Run("value 合计超 ConfigBytes", func(t *testing.T) {
		// 65 个恰好 65,536 字节(不超单串预算)的 value 合计
		// 4,259,840,超过默认 ConfigBytes 4,194,304。
		options := make([]InstanceOption, 65)
		for i := range options {
			options[i] = InstanceOption{Key: int32(i + 1), Value: strings.Repeat("a", 65_536)}
		}
		_, err := snapshotInstanceConfig(InstanceConfig{Options: options}, defaultLimits())
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "ConfigBytes") {
			t.Fatalf("文本合计超 ConfigBytes 必须拒绝: %v", err)
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
		t.Fatalf("未知非零 key 与空 value 应当被接受: %v", err)
	}
	if len(snap.options) != len(cfg.Options) || snap.options[1] != cfg.Options[1] {
		t.Errorf("实例选项快照不符: %+v", snap.options)
	}

	options := make([]InstanceOption, defaultLimits().ConfigEntries)
	for i := range options {
		options[i] = InstanceOption{Key: int32(i + 1), Value: "v"}
	}
	if _, err := snapshotInstanceConfig(InstanceConfig{Options: options}, defaultLimits()); err != nil {
		t.Fatalf("合计项数恰为 ConfigEntries 应当被接受: %v", err)
	}
}

// TestInstanceConfigSnapshotIsDeepCopy 验证:快照返回后修改原
// Options 的元素与追加,不影响快照。
func TestInstanceConfigSnapshotIsDeepCopy(t *testing.T) {
	cfg := InstanceConfig{Options: []InstanceOption{{Key: 3, Value: "orig"}}}

	snap, err := snapshotInstanceConfig(cfg, defaultLimits())
	if err != nil {
		t.Fatalf("合法配置不得校验失败: %v", err)
	}

	cfg.Options[0].Value = "tampered"
	cfg.Options[0].Key = 4
	cfg.Options = append(cfg.Options, InstanceOption{Key: 9, Value: "extra"})

	if len(snap.options) != 1 || snap.options[0] != (InstanceOption{Key: 3, Value: "orig"}) {
		t.Errorf("实例选项快照被改写: %+v", snap.options)
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
		{"ADBPath 为空", ConnectConfig{Address: "127.0.0.1:5555"}, "ADBPath"},
		{"ADBPath 相对路径", ConnectConfig{ADBPath: "adb", Address: "127.0.0.1:5555"}, "ADBPath"},
		{"ADBPath 内嵌 NUL", ConnectConfig{ADBPath: "/usr/bin/ad" + nul + "b", Address: "127.0.0.1:5555"}, "ADBPath"},
		{"ADBPath 非法 UTF-8", ConnectConfig{ADBPath: "/usr/bin/" + invalidUTF8, Address: "127.0.0.1:5555"}, "ADBPath"},
		{"Address 为空", ConnectConfig{ADBPath: "/usr/bin/adb"}, "Address"},
		{"Address 内嵌 NUL", ConnectConfig{ADBPath: "/usr/bin/adb", Address: "127.0.0.1" + nul}, "Address"},
		{"Config 内嵌 NUL", ConnectConfig{ADBPath: "/usr/bin/adb", Address: "127.0.0.1:5555", Config: "{" + nul + "}"}, "Config"},
		{"Config 非法 UTF-8", ConnectConfig{ADBPath: "/usr/bin/adb", Address: "127.0.0.1:5555", Config: invalidUTF8}, "Config"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := snapshotConnectInput(tc.cfg, defaultLimits())
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

// TestConnectConfigAcceptsEmptyConfigAndBudget 验证:Config 允许
// 空串(将被原样编码为空 C 字符串而非 nil);ADBPath + Address +
// Config 的文本合计超过 ConfigBytes 时拒绝,单串预算仍逐项执行。
func TestConnectConfigAcceptsEmptyConfigAndBudget(t *testing.T) {
	cfg := ConnectConfig{ADBPath: "/usr/bin/adb", Address: "127.0.0.1:5555", Config: ""}
	snap, err := snapshotConnectInput(cfg, defaultLimits())
	if err != nil {
		t.Fatalf("空 Config 应当被接受: %v", err)
	}
	if snap.adbPath != cfg.ADBPath || snap.address != cfg.Address || snap.config != "" {
		t.Errorf("连接输入快照不符: %+v", snap)
	}

	// 三串各 40 字节(不超单串预算),合计 120 超过收窄后的
	// ConfigBytes 100。收窄后的整套限制仍满足全部交叉校验。
	limits := defaultLimits()
	limits.InputStringBytes = 64
	limits.InputJSONBytes = 64
	limits.ConfigBytes = 100
	full := ConnectConfig{ADBPath: "/" + strings.Repeat("a", 39), Address: strings.Repeat("b", 40), Config: strings.Repeat("c", 40)}
	_, err = snapshotConnectInput(full, limits)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "connect input text bytes total") {
		t.Fatalf("连接输入文本合计超 ConfigBytes 必须拒绝: %v", err)
	}

	// 单串预算独立执行:恰达 65,537 字节的 ADBPath 超默认
	// InputStringBytes。
	_, err = snapshotConnectInput(ConnectConfig{ADBPath: "/" + strings.Repeat("a", 65_536), Address: "x"}, defaultLimits())
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "InputStringBytes") {
		t.Fatalf("ADBPath 超单串预算必须拒绝: %v", err)
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
		{"taskType 为空", "", json.RawMessage(`{}`), "taskType must not be empty"},
		{"taskType 内嵌 NUL", "Fighting" + nul, json.RawMessage(`{}`), "taskType contains an embedded NUL byte"},
		{"taskType 非法 UTF-8", invalidUTF8, json.RawMessage(`{}`), "taskType is not valid UTF-8"},
		{"taskType 超单串预算", strings.Repeat("a", 65_537), json.RawMessage(`{}`), "taskType length"},
		{"params 为 nil", "Fighting", nil, "params is not valid JSON"},
		{"params 为空白", "Fighting", json.RawMessage(`   `), "params is not valid JSON"},
		{"params 为 null", "Fighting", json.RawMessage(`null`), "params must be a JSON object"},
		{"params 为数组", "Fighting", json.RawMessage(`[1,2]`), "params must be a JSON object"},
		{"params 为标量", "Fighting", json.RawMessage(`42`), "params must be a JSON object"},
		{"params 截断", "Fighting", json.RawMessage(`{"a":`), "params is not a complete JSON object"},
		{"params 尾随第二个值", "Fighting", json.RawMessage(`{} {}`), "params has trailing content"},
		{"params 值内实际 NUL", "Fighting", json.RawMessage("{\"a\":\"\x00\"}"), "params contains a literal NUL byte"},
		{"params 键内实际 NUL", "Fighting", json.RawMessage("{\"a\x00\":1}"), "params contains a literal NUL byte"},
	}

	var representative [3]string
	seen := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskInput(tc.taskType, tc.params, defaultLimits())
			if err == nil {
				t.Fatal("校验失败必须返回错误")
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("错误文本 %q 未包含可辨识诊断 %q", err.Error(), tc.wantSubstr)
			}
			// 收集三个代表性失败,验证诊断彼此可区分。
			switch tc.name {
			case "taskType 内嵌 NUL":
				representative[0] = err.Error()
				seen++
			case "params 值内实际 NUL":
				representative[1] = err.Error()
				seen++
			case "params 截断":
				representative[2] = err.Error()
				seen++
			}
		})
	}
	if seen != 3 {
		t.Fatalf("代表性用例收集不全: %d", seen)
	}
	if representative[0] == representative[1] || representative[1] == representative[2] || representative[0] == representative[2] {
		t.Errorf("内嵌 NUL 与非法 JSON 的诊断必须可区分: %q / %q / %q",
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
		t.Fatalf("未知任务类型与大整数 params 应当被接受: %v", err)
	}
	snap, err := snapshotTaskInput(taskType, raw, defaultLimits())
	if err != nil {
		t.Fatalf("快照不得失败: %v", err)
	}
	if snap.taskType != taskType {
		t.Errorf("taskType 被改写: %q", snap.taskType)
	}
	if string(snap.params) != string(raw) {
		t.Errorf("params 原文被改写:\n got %q\nwant %q", string(snap.params), string(raw))
	}

	// 重复校验与快照不改变原输入。
	if err := validateTaskInput(taskType, raw, defaultLimits()); err != nil {
		t.Errorf("重复校验不得失败: %v", err)
	}
	if string(raw) != `{"unknown_field":{"nested":[1,{"x":9007199254740993}]},"esc":"\u0000","big":12345678901234567890}` {
		t.Errorf("原输入被改写: %q", string(raw))
	}
}

// TestTaskInputSnapshotIsDeepCopyAndBudget 验证:快照返回后原地
// 改写原 RawMessage 底层字节不影响快照;taskType + params 文本合计
// 超 ConfigBytes 时拒绝(两串各自不超单串/单份预算)。
func TestTaskInputSnapshotIsDeepCopyAndBudget(t *testing.T) {
	raw := json.RawMessage(`{"orig":9007199254740993}`)
	snap, err := snapshotTaskInput("Fighting", raw, defaultLimits())
	if err != nil {
		t.Fatalf("合法输入不得失败: %v", err)
	}

	for i := range raw {
		raw[i] = 'X'
	}
	if string(snap.params) != `{"orig":9007199254740993}` {
		t.Errorf("任务输入快照被改写: %q", string(snap.params))
	}

	// 60 + 60 = 120 字节(各自不超收窄后的单串/单份预算 64),
	// 合计超过收窄后的 ConfigBytes 100。
	limits := defaultLimits()
	limits.InputStringBytes = 64
	limits.InputJSONBytes = 64
	limits.ConfigBytes = 100
	err = validateTaskInput(strings.Repeat("a", 60), json.RawMessage(`{"b":"`+strings.Repeat("c", 52)+`"}`), limits)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "task input text bytes total") {
		t.Fatalf("任务输入文本合计超 ConfigBytes 必须拒绝: %v", err)
	}
}

// 编译期锁定:任务输入校验失败的哨兵必须与公开错误面同一错误值。
var _ error = native.ErrInvalidInput
