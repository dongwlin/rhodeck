// 本文件是 Win32 窗口绑定(AttachWindow)的受控用例:能力门/平台门
// 在准入前拒绝、提交转发六参数与 block=false、以及
// AsyncCallInfo(what=AttachWindow)的早到接合与晚到结算。
//
// 注意平台边界:底层符号是 windowsOnly,Linux 生产运行时能力位恒关;
// 用例以 CapabilitiesForTest 注入能力位验证「门开启后的接线」——这只
// 证明 fake 接线与 what 关联语义,六参数形状的真实 ABI 由 native 层
// ABI fixture 的 queries/params 组证明,不据此声明非 Windows 平台可用。

package maa

import (
	"context"
	"errors"
	"testing"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// TestAttachWindowCapabilityGate 验证能力门:能力缺席时在准入前直接
// 返回 ErrUnsupported——不触达 caller、不占 pending 槽位。
func TestAttachWindowCapabilityGate(t *testing.T) {
	const handle = uintptr(0xF2A0)
	inst, rig := newAsyncRig(t, handle, nil)
	// 能力快照为零值:AsyncAttachWindow 关闭(非 Windows 生产运行时
	// 的真实形态)。
	op, err := inst.AttachWindow(0x10010, Win32ScreencapGDI, Win32InputPostMessage, Win32InputSeize)
	if op != nil {
		t.Fatalf("AttachWindow with a missing capability = (%+v, nil), want nil Operation", op)
	}
	if !errors.Is(err, native.ErrUnsupported) {
		t.Fatalf("error with a missing capability = %v, want ErrUnsupported", err)
	}
	if n := len(rig.caller.asyncCalls); n != 0 {
		t.Errorf("asyncAttachWindow called %d times with a missing capability, want 0 (rejected before admission)", n)
	}
	if n := int(pendingReservedOf(inst.inst.state)); n != 0 {
		t.Errorf("pending reservation with a missing capability = %d, want 0 (no slot consumed)", n)
	}
}

// TestAttachWindowSubmitsAndJoins 验证门开启后的提交接线:六参数逐字
// 转发(含 block=false)、返回 ID 关联 Operation、早到结果按预期
// what=AttachWindow 接合、晚到结果按 ID 结算。
func TestAttachWindowSubmitsAndJoins(t *testing.T) {
	const handle = uintptr(0xF2B0)

	t.Run("six parameters forwarded verbatim with early join", func(t *testing.T) {
		inst, rig := newAsyncRig(t, handle, nil)
		rig.rt.caps = native.CapabilitiesForTest(false, false, true, false, false)
		rig.caller.asyncRets = map[string]int32{"asyncAttachWindow": 7}
		token := rig.creator.exToken(0)
		s := inst.inst.state

		rig.caller.onAsyncAttach = func(uintptr) {
			deliverAsyncInfo(t, token, 7, "AttachWindow", `{"ret":true}`)
			waitUntil(t, "early result staged in the window", func() bool {
				v := earlyWindowOf(s)
				return v.open && v.has && v.id == 7 && v.what == "AttachWindow"
			})
		}

		op, err := inst.AttachWindow(0x20020, Win32ScreencapDXGIDesktopDup, Win32InputSendMessage|Win32InputPostMessage, Win32InputLegacyEvent)
		if err != nil {
			t.Fatalf("AttachWindow = %v, want nil", err)
		}
		if op.ID() != 7 {
			t.Fatalf("AttachWindow returned ID = %d, want 7", op.ID())
		}
		rec := rig.caller.asyncRecordByName(0, "asyncAttachWindow")
		if rec.attach == nil {
			t.Fatalf("the record is missing the attach detail: %+v", rec)
		}
		if rec.attach.handle != handle || rec.attach.hwnd != 0x20020 ||
			rec.attach.screencapMethod != uint64(Win32ScreencapDXGIDesktopDup) ||
			rec.attach.mouseMethod != uint64(Win32InputSendMessage|Win32InputPostMessage) ||
			rec.attach.keyboardMethod != uint64(Win32InputLegacyEvent) {
			t.Fatalf("AttachWindow forwarded parameters = %+v, want all six parameters passed through verbatim", rec.attach)
		}
		if rec.attach.block != 0 {
			t.Errorf("AttachWindow block = %d, want 0 (frozen block=false)", rec.attach.block)
		}
		res, err := waitOperationGuarded(t, op, context.Background())
		if err != nil {
			t.Fatalf("wait after the early join = %v, want nil", err)
		}
		if res.ID != 7 || res.What != "AttachWindow" || !res.OK {
			t.Fatalf("early-join result = %+v, want the AttachWindow completion terminal state", res)
		}
	})

	t.Run("late result settled by ID", func(t *testing.T) {
		inst, rig := newAsyncRig(t, handle, nil)
		rig.rt.caps = native.CapabilitiesForTest(false, false, true, false, false)
		token := rig.creator.exToken(0)

		op, err := inst.AttachWindow(0x30030, Win32ScreencapPrintWindow, Win32InputPostMessage, Win32InputNone)
		if err != nil {
			t.Fatalf("AttachWindow = %v, want nil", err)
		}
		deliverAsyncInfo(t, token, op.ID(), "AttachWindow", `{"ret":true,"cost":9}`)
		res, err := waitOperationGuarded(t, op, context.Background())
		if err != nil {
			t.Fatalf("late settlement wait = %v, want nil", err)
		}
		if res.What != "AttachWindow" || !res.OK || string(res.Details) != `{"ret":true,"cost":9}` {
			t.Fatalf("late settlement result = %+v, want the AttachWindow completion terminal state", res)
		}
	})
}
