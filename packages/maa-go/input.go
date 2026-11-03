package maa

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// InstanceOption 是一条实例级选项:key 为 int32(InstanceOptionKey
// 语义;key 为 0 即 Invalid,在纯 Go 校验阶段拒绝并返回
// ErrInvalidInput;未识别的非零 key 原样透传,不把业务选项封闭为
// 枚举),value 允许空串。key 不得重复。与 StaticOption 的 key
// 分属不同枚举空间,类型分开定义避免混用。
type InstanceOption struct {
	Key   int32
	Value string
}

// InstanceConfig 是实例创建的输入:有序实例选项。全部内容在校验
// 通过后被深拷贝冻结;调用者在创建方法返回前不得并发修改输入,
// 返回后可修改原配置,不影响已创建实例。
type InstanceConfig struct {
	Options []InstanceOption
}

// ConnectConfig 是设备连接的输入:ADBPath 是 ADB 可执行文件路径
// (非空绝对路径,不做 shell 展开、不做存在性检查),Address 是
// 连接地址(非空),Config 是连接 profile 字符串——允许空串并
// 原样传空 C 字符串,由锁定 Core 选择 General;它不是 JSON,
// 按字符串原样传递。
type ConnectConfig struct {
	ADBPath string
	Address string
	Config  string
}

func validateInstanceConfig(cfg InstanceConfig, limits Limits) error {
	seenKey := make(map[int32]bool, len(cfg.Options))
	total := uint64(0)
	for i, opt := range cfg.Options {
		if opt.Key == 0 {
			return fmt.Errorf("maa: Options[%d].Key must not be 0 (Invalid): %w", i, native.ErrInvalidInput)
		}
		if seenKey[opt.Key] {
			return fmt.Errorf("maa: Options[%d] duplicates key %d: %w", i, opt.Key, native.ErrInvalidInput)
		}
		seenKey[opt.Key] = true
		if err := validateStringField(fmt.Sprintf("Options[%d].Value", i), opt.Value, limits.InputStringBytes, true); err != nil {
			return err
		}
		var ok bool
		total, ok = addChecked(total, uint64(len(opt.Value)))
		if !ok {
			return fmt.Errorf("maa: instance config text bytes total overflows uint64: %w", native.ErrInvalidInput)
		}
	}
	// 检查顺序与 runtime.go validateConfigBudget 一致:先字节合计,后条目。
	if total > limits.ConfigBytes {
		return fmt.Errorf("maa: instance config text bytes total %d exceeds Limits.ConfigBytes %d: %w", total, limits.ConfigBytes, native.ErrInvalidInput)
	}
	if entries := uint64(len(cfg.Options)); entries > limits.ConfigEntries {
		return fmt.Errorf("maa: instance config entries %d exceeds Limits.ConfigEntries %d: %w", entries, limits.ConfigEntries, native.ErrInvalidInput)
	}
	return nil
}

type instanceConfigSnapshot struct {
	options []InstanceOption
}

func snapshotInstanceConfig(cfg InstanceConfig, limits Limits) (instanceConfigSnapshot, error) {
	if err := validateInstanceConfig(cfg, limits); err != nil {
		return instanceConfigSnapshot{}, err
	}
	options := make([]InstanceOption, len(cfg.Options))
	copy(options, cfg.Options)
	return instanceConfigSnapshot{options: options}, nil
}

func validateConnectConfig(cfg ConnectConfig, limits Limits) error {
	if err := validatePathField("ADBPath", cfg.ADBPath, limits.InputStringBytes); err != nil {
		return err
	}
	if err := validateStringField("Address", cfg.Address, limits.InputStringBytes, false); err != nil {
		return err
	}
	if err := validateStringField("Config", cfg.Config, limits.InputStringBytes, true); err != nil {
		return err
	}
	total := uint64(0)
	for _, n := range []uint64{uint64(len(cfg.ADBPath)), uint64(len(cfg.Address)), uint64(len(cfg.Config))} {
		var ok bool
		total, ok = addChecked(total, n)
		if !ok {
			return fmt.Errorf("maa: connect input text bytes total overflows uint64: %w", native.ErrInvalidInput)
		}
	}
	if total > limits.ConfigBytes {
		return fmt.Errorf("maa: connect input text bytes total %d exceeds Limits.ConfigBytes %d: %w", total, limits.ConfigBytes, native.ErrInvalidInput)
	}
	return nil
}

type connectInputSnapshot struct {
	adbPath string
	address string
	config  string
}

func snapshotConnectInput(cfg ConnectConfig, limits Limits) (connectInputSnapshot, error) {
	if err := validateConnectConfig(cfg, limits); err != nil {
		return connectInputSnapshot{}, err
	}
	return connectInputSnapshot{adbPath: cfg.ADBPath, address: cfg.Address, config: cfg.Config}, nil
}

func validateTaskInput(taskType string, params json.RawMessage, limits Limits) error {
	if err := validateStringField("taskType", taskType, limits.InputStringBytes, false); err != nil {
		return err
	}
	if err := validateJSONObject("params", params, limits.InputJSONBytes); err != nil {
		return err
	}
	total, ok := addChecked(uint64(len(taskType)), uint64(len(params)))
	if !ok {
		return fmt.Errorf("maa: task input text bytes total overflows uint64: %w", native.ErrInvalidInput)
	}
	if total > limits.ConfigBytes {
		return fmt.Errorf("maa: task input text bytes total %d exceeds Limits.ConfigBytes %d: %w", total, limits.ConfigBytes, native.ErrInvalidInput)
	}
	return nil
}

type taskInputSnapshot struct {
	taskType string
	params   json.RawMessage
}

func snapshotTaskInput(taskType string, params json.RawMessage, limits Limits) (taskInputSnapshot, error) {
	if err := validateTaskInput(taskType, params, limits); err != nil {
		return taskInputSnapshot{}, err
	}
	return taskInputSnapshot{taskType: taskType, params: bytes.Clone(params)}, nil
}
