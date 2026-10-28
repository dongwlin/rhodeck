// 本文件是进程级实验查询(MapLevelKey、ItemName)的受控用例:能力门
// 在进入生命周期门之前拒绝、借用指针的有界复制与协议校验、零值即
// 「未找到」的语义,以及生命周期终止的拒绝。全程注入 fake querier,
// 不加载任何原生库。

package maa

import (
	"errors"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// fakeRuntimeQuerier 是 runtimeQuerier 的受控 fake:记录原始输入,
// 注入返回的借用指针与 nil 组合。C 字符串缓冲区由 fake 持有至断言
// 结束(raw 调用同步返回后内容不再被改写)。
type fakeRuntimeQuerier struct {
	levelCalls  []string
	itemCalls   []string
	levelStages []*byte
	levelCodes  []*byte
	levelLevels []*byte
	levelNames  []*byte
	itemReturns []*byte
	nilLevelAll bool
	nilItemRet  bool
}

func newFakeRuntimeQuerier() *fakeRuntimeQuerier { return &fakeRuntimeQuerier{} }

func (q *fakeRuntimeQuerier) cBytes(s string) *byte {
	// 假 buffer 始终分配满扫描上限(queryBorrowedStringLimit+1 字节):
	// 契约允许借用指针的扫描跨出字符串本身(真实 Core 的指针指向
	// .rodata,字符串之后仍有可读数据);Go 堆上按实际文本长度分配的
	// 短 buffer 无法诚实地模拟该契约,且会在 -race(checkptr) 下被判
	// 为跨分配。零值填充即合法的 NUL 终止(对照 fakeSession.version
	// 的同一纪律)。
	b := make([]byte, queryBorrowedStringLimit+1)
	copy(b, s)
	return &b[0]
}

func (q *fakeRuntimeQuerier) mapLevelKey(key *byte) (stageID, code, levelID, name *byte) {
	q.levelCalls = append(q.levelCalls, cArgString(key))
	if q.nilLevelAll {
		return nil, nil, nil, nil
	}
	return q.levelStages[0], q.levelCodes[0], q.levelLevels[0], q.levelNames[0]
}

func (q *fakeRuntimeQuerier) itemName(id *byte) *byte {
	q.itemCalls = append(q.itemCalls, cArgString(id))
	if q.nilItemRet {
		return nil
	}
	// 按调用序号取应答:每次调用消耗 itemReturns 中的一项,调用超出
	// 注入序列时复用最后一项。
	i := len(q.itemCalls) - 1
	if i >= len(q.itemReturns) {
		i = len(q.itemReturns) - 1
	}
	return q.itemReturns[i]
}

// newQueryRuntime 构造带注入 querier 与能力位的受控 Runtime(ready
// 状态,独立生命周期门,不占用进程级门)。
func newQueryRuntime(t *testing.T, caps native.Capabilities, q runtimeQuerier) *Runtime {
	t.Helper()
	return &Runtime{
		caps:     caps,
		version:  "fake",
		nullSize: profileNullSize,
		limits:   defaultLimits(),
		gate:     &lifecycleGate{state: lifecycleReady},
		querier:  q,
	}
}

func TestMapLevelKeyQuery(t *testing.T) {
	t.Run("能力缺席返回 ErrUnsupported 且不触达 native", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, false), q)
		out, err := rt.MapLevelKey("6-16")
		if out != (MapLevelKey{}) || !errors.Is(err, native.ErrUnsupported) {
			t.Fatalf("能力缺席 = (%+v, %v), want 零值与 ErrUnsupported", out, err)
		}
		if len(q.levelCalls) != 0 {
			t.Errorf("能力缺席时 mapLevelKey 被调用 %d 次, want 0", len(q.levelCalls))
		}
	})

	t.Run("命中四字段转换与输入透传", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.levelStages = []*byte{q.cBytes("act1side_stg_3")}
		q.levelCodes = []*byte{q.cBytes("HV-3")}
		q.levelLevels = []*byte{q.cBytes("level_act1side_stage3")}
		q.levelNames = []*byte{q.cBytes("覆潮之地-3")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)

		out, err := rt.MapLevelKey("HV-3")
		if err != nil {
			t.Fatalf("MapLevelKey = %v, want nil", err)
		}
		want := MapLevelKey{StageID: "act1side_stg_3", Code: "HV-3",
			LevelID: "level_act1side_stage3", Name: "覆潮之地-3"}
		if out != want {
			t.Fatalf("MapLevelKey = %+v, want %+v(四字段按 C 成员顺序)", out, want)
		}
		if len(q.levelCalls) != 1 || q.levelCalls[0] != "HV-3" {
			t.Fatalf("收到 key = %v, want 原样透传", q.levelCalls)
		}
	})

	t.Run("全 NULL struct 转换为零值即未找到", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.nilLevelAll = true
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)
		out, err := rt.MapLevelKey("missing")
		if err != nil {
			t.Fatalf("未找到时错误 = %v, want nil(零值即未找到)", err)
		}
		if out != (MapLevelKey{}) {
			t.Fatalf("未找到 = %+v, want 零值 MapLevelKey", out)
		}
	})

	t.Run("界内无 NUL 是协议破坏", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		// 扫描窗口是 queryBorrowedStringLimit+1 字节:满长度分配且
		// 全程无 NUL,使「界内无 NUL」确定可辨。
		bad := make([]byte, queryBorrowedStringLimit+1)
		for i := range bad {
			bad[i] = 0x41
		}
		q.levelStages = []*byte{&bad[0]}
		q.levelCodes = []*byte{q.cBytes("ok")}
		q.levelLevels = []*byte{q.cBytes("ok")}
		q.levelNames = []*byte{q.cBytes("ok")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)
		out, err := rt.MapLevelKey("k")
		if out != (MapLevelKey{}) || !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("界内无 NUL = (%+v, %v), want 零值与 ErrProtocol", out, err)
		}
	})

	t.Run("内嵌 NUL 输入在纯 Go 校验拒绝", func(t *testing.T) {
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), newFakeRuntimeQuerier())
		if _, err := rt.MapLevelKey("a\x00b"); !errors.Is(err, native.ErrInvalidInput) {
			t.Fatalf("内嵌 NUL = %v, want ErrInvalidInput", err)
		}
	})

	t.Run("生命周期终止拒绝", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)
		rt.gate.terminate(nil)
		if _, err := rt.MapLevelKey("k"); !errors.Is(err, native.ErrRuntimeTerminated) {
			t.Fatalf("终止后 = %v, want ErrRuntimeTerminated", err)
		}
	})
}

