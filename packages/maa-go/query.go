package maa

import (
	"bytes"
	"fmt"
	"runtime"
	"unicode/utf8"
	"unsafe"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// MapLevelKey 是一次关卡信息查询的结果(对应实验接口
// AsstGetMapLevelKey 的 struct AsstMapLevelKey):四个字段按 C 定义
// 顺序映射 stage_id/code/level_id/name,全部为独立 Go 字符串,可长期
// 持有、修改。未找到匹配关卡时四个字段均为空串——零值即「未找到」,
// binding 不引入额外的布尔或哨兵字段。实验接口:上游明确不提供稳定
// 兼容保证,字段集合可能随上游变动,依赖方需自担兼容风险。
type MapLevelKey struct {
	// StageID 是关卡的 stageId(资源内唯一标识)。
	StageID string
	// Code 是关卡代码(如 "1-7")。
	Code string
	// LevelID 是关卡的国际服关卡 ID。
	LevelID string
	// Name 是关卡的显示名称。
	Name string
}

type runtimeQuerier interface {
	mapLevelKey(key *byte) (stageID, code, levelID, name *byte)
	itemName(id *byte) *byte
}

type apiTableQuery struct {
	table *native.APITable
}

func (q apiTableQuery) mapLevelKey(key *byte) (stageID, code, levelID, name *byte) {
	raw := q.table.GetMapLevelKey(key)
	return raw.StageID, raw.Code, raw.LevelID, raw.Name
}

func (q apiTableQuery) itemName(id *byte) *byte { return q.table.GetItemName(id) }

const queryBorrowedStringLimit = 4096

func copyBorrowedString(what string, p *byte) (string, error) {
	if p == nil {
		return "", nil
	}
	span := unsafe.Slice(p, queryBorrowedStringLimit+1)
	end := bytes.IndexByte(span, 0)
	switch {
	case end < 0:
		return "", fmt.Errorf("maa: %s string has no NUL within %d bytes: %w", what, queryBorrowedStringLimit+1, native.ErrProtocol)
	case !utf8.Valid(span[:end]):
		return "", fmt.Errorf("maa: %s string is not valid UTF-8: %w", what, native.ErrProtocol)
	}
	return string(span[:end]), nil
}

func (rt *Runtime) runtimeQueryGate(what string, call func()) error {
	rt.gate.mu.Lock()
	var lifecycleErr error
	switch rt.gate.state {
	case lifecycleReady:
	case lifecycleTerminated:
		lifecycleErr = rt.gate.terminatedErrLocked()
	default:
		lifecycleErr = fmt.Errorf("maa: runtime is not ready, %s rejected: %w", what, native.ErrRuntimeInUse)
	}
	rt.gate.mu.Unlock()
	if lifecycleErr != nil {
		return lifecycleErr
	}
	call()
	return nil
}

// MapLevelKey 按关卡名、code、stageId 或 levelId 查找匹配关卡
// (AsstGetMapLevelKey,实验接口):key 原样透传(允许空串,合法
// UTF-8、无内嵌 NUL、不超单字符串预算),返回独立 Go 快照。未找到
// 匹配关卡时返回零值 MapLevelKey 与 nil 错误——零值即「未找到」,
// 不作为错误上报(C 契约:未找到返回全 NULL struct)。
//
// 能力门:符号未随本次加载的原生库导出时返回包装 ErrUnsupported 的
// 错误(上游发行包默认仅 Apple 构建导出实验接口)。实验接口:上游
// 明确不提供稳定兼容保证,不做任何降级或模拟。生命周期错误经错误链
// 可辨(ErrRuntimeTerminated);输入校验失败包装 ErrInvalidInput,
// 不进入 native。并发安全。
func (rt *Runtime) MapLevelKey(key string) (MapLevelKey, error) {
	if !rt.caps.MapLevelKey() {
		return MapLevelKey{}, fmt.Errorf("maa: AsstGetMapLevelKey is not available in this capability set: %w", native.ErrUnsupported)
	}
	if err := validateStringField("key", key, rt.limits.InputStringBytes, true); err != nil {
		return MapLevelKey{}, err
	}
	var out MapLevelKey
	var copyErr error
	err := rt.runtimeQueryGate("MapLevelKey", func() {
		k, keyBuf := cString(key)
		stageID, code, levelID, name := rt.querier.mapLevelKey(k)
		runtime.KeepAlive(keyBuf)
		fields := [4]struct {
			what string
			p    *byte
		}{{"MapLevelKey stage_id", stageID}, {"MapLevelKey code", code},
			{"MapLevelKey level_id", levelID}, {"MapLevelKey name", name}}
		texts := [4]string{}
		for i, f := range fields {
			s, err := copyBorrowedString(f.what, f.p)
			if err != nil {
				copyErr = err
				return
			}
			texts[i] = s
		}
		out = MapLevelKey{StageID: texts[0], Code: texts[1], LevelID: texts[2], Name: texts[3]}
	})
	if err != nil {
		return MapLevelKey{}, err
	}
	if copyErr != nil {
		return MapLevelKey{}, copyErr
	}
	return out, nil
}

// ItemName 按物品 Id 查找物品名称(AsstGetItemName,实验接口):id
// 原样透传(允许空串;C 契约:空 Id 返回 "Unknown",找不到的 Id 返回
// 空串——空串结果是正常查询结果,不转换为错误),返回独立 Go 字符串。
//
// 能力门与生命周期语义同 MapLevelKey;原生返回 nil 指针是协议破坏
// (C 契约恒非 NULL),返回包装 ErrProtocol 的错误,不伪装成空名称。
// 并发安全。
func (rt *Runtime) ItemName(id string) (string, error) {
	if !rt.caps.ItemName() {
		return "", fmt.Errorf("maa: AsstGetItemName is not available in this capability set: %w", native.ErrUnsupported)
	}
	if err := validateStringField("id", id, rt.limits.InputStringBytes, true); err != nil {
		return "", err
	}
	var (
		name     string
		copyErr  error
		nilNamed bool
	)
	err := rt.runtimeQueryGate("ItemName", func() {
		p, idBuf := cString(id)
		ret := rt.querier.itemName(p)
		runtime.KeepAlive(idBuf)
		if ret == nil {
			nilNamed = true
			return
		}
		name, copyErr = copyBorrowedString("ItemName", ret)
	})
	if err != nil {
		return "", err
	}
	if nilNamed {
		return "", fmt.Errorf("maa: AsstGetItemName returned a null pointer: %w", native.ErrProtocol)
	}
	if copyErr != nil {
		return "", copyErr
	}
	return name, nil
}
