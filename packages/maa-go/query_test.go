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
	t.Run("missing capability returns ErrUnsupported without touching native", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, false), q)
		out, err := rt.MapLevelKey("6-16")
		if out != (MapLevelKey{}) || !errors.Is(err, native.ErrUnsupported) {
			t.Fatalf("missing capability = (%+v, %v), want zero value and ErrUnsupported", out, err)
		}
		if len(q.levelCalls) != 0 {
			t.Errorf("mapLevelKey called %d times with a missing capability, want 0", len(q.levelCalls))
		}
	})

	t.Run("hit converts four fields and passes input through", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.levelStages = []*byte{q.cBytes("act1side_stg_3")}
		q.levelCodes = []*byte{q.cBytes("HV-3")}
		q.levelLevels = []*byte{q.cBytes("level_act1side_stage3")}
		q.levelNames = []*byte{q.cBytes("Undertide Land-3")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)

		out, err := rt.MapLevelKey("HV-3")
		if err != nil {
			t.Fatalf("MapLevelKey = %v, want nil", err)
		}
		want := MapLevelKey{StageID: "act1side_stg_3", Code: "HV-3",
			LevelID: "level_act1side_stage3", Name: "Undertide Land-3"}
		if out != want {
			t.Fatalf("MapLevelKey = %+v, want %+v (four fields in C member order)", out, want)
		}
		if len(q.levelCalls) != 1 || q.levelCalls[0] != "HV-3" {
			t.Fatalf("received key = %v, want it passed through unchanged", q.levelCalls)
		}
	})

	t.Run("all-NULL struct converts to the zero value meaning not found", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.nilLevelAll = true
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)
		out, err := rt.MapLevelKey("missing")
		if err != nil {
			t.Fatalf("error on not-found = %v, want nil (the zero value means not found)", err)
		}
		if out != (MapLevelKey{}) {
			t.Fatalf("not found = %+v, want a zero-value MapLevelKey", out)
		}
	})

	t.Run("no NUL within bounds is a protocol violation", func(t *testing.T) {
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
			t.Fatalf("no NUL within bounds = (%+v, %v), want zero value and ErrProtocol", out, err)
		}
	})

	t.Run("embedded NUL input rejected by pure Go validation", func(t *testing.T) {
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), newFakeRuntimeQuerier())
		if _, err := rt.MapLevelKey("a\x00b"); !errors.Is(err, native.ErrInvalidInput) {
			t.Fatalf("embedded NUL = %v, want ErrInvalidInput", err)
		}
	})

	t.Run("terminated lifecycle rejects", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, true, false), q)
		rt.gate.terminate(nil)
		if _, err := rt.MapLevelKey("k"); !errors.Is(err, native.ErrRuntimeTerminated) {
			t.Fatalf("after termination = %v, want ErrRuntimeTerminated", err)
		}
	})
}

func TestItemNameQuery(t *testing.T) {
	t.Run("missing capability returns ErrUnsupported without touching native", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, false), q)
		name, err := rt.ItemName("0111")
		if name != "" || !errors.Is(err, native.ErrUnsupported) {
			t.Fatalf("missing capability = (%q, %v), want empty string and ErrUnsupported", name, err)
		}
		if len(q.itemCalls) != 0 {
			t.Errorf("itemName called %d times with a missing capability, want 0", len(q.itemCalls))
		}
	})

	t.Run("borrowed copy for empty and known IDs", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		unknown := q.cBytes("")
		q.itemReturns = []*byte{q.cBytes("Unknown"), q.cBytes("Recruitment Permit")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), q)

		unknownName, err := rt.ItemName("")
		if err != nil || unknownName != "Unknown" {
			t.Fatalf("empty ID = (%q, %v), want Unknown and nil (an empty ID is a valid query)", unknownName, err)
		}
		name, err := rt.ItemName("4003")
		if err != nil || name != "Recruitment Permit" {
			t.Fatalf("known ID = (%q, %v), want Recruitment Permit and nil", name, err)
		}
		if len(q.itemCalls) != 2 || q.itemCalls[0] != "" || q.itemCalls[1] != "4003" {
			t.Fatalf("received id sequence = %v, want it passed through unchanged", q.itemCalls)
		}
		_ = unknown
	})

	t.Run("invalid UTF-8 is a protocol violation", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.itemReturns = []*byte{q.cBytes("\xff\xfe")}
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), q)
		if _, err := rt.ItemName("x"); !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("invalid UTF-8 = %v, want ErrProtocol", err)
		}
	})

	t.Run("nil return is a protocol violation", func(t *testing.T) {
		q := newFakeRuntimeQuerier()
		q.nilItemRet = true
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), q)
		if _, err := rt.ItemName("x"); !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("nil return = %v, want ErrProtocol (the C contract is never NULL)", err)
		}
	})

	t.Run("rejects after termination with a distinguishable error", func(t *testing.T) {
		rt := newQueryRuntime(t, native.CapabilitiesForTest(false, false, false, false, true), newFakeRuntimeQuerier())
		rt.gate.terminate(errors.New("boom"))
		_, err := rt.ItemName("x")
		if !errors.Is(err, native.ErrRuntimeTerminated) || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("after termination = %v, want ErrRuntimeTerminated carrying the first cause", err)
		}
	})
}