func TestItemNameQuery(t *testing.T) {
	t.Run("能力缺席返回 ErrUnsupported 且不触达 native", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, false), q)
		name, err := rt.ItemName("0111")
		if name != "" || !errors.Is(err, native.ErrUnsupported) {
			t.Fatalf("能力缺席 = (%q, %v), want 空串与 ErrUnsupported", name, err)
		}
		if len(q.itemCalls) != 0 {
			t.Errorf("能力缺席时 itemName 被调用 %d 次, want 0", len(q.itemCalls))
		}
	})

	t.Run("空 Id 与已知 Id 的借用复制", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		unknown := q.cBytes("")
		q.itemReturns = []*byte{q.cBytes("Unknown"), q.cBytes("招聘许可")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), q)

		unknownName, err := rt.ItemName("")
		if err != nil || unknownName != "Unknown" {
			t.Fatalf("空 Id = (%q, %v), want Unknown 与 nil(空 Id 是合法查询)", unknownName, err)
		}
		name, err := rt.ItemName("4003")
		if err != nil || name != "招聘许可" {
			t.Fatalf("已知 Id = (%q, %v), want 招聘许可与 nil", name, err)
		}
		if len(q.itemCalls) != 2 || q.itemCalls[0] != "" || q.itemCalls[1] != "4003" {
			t.Fatalf("收到 id 序列 = %v, want 原样透传", q.itemCalls)
		}
		_ = unknown
	})

	t.Run("非法 UTF-8 是协议破坏", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.itemReturns = []*byte{q.cBytes("\xff\xfe")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), q)
		if _, err := rt.ItemName("x"); !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("非法 UTF-8 = %v, want ErrProtocol", err)
		}
	})

	t.Run("nil 返回是协议破坏", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.nilItemRet = true
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), q)
		if _, err := rt.ItemName("x"); !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("nil 返回 = %v, want ErrProtocol(C 契约恒非 NULL)", err)
		}
	})

	t.Run("终止后拒绝且错误可辨", func(t *testing.T) {
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), newFakeRuntimeQuerier())
		rt.gate.terminate(errors.New("boom"))
		_, err := rt.ItemName("x")
		if !errors.Is(err, native.ErrRuntimeTerminated) || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("终止后 = %v, want 附首因的 ErrRuntimeTerminated", err)
		}
	})
}
