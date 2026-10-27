// 本文件定义实例配置与连接、任务输入的公开数据类型,以及对应的纯 Go
// 校验与快照辅助。校验与复制复用 snapshot.go 的共享辅助层,不另立一套
// 逻辑。
//
// 实例在预留生命周期名额之前构造快照;方法输入在 raw 调用前经 cString
// 构造带 NUL 的 C buffer 并持有至 raw 返回及结果登记完成。
//
// 预算契约:配置预算(ConfigEntries/ConfigBytes)分别用于 Open 与
// NewInstance 的快照;方法输入(taskType + params、ADBPath + Address +
// Config)先逐项校验单字符串/单份 JSON 预算,再用检查溢出的加法累计核对
// ConfigBytes。终止 NUL 的额外每串 1 字节不计入本预算,由分配方在分配
// 前另行检查。

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

// InstanceConfig 是实例创建的输入:有序实例选项(
// InstanceConfig 只有有序 Options,不覆盖 Runtime 的限制)。
// 全部内容在校验通过后被深拷贝冻结为 instanceConfigSnapshot;
// 调用者在创建方法返回前不得并发修改输入,返回后可修改原配置,
// 不影响已创建实例。
type InstanceConfig struct {
	Options []InstanceOption
}

// ConnectConfig 是设备连接的输入:ADBPath 是 ADB 可执行文件路径
// (非空绝对路径,不做 shell 展开、不做存在性检查),Address 是
// 连接地址(非空),Config 是连接 profile 字符串——允许空串并
// 原样传空 C 字符串,由锁定 Core 选择 General;它不是 JSON,
// 也不传 nil()。
type ConnectConfig struct {
	ADBPath string
	Address string
	Config  string
}

// validateInstanceConfig 校验实例配置的内容与预算:key 不得为 0
// (Invalid)或重复,value 允许空串;全部 value 文本字节合计不超过
// ConfigBytes,合计项数不超过 ConfigEntries(先字节合计、后条目,
// 与 Runtime 配置预算的检查顺序一致)。失败返回带字段路径的
// ErrInvalidInput。
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
	// 先字节合计、后条目:与 runtime.go validateConfigBudget 的
	// 检查顺序一致,两处预算校验保持同一形状。
	if total > limits.ConfigBytes {
		return fmt.Errorf("maa: instance config text bytes total %d exceeds Limits.ConfigBytes %d: %w", total, limits.ConfigBytes, native.ErrInvalidInput)
	}
	if entries := uint64(len(cfg.Options)); entries > limits.ConfigEntries {
		return fmt.Errorf("maa: instance config entries %d exceeds Limits.ConfigEntries %d: %w", entries, limits.ConfigEntries, native.ErrInvalidInput)
	}
	return nil
}

// instanceConfigSnapshot 是校验通过后冻结的实例配置快照:options
// 已深拷贝。实例持有快照至关闭;调用者在方法返回后修改原配置
// 不影响快照。
type instanceConfigSnapshot struct {
	options []InstanceOption
}

// snapshotInstanceConfig 依次执行内容校验 → 预算校验 → 深拷贝
// 冻结。任何失败都返回带字段路径的 ErrInvalidInput。
func snapshotInstanceConfig(cfg InstanceConfig, limits Limits) (instanceConfigSnapshot, error) {
	if err := validateInstanceConfig(cfg, limits); err != nil {
		return instanceConfigSnapshot{}, err
	}
	options := make([]InstanceOption, len(cfg.Options))
	copy(options, cfg.Options)
	return instanceConfigSnapshot{options: options}, nil
}

// validateConnectConfig 校验连接输入:ADBPath 非空绝对路径、
// Address 非空、Config 允许空串(原样传空 C 字符串,不传 nil);
// 全部字符串合法 UTF-8、无内嵌 NUL、不超过单字符串预算,且
// ADBPath + Address + Config 的文本字节合计不超过 ConfigBytes。
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

// connectInputSnapshot 是校验通过后冻结的连接输入:全部字段为只读
// Go string,值复制即深拷贝。带 NUL 的 C buffer 由调用方在 raw
// 调用前经 cString 另行构造;Config 为空串时编码为单个 NUL 字节的
// 空 C 字符串,不传 nil。
type connectInputSnapshot struct {
	adbPath string
	address string
	config  string
}

// snapshotConnectInput 依次执行内容校验 → 预算校验 → 冻结。任何
// 失败都返回带字段路径的 ErrInvalidInput。
func snapshotConnectInput(cfg ConnectConfig, limits Limits) (connectInputSnapshot, error) {
	if err := validateConnectConfig(cfg, limits); err != nil {
		return connectInputSnapshot{}, err
	}
	return connectInputSnapshot{adbPath: cfg.ADBPath, address: cfg.Address, config: cfg.Config}, nil
}

// validateTaskInput 校验一次任务提交输入:taskType 非空、合法
// UTF-8、无内嵌 NUL 且不超过单字符串预算;params 必须是非空、
// 完整的 JSON object(validateJSONObject 的语义);taskType +
// params 的文本字节合计不超过 ConfigBytes。任务类型保持字符串,
// 不作业务枚举校验;params 只做语法与输入安全校验,不丢弃未知
// 字段、不经 map[string]any 往返,超过 2^53 的整数按原文字节保留。
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

// taskInputSnapshot 是校验通过后冻结的任务输入:taskType 为只读
// string,params 为逐字节 clone 的独立副本——提交方法返回后,
// 调用者可原地改写原 RawMessage 而不影响快照,不要求保持输入到
// Wait 完成。
type taskInputSnapshot struct {
	taskType string
	params   json.RawMessage
}

// snapshotTaskInput 依次执行校验 → 深拷贝冻结。任何失败都返回带
// 字段路径的 ErrInvalidInput。
func snapshotTaskInput(taskType string, params json.RawMessage, limits Limits) (taskInputSnapshot, error) {
	if err := validateTaskInput(taskType, params, limits); err != nil {
		return taskInputSnapshot{}, err
	}
	return taskInputSnapshot{taskType: taskType, params: bytes.Clone(params)}, nil
}
