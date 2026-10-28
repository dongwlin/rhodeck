// 本文件实现进程级实验查询(MapLevelKey、ItemName)的公开入口与
// native 注入接缝。两个查询对应 AsstCallerExtra.h 的
// ASST_WITH_EXTRA_CALLERS 条件导出:上游明确不提供稳定兼容保证,
// 本包以能力门表达——符号未随本次加载的原生库导出时(发行包默认
// 仅 Apple 构建打开该选项)一律返回 ErrUnsupported,不模拟、不降级。
//
// 两个查询都是进程级全局函数(无 handle):与 Runtime.Log 同一生命周期
// 纪律——门锁内复核 Runtime 处于 ready,native 调用在锁外执行,不经
// 实例状态与 raw 串行门禁;并发安全。返回的字符串/结构体一律是
// 独立 Go 快照:Core 借出的指针(实现写入 thread_local 存储,只在
// 本次调用与同线程的下一次调用之间有效)在 raw 返回后立即有界复制,
// 不 free、不缓存、不外泄视图;界内无 NUL 或非法 UTF-8 是协议破坏
// (ErrProtocol),不把矛盾证据伪装成成功。

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

// runtimeQuerier 是进程级实验查询的 native 注入边界(对照
// runtimeLogger 先例):生产实现转发 calls.go 的 raw 调用包装,fake
// 实现供受控用例记录输入与注入返回,不替换生产包的全局函数变量。
type runtimeQuerier interface {
	// mapLevelKey 对应 AsstGetMapLevelKey(key):按 C struct 成员
	// 顺序返回四个借用指针(stage_id/code/level_id/name),调用方
	// 必须立即有界复制;nil *byte 参数原样转发,未找到时四个返回值
	// 均为 nil。struct 按值返回的解包只发生在本接缝的生产实现内,
	// raw 同构类型不向公开层外泄。
	mapLevelKey(key *byte) (stageID, code, levelID, name *byte)
	// itemName 对应 AsstGetItemName(id):返回借用指针,契约恒非
	// nil;nil 返回由公开层按 ErrProtocol 处理。
	itemName(id *byte) *byte
}

// apiTableQuery 是 runtimeQuerier 的生产实现:转发 *native.APITable
// 的 GetMapLevelKey/GetItemName raw 调用包装并展开按值返回的 struct,
// 不解释返回值、不复制、不编造诊断;借用指针的消费责任在公开层。
type apiTableQuery struct {
	table *native.APITable
}

func (q apiTableQuery) mapLevelKey(key *byte) (stageID, code, levelID, name *byte) {
	raw := q.table.GetMapLevelKey(key)
	return raw.StageID, raw.Code, raw.LevelID, raw.Name
}

func (q apiTableQuery) itemName(id *byte) *byte { return q.table.GetItemName(id) }

// queryBorrowedStringLimit 是实验查询借用字符串的有界复制上限:
// 关卡标识与物品名称都是短资源字符串,4,096 字节覆盖全部合法内容;
// 界内无 NUL 即协议破坏。
const queryBorrowedStringLimit = 4096

// copyBorrowedString 有界复制一个 Core 借出的 NUL 终止字符串指针:
// 空指针返回空串(调用方自行决定 nil 的语义——AsstGetMapLevelKey 的
// 全 NULL struct 表示未找到);界内(含 limit+1 字节扫描范围)无 NUL
// 或非法 UTF-8 是协议破坏,返回包装 ErrProtocol 的错误。
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

// runtimeQueryGate 复核进程级查询的生命周期前置:ready 放行,
// terminated 返回包装 ErrRuntimeTerminated 的错误,其余状态防御拒绝
// (对照 Runtime.Log 的同一纪律)。放行时调用 call,在生命周期门锁外
// 执行。
func (rt *Runtime) runtimeQueryGate(what string, call func()) error {
	rt.gate.mu.Lock()
	var lifecycleErr error
	switch rt.gate.state {
	case lifecycleReady:
		// ready:正常放行。
	case lifecycleTerminated:
		lifecycleErr = rt.gate.terminatedErrLocked()
	default:
		// idle/reserved 对经 Open 获得的 Runtime 不可达,防御拒绝。
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
		// 借出指针只在本次调用与同线程的下一次调用之间有效(raw 已
		// 返回,同线程的下一次调用尚未发生);立即有界复制为 Go 数据,
		// C buffer 保活至 raw 返回。
		runtime.KeepAlive(keyBuf)
		// 任一字段协议破坏(界内无 NUL/非法 UTF-8)即整体失败,
		// 不返回部分结果;失败时 out 保持零值。
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
