// 本文件是有界事件分发器与故障诊断的受控用例:验证预算隔离、消费者停读
// 时的非阻塞行为、溢出故障与丢失计数、未知消息的原文保留、排空与结算
// 纪律,以及 slot 原语和首因前缀的深拷贝。

package maa

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// dispatchBudgets 是事件分发器的五项预算(冻结的字段集合):
// 仅供用例逐项收紧单一预算,其余保持互不设限。
type dispatchBudgets struct {
	callbackBytes  uint64
	internalEvents uint64
	internalBytes  uint64
	publicEvents   uint64
	publicBytes    uint64
}

// generousBudgets 返回各项互不设限的基线预算:单条 1024 字节(容纳
// 全部用例 payload)、内部与公开各 64 事件、各 4096 字节。预算越界用
// 例只收紧自己关注的一项,保证越界确由该项触发,不被相邻预算遮蔽。
func generousBudgets() dispatchBudgets {
	return dispatchBudgets{
		callbackBytes:  1024,
		internalEvents: 64,
		internalBytes:  4096,
		publicEvents:   64,
		publicBytes:    4096,
	}
}

// newTestDispatcher 构造已发布 ready 的实例状态与其事件分发器:
// settle 可为 nil(不关心结算通知的用例)。只建立状态与通道,不启动
// goroutine;是否 start 由用例自行决定(纯预算用例无需分发循环)。
func newTestDispatcher(t *testing.T, settle func(completionInfo), b dispatchBudgets) (*eventDispatcher, *instanceState) {
	t.Helper()
	s := newInstanceState(0x0D17000, 128, 1_048_576)
	s.publishReady()
	d := newEventDispatcher(s, settle, b.callbackBytes, b.internalEvents, b.internalBytes, b.publicEvents, b.publicBytes)
	return d, s
}

// enqueue 是「admit + commit」的快路径:按预算准入 payload 并立即
// 入队,供正向路径用例复用;准入失败直接 Fatal,commit 违约由实现
// 自己 panic 暴露。
func enqueue(t *testing.T, d *eventDispatcher, messageID MessageID, payload []byte) {
	t.Helper()
	slot, err := d.admit(messageID, len(payload))
	if err != nil {
		t.Fatalf("admitting message %d must not fail: %v", messageID, err)
	}
	slot.commit(payload)
}

// internalBudgetHold 在分发器锁内读取内部预算占用(计数、字节):
// 白盒诊断读取,不绕过锁。
func internalBudgetHold(d *eventDispatcher) (uint64, uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count, d.bytes
}

// mustReadEvent 从公开通道读取一条事件:分发器对读取中的消费者只会
// 尽快投递,超时即实现挂死,按防挂死上限兜底失败。已关闭通道中的
// 缓冲事件仍可正常读取。
func mustReadEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(hangGuard):
		t.Fatal("timed out reading a public event: the dispatcher appears blocked or not delivering (hang guard triggered)")
		return Event{}
	}
}

// mustDrain 以防挂死上限兜底执行排空:drain 以公开通道关闭为界返回,
// 外部消费者完全停读时也必须有限步完成;若实现在排空时等待读者,
// 本辅助超时失败而不是用例挂死。
func mustDrain(t *testing.T, d *eventDispatcher) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		d.drain()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hangGuard):
		t.Fatal("drain did not return within the hang guard limit: draining appears to be waiting for an external consumer")
	}
}

// closeAndDrain 走「关门 → 排空」的标准收尾顺序(第 4、5 步):
// 供正向路径用例终结分发循环;drain 的前置契约(已关门)由该顺序满足。
func closeAndDrain(t *testing.T, d *eventDispatcher) {
	t.Helper()
	d.closeIntake()
	mustDrain(t, d)
}

// settleRecorder 记录结算通知的全部调用:calls 保存副本供逐字段断言,
// count 统计次数(恰好一次/绝不被调用的断言),signal 每次调用发送
// 一个空结构供用例观察结算已发生。缓冲容量由用例按预期调用数给定。
type settleRecorder struct {
	count  atomic.Int32
	mu     sync.Mutex
	calls  []completionInfo
	signal chan struct{}
}

// newSettleRecorder 构造结算记录器:buffered 是信号缓冲容量,应不小
// 于该用例预期的最大结算次数。
func newSettleRecorder(buffered int) *settleRecorder {
	return &settleRecorder{signal: make(chan struct{}, buffered)}
}

// record 是接入 dispatcher 的结算通知:登记副本、计数并发信号。
func (r *settleRecorder) record(info completionInfo) {
	r.mu.Lock()
	r.calls = append(r.calls, info)
	r.mu.Unlock()
	r.count.Add(1)
	r.signal <- struct{}{}
}

// total 返回结算通知被调用的总次数。
func (r *settleRecorder) total() int { return int(r.count.Load()) }

// all 返回全部结算副本的独立切片。
func (r *settleRecorder) all() []completionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]completionInfo(nil), r.calls...)
}

// assertOverflowClass 断言 err 非空、沿错误链可达
// native.ErrEventOverflow,且不误标为相邻哨兵 ErrProtocol。
func assertOverflowClass(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("a budget overflow must return an error")
	}
	if !errors.Is(err, native.ErrEventOverflow) {
		t.Errorf("errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
	}
	if errors.Is(err, native.ErrProtocol) {
		t.Errorf("a budget overflow must not be mislabeled as ErrProtocol: %v", err)
	}
}

// TestEventDispatchBudgetOverflowReportsFault 验证「内部队列
// 和公开缓冲均计入预算,事件数或总字节数越界会明确报告故障」:单条
// CallbackBytes、内部事件计数、内部字节、公开缓冲计数与公开字节五
// 个预算各自独立越界(子用例互不混用),每次越界都以
// errors.Is(native.ErrEventOverflow) 明确报告故障,入口不阻塞、不预
// 留任何额度。
func TestEventDispatchBudgetOverflowReportsFault(t *testing.T) {
	t.Run("single message exceeds CallbackBytes", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 8
		d, s := newTestDispatcher(t, nil, b)

		slot, err := d.admit(MessageID(100), 9)
		assertOverflowClass(t, err)
		if slot.d != nil {
			t.Error("a rejected admission must not return a usable slot")
		}
		// 不预留任何额度:计数与字节都保持 0。
		if count, hold := internalBudgetHold(d); count != 0 || hold != 0 {
			t.Errorf("internal budget held after rejection = (%d, %d), want (0, 0)", count, hold)
		}
		// 越界按溢出契约上报故障并计 1 条丢失;故障通知独立关闭。
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
		if n := s.currentLostEvents(); n != 1 {
			t.Errorf("lost events count = %d, want 1", n)
		}
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("the first overflow must close the one-shot fault notification")
		}
		// 恰好等于 CallbackBytes 的消息仍在界内:准入成功(随后退回)。
		boundary, err := d.admit(MessageID(100), 8)
		if err != nil {
			t.Fatalf("admission of exactly CallbackBytes must not fail: %v", err)
		}
		boundary.abort()
	})

	t.Run("internal event count overflow", func(t *testing.T) {
		b := generousBudgets()
		b.internalEvents = 2
		d, s := newTestDispatcher(t, nil, b)

		first, err := d.admit(MessageID(101), 4)
		if err != nil {
			t.Fatalf("admission #1 must not fail: %v", err)
		}
		second, err := d.admit(MessageID(101), 4)
		if err != nil {
			t.Fatalf("admission #2 must not fail: %v", err)
		}
		_, err = d.admit(MessageID(101), 4)
		assertOverflowClass(t, err)
		// 已持有的预留不受拒绝影响:计数仍为 2,字节仍为 8。
		if count, hold := internalBudgetHold(d); count != 2 || hold != 8 {
			t.Errorf("internal budget held after rejection = (%d, %d), want (2, 8)", count, hold)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
		// 退回预留后同一预算可再次准入:拒绝不污染预算。
		first.abort()
		second.abort()
		again, err := d.admit(MessageID(101), 4)
		if err != nil {
			t.Fatalf("admission after reservations are returned must not fail: %v", err)
		}
		again.abort()
	})

	t.Run("internal bytes overflow", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 8
		b.internalBytes = 16
		d, s := newTestDispatcher(t, nil, b)

		var slots []eventSlot
		for i := 0; i < 4; i++ {
			slot, err := d.admit(MessageID(102), 4)
			if err != nil {
				t.Fatalf("admission #%d of 4 bytes must not fail (byte budget not full): %v", i+1, err)
			}
			slots = append(slots, slot)
		}
		// 4 条恰好装满 16 字节字节预算;事件计数预算(64)远未触顶,
		// 第 5 条越界确由字节触发。
		if count, hold := internalBudgetHold(d); count != 4 || hold != 16 {
			t.Fatalf("internal budget held after filling = (%d, %d), want (4, 16)", count, hold)
		}
		_, err := d.admit(MessageID(102), 4)
		assertOverflowClass(t, err)
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
		for i := range slots {
			slots[i].abort()
		}
		if count, hold := internalBudgetHold(d); count != 0 || hold != 0 {
			t.Errorf("internal budget held after returning all = (%d, %d), want (0, 0)", count, hold)
		}
	})

	t.Run("public buffer event count overflow (consumer stops reading)", func(t *testing.T) {
		b := generousBudgets()
		b.publicEvents = 2
		d, s := newTestDispatcher(t, nil, b)
		d.start()

		// 持续 admit+commit 5 条:循环在主 goroutine 内走完即证明入口
		// 不阻塞;分发器对满缓冲非阻塞投递,多出的按溢出契约计丢失。
		// 消费者全程停读。
		for i := 0; i < 5; i++ {
			enqueue(t, d, MessageID(200), []byte(`{"k":1}`))
		}
		closeAndDrain(t, d)

		if n := len(d.events()); n != 2 {
			t.Errorf("events left in the public buffer = %d, want exactly PublicEvents 2", n)
		}
		if lost := s.currentLostEvents(); lost != 3 {
			t.Errorf("lost events count = %d, want 3", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
		// 计数越界与字节越界互不混用:本用例字节预算(4096)远未触顶。
	})

	t.Run("public buffer bytes overflow (consumer stops reading)", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 16
		b.publicBytes = 16
		d, s := newTestDispatcher(t, nil, b)
		d.start()

		// 7 字节/条:前 2 条共 14 字节进入缓冲;第 3 条将使计费 21 字节
		// 越过 PublicBytes 16,按字节判满。事件计数预算(64)远未触顶。
		for i := 0; i < 5; i++ {
			enqueue(t, d, MessageID(201), []byte(`{"k":1}`))
		}
		closeAndDrain(t, d)

		if n := len(d.events()); n != 2 {
			t.Errorf("events left in the public buffer = %d, want 2 (exactly 14/16 bytes fit)", n)
		}
		if lost := s.currentLostEvents(); lost != 3 {
			t.Errorf("lost events count = %d, want 3", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
	})
}

// TestEventDispatchBoundedWhenConsumerStopsReading 验证「外
// 部完全停读 Events 时,事件入口不无限阻塞,故障通知仍可观察」:消费
// 者从不读取,入口持续 admit+commit 远超公开预算的消息量,主
// goroutine 不阻塞;一次性故障通知独立于事件队列关闭;丢失计数增加;
// 排空仍有限步完成且已缓冲事件完好。
func TestEventDispatchBoundedWhenConsumerStopsReading(t *testing.T) {
	b := generousBudgets()
	b.publicEvents = 3
	d, s := newTestDispatcher(t, nil, b)
	d.start()

	const total = 12
	for i := 0; i < total; i++ {
		enqueue(t, d, MessageID(210), []byte(fmt.Sprintf(`{"n":%d}`, i)))
	}
	// 入口与投递全程在主 goroutine 内完成:消费者停读既不阻塞入口也
	// 不阻塞分发器;若实现违反有界性,mustDrain 在防挂死上限处失败,
	// 而不是本用例挂死。
	closeAndDrain(t, d)

	// 故障通知独立于事件队列可观察:绝不依赖 Events 还有空间。
	select {
	case <-s.faultNotify():
	default:
		t.Fatal("the overflow fault caused by a consumer stopping reading did not close the fault notification")
	}
	if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
		t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
	}
	// 恰好 PublicEvents 条已入缓冲,其余全部计入丢失。
	if n := len(d.events()); n != 3 {
		t.Errorf("events left in the public buffer = %d, want 3", n)
	}
	if lost := s.currentLostEvents(); lost != total-3 {
		t.Errorf("lost events count = %d, want %d", lost, total-3)
	}
	// 关闭后已缓冲事件仍可读:序号 1,2,3、原文完整、FIFO。
	for i := 0; i < 3; i++ {
		ev := mustReadEvent(t, d.events())
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("buffered event %d sequence = %d, want %d", i+1, ev.Sequence, i+1)
		}
		if want := fmt.Sprintf(`{"n":%d}`, i); string(ev.JSON) != want {
			t.Fatalf("buffered event %d raw JSON = %s, want %s", i+1, ev.JSON, want)
		}
	}
	if ev, ok := <-d.events(); ok {
		t.Fatalf("the public channel must be closed after draining, but read %+v", ev)
	}
}

// TestEventDispatchLedgerReconcilesAfterConsumerReads 钉住公开
// 账本对账:消费者中途读走已缓冲事件后,下次投递前按 FIFO 对账删除
// 已确认接收的长度记录并扣减字节,公开预算得以回收续投;并钉住
// PublicBytes 恰好装满允许、再加入才越界的边界。
func TestEventDispatchLedgerReconcilesAfterConsumerReads(t *testing.T) {
	t.Run("delivery continues successfully after the consumer reads", func(t *testing.T) {
		b := generousBudgets()
		b.publicEvents = 2
		d, s := newTestDispatcher(t, nil, b)
		d.start()

		// 前 2 条装满公开缓冲,随后被消费者读走。
		for i := 0; i < 2; i++ {
			enqueue(t, d, MessageID(240), []byte(fmt.Sprintf(`{"i":%d}`, i)))
		}
		for i := 0; i < 2; i++ {
			ev := mustReadEvent(t, d.events())
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("event %d sequence = %d, want %d", i+1, ev.Sequence, i+1)
			}
		}
		// 缓冲已空;账本仍保守计费这 2 条,直到下次投递前对账回收。
		// 续投 2 条必须全部成功:对账删除最老 2 条长度记录并扣减字节。
		for i := 2; i < 4; i++ {
			enqueue(t, d, MessageID(240), []byte(fmt.Sprintf(`{"i":%d}`, i)))
		}
		closeAndDrain(t, d)

		// 续投的 2 条全部进入缓冲且完好;全程无丢失、无故障。
		for i := 2; i < 4; i++ {
			ev := mustReadEvent(t, d.events())
			if want := fmt.Sprintf(`{"i":%d}`, i); string(ev.JSON) != want {
				t.Fatalf("redelivered event raw JSON = %s, want %s", ev.JSON, want)
			}
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("redelivered event sequence = %d, want %d", ev.Sequence, i+1)
			}
		}
		if ev, ok := <-d.events(); ok {
			t.Fatalf("the public channel must be closed after draining, but read %+v", ev)
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("lost events count = %d, want 0 (budget reclaimed after reading, redelivered events must not be lost)", lost)
		}
		select {
		case <-s.faultNotify():
			t.Fatal("redelivery after reading must not trigger an overflow fault")
		default:
		}
	})

	t.Run("PublicBytes exactly full is allowed, adding more overflows", func(t *testing.T) {
		b := generousBudgets()
		b.publicBytes = 14 // 恰为 2 条 7 字节 payload 之和
		d, s := newTestDispatcher(t, nil, b)
		d.start()

		// 前 2 条使账本计费恰好到达 PublicBytes:必须允许(边界在内,
		// 检查为严格大于)。消费者停读。
		for i := 0; i < 2; i++ {
			enqueue(t, d, MessageID(241), []byte(`{"k":1}`))
		}
		// 第 3 条将使计费 21 字节越过 14:按字节判满拒绝,事件计数预算
		// (64)远未触顶,越界确由字节触发。
		enqueue(t, d, MessageID(241), []byte(`{"k":1}`))
		closeAndDrain(t, d)

		// 恰好装满的 2 条完好入缓冲(序号 1,2);只有越界的第 3 条丢失。
		if n := len(d.events()); n != 2 {
			t.Errorf("events left in the public buffer = %d, want 2 (both events exactly at the limit must be accepted)", n)
		}
		for i := 0; i < 2; i++ {
			ev := mustReadEvent(t, d.events())
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("buffered event %d sequence = %d, want %d", i+1, ev.Sequence, i+1)
			}
		}
		if lost := s.currentLostEvents(); lost != 1 {
			t.Errorf("lost events count = %d, want 1 (only the overflowing third one is lost)", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
	})
}

// TestInstanceFaultFirstOverflowStickyAndCallRejection 验证
// 「首次溢出保留 ErrEventOverflow 与丢失计数,新业务调用被拒绝」:
// 公开缓冲溢出的首因沿错误链可达 native.ErrEventOverflow,faulted 后
// admitCall 拒绝并共享同一错误身份,重复故障只累计丢失、首因与已保
// 留诊断不变。
func TestInstanceFaultFirstOverflowStickyAndCallRejection(t *testing.T) {
	// setup 构造 PublicEvents=1 的分发器并注入 2 条消息:第 1 条占满
	// 公开缓冲,第 2 条必然投递失败产生首次溢出。
	setup := func(t *testing.T) (*eventDispatcher, *instanceState, []byte) {
		t.Helper()
		b := generousBudgets()
		b.publicEvents = 1
		d, s := newTestDispatcher(t, nil, b)
		d.start()
		return d, s, []byte(`{"k":1}`)
	}

	t.Run("first fault keeps ErrEventOverflow and the lost events count is readable", func(t *testing.T) {
		d, s, payload := setup(t)
		for i := 0; i < 3; i++ {
			enqueue(t, d, MessageID(220), payload)
		}
		closeAndDrain(t, d)

		first := s.currentFault()
		if first == nil {
			t.Fatal("the first overflow must record a fault")
		}
		if !errors.Is(first, native.ErrEventOverflow) {
			t.Errorf("errors.Is(first fault, native.ErrEventOverflow) = false; first fault = %v", first)
		}
		snap := s.faultDiagnostics()
		if snap.lostEvents != 2 {
			t.Errorf("lost events count = %d, want 2 (deliveries #2 and #3 failed)", snap.lostEvents)
		}
		if snap.rawPrefix != nil {
			t.Errorf("an overflow fault must not carry a raw prefix, got %q", snap.rawPrefix)
		}
		if got := s.currentPhase(); got != phaseFaulted {
			t.Fatalf("phase after the first overflow = %d, want faulted", got)
		}
	})

	t.Run("admitCall rejected after faulted and reaches overflow along the chain", func(t *testing.T) {
		d, s, payload := setup(t)
		for i := 0; i < 2; i++ {
			enqueue(t, d, MessageID(221), payload)
		}
		closeAndDrain(t, d)

		_, err := s.admitCall("new call after overflow")
		if err == nil {
			t.Fatal("a faulted instance must reject new business calls")
		}
		if !errors.Is(err, native.ErrEventOverflow) {
			t.Errorf("errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
		}
		// 拒绝与实例首因共享同一错误身份。
		if !errors.Is(err, s.currentFault()) {
			t.Errorf("errors.Is(err, first fault) = false; err = %v, first fault = %v", err, s.currentFault())
		}
		if errors.Is(err, native.ErrClosing) || errors.Is(err, native.ErrClosed) {
			t.Errorf("the overflow rejection must not be mislabeled as closing/closed: %v", err)
		}
	})

	t.Run("repeated faults only accumulate losses, first fault and prefix unchanged", func(t *testing.T) {
		d, s, payload := setup(t)
		for i := 0; i < 3; i++ {
			enqueue(t, d, MessageID(222), payload)
		}
		closeAndDrain(t, d)

		before := s.faultDiagnostics()
		if before.lostEvents != 2 {
			t.Fatalf("repeated overflows should accumulate lost = 2, got %d", before.lostEvents)
		}
		// 后到的异类故障与带前缀记录都不得覆盖首因、前缀或丢失计数。
		s.recordFaultWithDetail(
			fmt.Errorf("late-arriving protocol fault: %w", native.ErrProtocol),
			[]byte("late prefix"),
		)
		after := s.faultDiagnostics()
		if !errors.Is(after.firstFault, native.ErrEventOverflow) {
			t.Errorf("the first fault was overwritten by a late-arriving fault: %v", after.firstFault)
		}
		if errors.Is(after.firstFault, native.ErrProtocol) {
			t.Errorf("a late-arriving fault mixed into the first fault: %v", after.firstFault)
		}
		if !bytes.Equal(after.rawPrefix, before.rawPrefix) {
			t.Errorf("the prefix was overwritten by a late record = %q, want it kept as %q", after.rawPrefix, before.rawPrefix)
		}
		if after.lostEvents != before.lostEvents {
			t.Errorf("lost events count after repeated records = %d, want it kept as %d (repeated faults only accumulate losses, diagnostics unchanged)", after.lostEvents, before.lostEvents)
		}
	})
}

// TestEventDispatchUnknownAndInvalidMessages 验证「未知消息
// 及字段可观察;非法 JSON 保留原文,不能将无法关联的完成信息视为成
// 功」:未知 MessageID/未知 what/未知字段按原样逐字节交付且不故障;
// 非法 UTF-8/JSON 以 ErrProtocol 故障、保留至多 4096 字节原文前缀且
// 原文仍尝试交付;AsyncCallInfo 关联字段异常绝不调用结算通知;合法
// 完成恰好结算一次、字段精确且先结算后投递。
func TestEventDispatchUnknownAndInvalidMessages(t *testing.T) {
	t.Run("unknown MessageID and unknown fields delivered verbatim", func(t *testing.T) {
		d, s := newTestDispatcher(t, nil, generousBudgets())
		d.start()

		want := []byte(`{"unknown":[1,2,{"deep":null}],"what":"Whatever","big":9007199254740993}`)
		original := bytes.Clone(want)
		slot, err := d.admit(MessageID(99), len(original))
		if err != nil {
			t.Fatalf("admitting an unknown message must not fail: %v", err)
		}
		slot.commit(original)
		// commit 已逐字节克隆:此后原地改写调用者缓冲不影响队列记录。
		for i := range original {
			original[i] = 'x'
		}

		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, want) {
			t.Fatalf("unknown message raw JSON = %s, want byte-for-byte equal %s", ev.JSON, want)
		}
		if ev.MessageID != MessageID(99) {
			t.Errorf("MessageID = %d, want 99", ev.MessageID)
		}
		if ev.Sequence != 1 {
			t.Errorf("sequence = %d, want 1 (unknown messages are also assigned normal sequence numbers)", ev.Sequence)
		}
		closeAndDrain(t, d)
		// 未知消息与未知字段不进故障流程:不故障、不丢失。
		select {
		case <-s.faultNotify():
			t.Fatal("unknown messages and unknown fields must not trigger a fault")
		default:
		}
		if n := s.currentLostEvents(); n != 0 {
			t.Errorf("lost events count = %d, want 0", n)
		}
	})

	t.Run("legal completion with unknown what settles and is delivered verbatim", func(t *testing.T) {
		rec := newSettleRecorder(2)
		d, s := newTestDispatcher(t, rec.record, generousBudgets())
		d.start()

		payload := []byte(`{"what":"SomeFutureWhat","async_call_id":7,"details":{"ret":true},"extra":"kept"}`)
		enqueue(t, d, asyncCallInfoMessageID, payload)
		closeAndDrain(t, d)

		if got := rec.total(); got != 1 {
			t.Fatalf("settle notification called %d times, want 1 (unknown what is still delivered verbatim to the settler)", got)
		}
		if got := rec.all()[0]; got.ID != 7 || got.What != "SomeFutureWhat" || !got.OK {
			t.Fatalf("settlement info = %+v, want {7 SomeFutureWhat true}", got)
		}
		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, payload) {
			t.Fatalf("event raw JSON = %s, want %s with unknown what and unknown fields preserved byte-for-byte", ev.JSON, payload)
		}
		select {
		case <-s.faultNotify():
			t.Fatal("an unknown what must not trigger a fault")
		default:
		}
	})

	t.Run("invalid UTF-8 and invalid JSON keep the raw prefix and still deliver the raw data", func(t *testing.T) {
		cases := []struct {
			name string
			raw  []byte
		}{
			// 5000 字节非法 UTF-8(孤立续字节):前缀必须截取到 4096。
			{"invalid UTF-8", bytes.Repeat([]byte{0xA5}, 5000)},
			{"invalid JSON", []byte(`{"what":"w","async_call_id":1,`)},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				b := generousBudgets()
				b.callbackBytes = 8192
				b.internalBytes = 16384
				b.publicBytes = 16384
				rec := newSettleRecorder(1)
				d, s := newTestDispatcher(t, rec.record, b)
				d.start()

				original := bytes.Clone(tc.raw)
				enqueue(t, d, asyncCallInfoMessageID, tc.raw)
				// 原文仍按公开事件预算尝试交付(步骤 1 故障不取消步骤 4)。
				ev := mustReadEvent(t, d.events())
				if !bytes.Equal(ev.JSON, original) {
					t.Fatal("invalid raw data must be delivered byte-for-byte, not truncated or replaced")
				}
				closeAndDrain(t, d)

				// 无法关联的完成信息绝不结算为成功:结算通知零调用。
				if got := rec.total(); got != 0 {
					t.Fatalf("an invalid message must not call the settle notification, got %d calls", got)
				}
				// 协议故障 + 原文前缀(至多 faultRawPrefixBytes 字节)。
				if !errors.Is(s.currentFault(), native.ErrProtocol) {
					t.Errorf("currentFault() = %v, want errors.Is native.ErrProtocol", s.currentFault())
				}
				wantLen := len(original)
				if wantLen > faultRawPrefixBytes {
					wantLen = faultRawPrefixBytes
				}
				snap := s.faultDiagnostics()
				if len(snap.rawPrefix) != wantLen {
					t.Fatalf("raw prefix length = %d, want %d", len(snap.rawPrefix), wantLen)
				}
				if !bytes.Equal(snap.rawPrefix, original[:wantLen]) {
					t.Fatal("the raw prefix content does not match the raw data prefix")
				}
				// 深拷贝:改写调用者原文不得穿透已保存前缀。
				for i := range tc.raw {
					tc.raw[i] = 0x00
				}
				if again := s.faultDiagnostics(); !bytes.Equal(again.rawPrefix, original[:wantLen]) {
					t.Fatal("overwriting the caller's raw data affected the saved prefix: the prefix is not a deep copy")
				}
				// 交付成功:该条不计丢失。
				if n := s.currentLostEvents(); n != 0 {
					t.Errorf("lost events count = %d, want 0 (the raw data was fully delivered)", n)
				}
			})
		}
	})

	t.Run("AsyncCallInfo with malformed correlation fields faults and never settles", func(t *testing.T) {
		cases := []struct{ name, payload string }{
			{"top level is not an object", `["not","an","object"]`},
			{"missing what", `{"async_call_id":1,"details":{"ret":true}}`},
			{"missing async_call_id", `{"what":"w","details":{"ret":true}}`},
			{"missing details", `{"what":"w","async_call_id":1}`},
			{"missing details.ret", `{"what":"w","async_call_id":1,"details":{"cost":1}}`},
			{"duplicate what", `{"what":"a","what":"b","async_call_id":1,"details":{"ret":true}}`},
			{"duplicate async_call_id", `{"what":"w","async_call_id":1,"async_call_id":2,"details":{"ret":true}}`},
			{"duplicate details", `{"what":"w","async_call_id":1,"details":{"ret":true},"details":{"ret":false}}`},
			{"duplicate ret", `{"what":"w","async_call_id":1,"details":{"ret":true,"ret":false}}`},
			{"id above the int32 upper bound", `{"what":"w","async_call_id":2147483648,"details":{"ret":true}}`},
			{"id below the int32 lower bound", `{"what":"w","async_call_id":-2147483649,"details":{"ret":true}}`},
			{"id not an exact integer", `{"what":"w","async_call_id":1.5,"details":{"ret":true}}`},
			{"what wrong type", `{"what":1,"async_call_id":1,"details":{"ret":true}}`},
			{"id wrong type", `{"what":"w","async_call_id":"1","details":{"ret":true}}`},
			{"details wrong type", `{"what":"w","async_call_id":1,"details":[]}`},
			{"ret wrong type", `{"what":"w","async_call_id":1,"details":{"ret":"true"}}`},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				rec := newSettleRecorder(1)
				d, s := newTestDispatcher(t, rec.record, generousBudgets())
				d.start()

				enqueue(t, d, asyncCallInfoMessageID, []byte(tc.payload))
				// 原文仍按公开事件预算交付,供诊断观察。
				ev := mustReadEvent(t, d.events())
				if !bytes.Equal(ev.JSON, []byte(tc.payload)) {
					t.Fatal("raw data with malformed correlation fields must be delivered byte-for-byte")
				}
				closeAndDrain(t, d)

				// 无法关联的完成信息不得视为成功:绝不调用结算通知。
				if got := rec.total(); got != 0 {
					t.Fatalf("the settle notification was called %d times, want 0 (uncorrelatable completion info must not settle)", got)
				}
				if !errors.Is(s.currentFault(), native.ErrProtocol) {
					t.Errorf("currentFault() = %v, want errors.Is native.ErrProtocol", s.currentFault())
				}
				// 交付成功:该条不计丢失。
				if n := s.currentLostEvents(); n != 0 {
					t.Errorf("lost events count = %d, want 0", n)
				}
			})
		}
	})

	t.Run("legal completion settles exactly once and settles before delivering", func(t *testing.T) {
		payload := []byte(`{"what":"AsyncCallFinished","async_call_id":2147483647,` +
			`"details":{"ret":true,"cost":12.5},"big":9007199254740993,"future":{"nested":[1,2]}}`)

		var (
			mu    sync.Mutex
			calls []completionInfo
		)
		// 阻塞式结算:分发器在结算通知返回前被挡住,「先结算后投递」
		// 由此获得无条件断言(单分发器 goroutine 不可能提前投递)。
		entered := make(chan struct{})
		release := make(chan struct{})
		settle := func(info completionInfo) {
			mu.Lock()
			calls = append(calls, info)
			mu.Unlock()
			entered <- struct{}{}
			<-release
		}
		d, s := newTestDispatcher(t, settle, generousBudgets())
		d.start()
		enqueue(t, d, asyncCallInfoMessageID, payload)

		<-entered // 分发器已进入结算通知并被挡在结算点
		mu.Lock()
		if len(calls) != 1 {
			t.Fatalf("settle called %d times, want exactly 1", len(calls))
		}
		got := calls[0]
		mu.Unlock()
		// json.Number 精确解析:int32 上界 ID 精确读出,不经 float64 往返。
		if want := (completionInfo{ID: 2147483647, What: "AsyncCallFinished", OK: true}); got.ID != want.ID || got.What != want.What || got.OK != want.OK {
			t.Fatalf("settlement info = %+v, want %+v", got, want)
		}
		// 结算未返回,公开通道此刻不可能有事件。
		select {
		case ev := <-d.events():
			t.Fatalf("delivery completed before settle returned, violating settle-before-deliver: %+v", ev)
		default:
		}
		close(release) // 放行投递

		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, payload) {
			t.Fatalf("event raw JSON = %s, want %s with big integers and unknown fields preserved byte-for-byte", ev.JSON, payload)
		}
		if ev.Sequence != 1 {
			t.Errorf("sequence = %d, want 1", ev.Sequence)
		}
		closeAndDrain(t, d)
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n != 1 {
			t.Fatalf("settle calls accumulated after release = %d, want exactly 1 (settle can happen only once)", n)
		}
		select {
		case <-s.faultNotify():
			t.Fatal("a legal completion must not trigger a fault")
		default:
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("lost events count = %d, want 0", lost)
		}
	})

	t.Run("ret=false normal completion settles OK=false without faulting", func(t *testing.T) {
		rec := newSettleRecorder(2)
		d, s := newTestDispatcher(t, rec.record, generousBudgets())
		d.start()

		payload := []byte(`{"what":"AsyncCallFinished","async_call_id":5,"details":{"ret":false}}`)
		enqueue(t, d, asyncCallInfoMessageID, payload)
		closeAndDrain(t, d)

		if got := rec.total(); got != 1 {
			t.Fatalf("settle notification called %d times, want 1", got)
		}
		if got := rec.all()[0]; got.ID != 5 || got.What != "AsyncCallFinished" || got.OK {
			t.Fatalf("settlement info = %+v, want {5 AsyncCallFinished false}", got)
		}
		// ret=false 是正常完成:不使实例故障,事件照常交付。
		select {
		case <-s.faultNotify():
			t.Fatal("ret=false is a normal completion and must not fault the instance")
		default:
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("lost events count = %d, want 0", lost)
		}
		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, payload) {
			t.Fatalf("event raw JSON = %s, want %s", ev.JSON, payload)
		}
	})
}

// TestEventDispatchDrainAndTerminate 验证「dispatcher 的排空
// 与终止不等待外部消费者,也不静默丢弃完成信息后宣称结果完整」:
// closeIntake+drain 在消费者停读时有限步完成,已缓冲事件关闭后仍可
// 消费;排空期间已排队的合法完成先结算再尝试投递,投不出的计丢失并
// 按溢出契约报告故障。
func TestEventDispatchDrainAndTerminate(t *testing.T) {
	t.Run("draining completes in finite steps when the consumer stops reading and buffered events stay consumable", func(t *testing.T) {
		b := generousBudgets()
		b.publicEvents = 2
		d, s := newTestDispatcher(t, nil, b)
		d.start()

		payloads := make([][]byte, 4)
		for i := range payloads {
			payloads[i] = []byte(fmt.Sprintf(`{"i":%d}`, i))
		}
		for i, p := range payloads {
			enqueue(t, d, MessageID(230)+MessageID(i), p)
		}
		// 消费者全程未读;drain 以公开通道关闭为界、不等待读者,必须
		// 在防挂死上限内返回。
		closeAndDrain(t, d)

		// 通道已关闭,已缓冲的 2 条(FIFO 最老两条)仍可读且完好。
		for i := 0; i < 2; i++ {
			ev := mustReadEvent(t, d.events())
			if ev.MessageID != MessageID(230)+MessageID(i) {
				t.Fatalf("buffered event %d MessageID = %d, want %d (FIFO)", i, ev.MessageID, MessageID(230)+MessageID(i))
			}
			if string(ev.JSON) != string(payloads[i]) {
				t.Fatalf("buffered event %d raw JSON = %s, want %s", i, ev.JSON, payloads[i])
			}
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("buffered event %d sequence = %d, want %d", i, ev.Sequence, i+1)
			}
		}
		if ev, ok := <-d.events(); ok {
			t.Fatalf("the public channel must be closed after draining, but read %+v", ev)
		}
		// 无法投递的两条计丢失并按溢出契约报告故障。
		if lost := s.currentLostEvents(); lost != 2 {
			t.Errorf("lost events count = %d, want 2", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
	})

	t.Run("completions queued during draining settle before delivering and undeliverable ones count as lost", func(t *testing.T) {
		b := generousBudgets()
		b.publicEvents = 1
		rec := newSettleRecorder(4)
		d, s := newTestDispatcher(t, rec.record, b)
		d.start()

		payload := []byte(`{"what":"AsyncCallFinished","async_call_id":9,"details":{"ret":true}}`)
		// 第 1 条占满公开缓冲;第 2、3 条在队列中等待,投递时必然失败。
		for i := 0; i < 3; i++ {
			enqueue(t, d, asyncCallInfoMessageID, payload)
		}
		closeAndDrain(t, d)

		// 完成信息绝不静默丢弃:3 条都先经过结算通知(含排空期间排队
		// 且最终投递失败的两条),再各自尝试投递。
		if got := rec.total(); got != 3 {
			t.Fatalf("settle notification called %d times, want 3 (completions queued during draining also settle first)", got)
		}
		for i, info := range rec.all() {
			if want := (completionInfo{ID: 9, What: "AsyncCallFinished", OK: true}); info.ID != want.ID || info.What != want.What || info.OK != want.OK {
				t.Fatalf("settlement info #%d = %+v, want %+v", i+1, info, want)
			}
		}
		// 只有第 1 条进入公开缓冲;投不出的 2 条计丢失并报告溢出故障。
		if n := len(d.events()); n != 1 {
			t.Errorf("events left in the public buffer = %d, want 1", n)
		}
		if lost := s.currentLostEvents(); lost != 2 {
			t.Errorf("lost events count = %d, want 2", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want errors.Is native.ErrEventOverflow", s.currentFault())
		}
	})
}

// TestEventDispatchSlotDiscipline 钉住 slot 原语纪律(文件头
// panic 边界):commit/abort 各只能结算一次、零值不可用、commit 长度
// 必须与预留一致;abort 精确退还计数与字节预算;序号单调且从 1 开始。
func TestEventDispatchSlotDiscipline(t *testing.T) {
	t.Run("commit and abort may each be called only once", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(300), 2)
		if err != nil {
			t.Fatalf("admission must not fail: %v", err)
		}
		slot.commit([]byte("ok"))
		// 已 commit 的额度由内部队列持有,直到分发器处理。
		if count, hold := internalBudgetHold(d); count != 1 || hold != 2 {
			t.Fatalf("internal budget held after commit = (%d, %d), want (1, 2)", count, hold)
		}
		assertPanics(t, "commit after commit", func() { slot.commit([]byte("ok")) })
		assertPanics(t, "abort after commit", func() { slot.abort() })
	})

	t.Run("abort after abort panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(300), 2)
		if err != nil {
			t.Fatalf("admission must not fail: %v", err)
		}
		slot.abort()
		assertPanics(t, "abort after abort", func() { slot.abort() })
	})

	t.Run("zero-value slot unusable", func(t *testing.T) {
		var zero eventSlot
		assertPanics(t, "zero-value slot commit", func() { zero.commit([]byte("x")) })
		assertPanics(t, "zero-value slot abort", func() { zero.abort() })
	})

	t.Run("commit length mismatching the reservation panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(300), 2)
		if err != nil {
			t.Fatalf("admission must not fail: %v", err)
		}
		assertPanics(t, "commit length 3 mismatches reserved 2", func() { slot.commit([]byte("abc")) })
		// 长度不符的 commit 未结算 slot:额度仍按预留持有,可正常 abort。
		if count, hold := internalBudgetHold(d); count != 1 || hold != 2 {
			t.Fatalf("internal budget held after the length-mismatch panic = (%d, %d), want (1, 2)", count, hold)
		}
		slot.abort()
	})

	t.Run("abort returns the count and byte budgets and allows admission again", func(t *testing.T) {
		b := generousBudgets()
		b.internalEvents = 1
		b.internalBytes = 4
		d, _ := newTestDispatcher(t, nil, b)

		slot, err := d.admit(MessageID(301), 4)
		if err != nil {
			t.Fatalf("admission must not fail: %v", err)
		}
		if count, hold := internalBudgetHold(d); count != 1 || hold != 4 {
			t.Fatalf("internal budget held after reservation = (%d, %d), want (1, 4)", count, hold)
		}
		slot.abort()
		if count, hold := internalBudgetHold(d); count != 0 || hold != 0 {
			t.Fatalf("internal budget held after abort = (%d, %d), want (0, 0): count and bytes must both be returned exactly", count, hold)
		}
		// 额度已退回:同样的预算可再次准入(此后 abort 收尾)。
		again, err := d.admit(MessageID(301), 4)
		if err != nil {
			t.Fatalf("admission after abort returns must not fail: %v", err)
		}
		again.abort()
	})

	t.Run("sequence numbers monotonic and starting from 1", func(t *testing.T) {
		d, s := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		for i := 0; i < 3; i++ {
			enqueue(t, d, MessageID(302)+MessageID(i), []byte(fmt.Sprintf(`{"i":%d}`, i)))
		}
		for i := 0; i < 3; i++ {
			ev := mustReadEvent(t, d.events())
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("event %d sequence = %d, want %d (monotonic and starting from 1)", i+1, ev.Sequence, i+1)
			}
			if ev.MessageID != MessageID(302)+MessageID(i) {
				t.Fatalf("event %d MessageID = %d, want %d (FIFO delivery)", i+1, ev.MessageID, MessageID(302)+MessageID(i))
			}
		}
		closeAndDrain(t, d)
		select {
		case <-s.faultNotify():
			t.Fatal("normal delivery must not trigger a fault")
		default:
		}
	})
}

// TestEventDispatchLifecyclePanics 逐条钉住 dispatch.go 声明的接线层
// 编程错误 panic:closeIntake 至多一次、关闭后 admit 即错、start 至多
// 一次、drain 前置契约(start、closeIntake、至多一次)与 drain 请求
// 后 commit 即错。违反契约一律 panic,不允许静默继续。
func TestEventDispatchLifecyclePanics(t *testing.T) {
	t.Run("closeIntake at most once", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.closeIntake()
		assertPanics(t, "repeated closeIntake", func() { d.closeIntake() })
	})

	t.Run("admit after closeIntake panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.closeIntake()
		assertPanics(t, "admit after closeIntake", func() { _, _ = d.admit(MessageID(1), 1) })
	})

	t.Run("start at most once", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		assertPanics(t, "repeated start", func() { d.start() })
		closeAndDrain(t, d) // 清理分发循环:关门后排空
	})

	t.Run("drain before start panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		assertPanics(t, "drain before start", func() { d.drain() })
	})

	t.Run("drain before closeIntake panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		assertPanics(t, "drain before intake closes", func() { d.drain() })
		// 契约检查失败未登记 drain 请求:正常关门排空,清理分发循环。
		d.closeIntake()
		mustDrain(t, d)
	})

	t.Run("drain at most once", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		d.closeIntake()
		d.drain()
		assertPanics(t, "repeated drain", func() { d.drain() })
	})

	t.Run("commit after drain request panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(303), 2)
		if err != nil {
			t.Fatalf("admission must not fail: %v", err)
		}
		d.start()
		d.closeIntake()
		d.requestDrain()
		assertPanics(t, "commit after drain request", func() { slot.commit([]byte("ok")) })
		// 空队列 + drain 请求:发送者已停在停止点等待释放(第 5 步,
		// Events 的关闭由通道所有者在终态发布后触发);经 closeEvents
		// 关闭公开通道并等待退出,不留悬挂 goroutine。
		d.closeEvents()
	})
}

// TestEventDispatchStopPointAndRelease 白盒钉住发送者停止点与 Events
// 关闭的拆分契约(第 5 步、:发送者停止是 Events 关闭的前置,
// Events 的关闭由通道所有者在终态发布后触发):requestDrain 后
// waitProcessed 到达停止点,此刻 Events 仍开放;closeEvents 触发关闭
// 并等待退出;未 requestDrain 即等待/释放与重复释放均按编程错误
// panic。
func TestEventDispatchStopPointAndRelease(t *testing.T) {
	t.Run("Events still open after the stop point and closed only after release", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		d.closeIntake()
		d.requestDrain()
		stopped := make(chan struct{})
		go func() {
			d.waitProcessed()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(hangGuard):
			t.Fatal("waitProcessed did not return within the hang guard limit: the sender stop point was not reached")
		}
		// 停止点已到达而 Events 尚未关闭:无缓冲事件时接收不得就绪。
		select {
		case _, ok := <-d.events():
			if ok {
				t.Error("an event should not be received when no events are buffered")
			} else {
				t.Error("Events must not be closed before closeEvents (close order: publish the terminal state first, then close Events)")
			}
		default:
		}
		d.closeEvents()
		if _, ok := <-d.events(); ok {
			t.Error("Events should be closed after closeEvents")
		}
	})

	t.Run("precondition contract violation panics", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		assertPanics(t, "waitProcessed before requestDrain", func() { d.waitProcessed() })
		assertPanics(t, "closeEvents before requestDrain", func() { d.closeEvents() })
		d.start()
		d.closeIntake()
		d.requestDrain()
		d.closeEvents()
		assertPanics(t, "repeated closeEvents", func() { d.closeEvents() })
	})
}

// TestEventDispatchSequenceExhaustionProtocolFault 白盒钉住序号耗尽
// 守卫(本地事件序号耗尽以 ErrProtocol 故障,不复用序号):
// 把已分配序号直接推到回绕边界后再投一条,该条无法成为 Event,以
// ErrProtocol 故障、计 1 条丢失、不投递、不结算。
func TestEventDispatchSequenceExhaustionProtocolFault(t *testing.T) {
	rec := newSettleRecorder(1)
	d, s := newTestDispatcher(t, rec.record, generousBudgets())
	d.start()

	// 白盒注入:在分发器锁内把已分配序号推到 MaxUint64。
	d.mu.Lock()
	d.seq = math.MaxUint64
	d.mu.Unlock()

	payload := []byte(`{"what":"AsyncCallFinished","async_call_id":1,"details":{"ret":true}}`)
	enqueue(t, d, asyncCallInfoMessageID, payload)
	closeAndDrain(t, d)

	// 序号耗尽的条目以 ErrProtocol 故障并计 1 条丢失。
	if !errors.Is(s.currentFault(), native.ErrProtocol) {
		t.Errorf("currentFault() = %v, want errors.Is native.ErrProtocol", s.currentFault())
	}
	if lost := s.currentLostEvents(); lost != 1 {
		t.Errorf("lost events count = %d, want 1", lost)
	}
	// 不投递:通道已关闭且为空。
	if ev, ok := <-d.events(); ok {
		t.Fatalf("an entry with an exhausted sequence must not be delivered, but read %+v", ev)
	}
	// 不结算:该条在任何关联步骤之前已被丢弃。
	if got := rec.total(); got != 0 {
		t.Errorf("the settle notification was called %d times, want 0 (an entry with an exhausted sequence does not settle)", got)
	}
}

// TestEventDispatchConcurrentAdmitAndConsume 验证「并发
// admit/消费在 -race 下干净」:8 个 goroutine 并发 admit+commit 400
// 条消息,消费者循环读取直到通道关闭;充足预算下全部消息恰好交付
// 一次、序号唯一、无故障、无丢失。
func TestEventDispatchConcurrentAdmitAndConsume(t *testing.T) {
	b := generousBudgets()
	b.internalEvents = 1024
	b.internalBytes = 1 << 20
	b.publicEvents = 1024
	b.publicBytes = 1 << 20
	d, s := newTestDispatcher(t, nil, b)
	d.start()

	const (
		workers   = 8
		perWorker = 50
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				payload := []byte(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i))
				slot, err := d.admit(MessageID(400)+MessageID(w), len(payload))
				if err != nil {
					t.Errorf("concurrent admission must not fail: %v", err)
					return
				}
				slot.commit(payload)
			}
		}(w)
	}

	// 消费者:读取直到通道关闭(drain 后),收集全部事件。
	var (
		mu           sync.Mutex
		seen         []Event
		consumerDone = make(chan struct{})
	)
	go func() {
		defer close(consumerDone)
		for ev := range d.events() {
			mu.Lock()
			seen = append(seen, ev)
			mu.Unlock()
		}
	}()

	wg.Wait()
	d.closeIntake()
	mustDrain(t, d)
	<-consumerDone

	if len(seen) != workers*perWorker {
		t.Fatalf("delivered events = %d, want exactly %d (each delivered exactly once)", len(seen), workers*perWorker)
	}
	seqSeen := make(map[uint64]bool, len(seen))
	for _, ev := range seen {
		if seqSeen[ev.Sequence] {
			t.Fatalf("sequence %d was assigned more than once", ev.Sequence)
		}
		seqSeen[ev.Sequence] = true
	}
	select {
	case <-s.faultNotify():
		t.Fatal("concurrent admission and consumption under ample budgets must not produce an overflow fault")
	default:
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("lost events count = %d, want 0", lost)
	}
}

// TestLostEventsSaturatingAndFaultSnapshot 钉住在实例状态上
// 增补的故障通知与丢失诊断:丢失计数饱和不回绕、首因原文前
// 缀截取至多 4096 字节且深拷贝隔离、重复故障不覆盖首因、诊断快照为
// 独立深拷贝、一次性故障通知恰在首次故障线性化时关闭且正常关闭不触
// 发。
func TestLostEventsSaturatingAndFaultSnapshot(t *testing.T) {
	t.Run("lost events count saturates without wrapping", func(t *testing.T) {
		s := newInstanceState(0x0D01, 128, 1_048_576)
		s.recordLostEvents(math.MaxUint64 - 1)
		s.recordLostEvents(1)
		if got := s.currentLostEvents(); got != math.MaxUint64 {
			t.Fatalf("lost events count = %d, want %d (exactly at the limit)", got, uint64(math.MaxUint64))
		}
		// 越过上限:夹到 MaxUint64,不回绕。
		s.recordLostEvents(1)
		if got := s.currentLostEvents(); got != math.MaxUint64 {
			t.Fatalf("counting 1 more after saturation = %d, want it kept as %d (no wrapping)", got, uint64(math.MaxUint64))
		}
		s.recordLostEvents(math.MaxUint64)
		if got := s.faultDiagnostics().lostEvents; got != math.MaxUint64 {
			t.Fatalf("snapshot lost events count = %d, want it kept saturated at %d", got, uint64(math.MaxUint64))
		}
	})

	t.Run("prefix truncated to at most 4096 bytes", func(t *testing.T) {
		s := newInstanceState(0x0D02, 128, 1_048_576)
		s.publishReady()
		prefix := make([]byte, faultRawPrefixBytes+904)
		for i := range prefix {
			prefix[i] = byte(i % 251)
		}
		cause := fmt.Errorf("first fault: %w", native.ErrProtocol)
		s.recordFaultWithDetail(cause, prefix)

		snap := s.faultDiagnostics()
		if len(snap.rawPrefix) != faultRawPrefixBytes {
			t.Fatalf("prefix length = %d, want at most %d", len(snap.rawPrefix), faultRawPrefixBytes)
		}
		if !bytes.Equal(snap.rawPrefix, prefix[:faultRawPrefixBytes]) {
			t.Fatal("the prefix content does not match the first 4096 bytes of the raw data")
		}
		if !errors.Is(snap.firstFault, cause) {
			t.Errorf("snapshot first fault = %v, want errors.Is %v", snap.firstFault, cause)
		}
		if got := s.currentPhase(); got != phaseFaulted {
			t.Fatalf("phase after recording a fault in the ready phase = %d, want faulted", got)
		}
	})

	t.Run("repeated faults do not overwrite the first fault and prefix", func(t *testing.T) {
		s := newInstanceState(0x0D03, 128, 1_048_576)
		s.publishReady()
		first := fmt.Errorf("first fault: %w", native.ErrEventOverflow)
		firstPrefix := []byte("first-prefix")
		s.recordFaultWithDetail(first, firstPrefix)
		second := fmt.Errorf("late arrival: %w", native.ErrProtocol)
		s.recordFaultWithDetail(second, bytes.Repeat([]byte("second!"), 1000))

		snap := s.faultDiagnostics()
		if !errors.Is(snap.firstFault, first) {
			t.Errorf("the first fault was overwritten by a late-arriving fault: %v", snap.firstFault)
		}
		if errors.Is(snap.firstFault, native.ErrProtocol) {
			t.Errorf("a late-arriving fault mixed into the first fault: %v", snap.firstFault)
		}
		if !bytes.Equal(snap.rawPrefix, firstPrefix) {
			t.Errorf("the prefix was overwritten by a late record = %q, want %q", snap.rawPrefix, firstPrefix)
		}
		if snap.lostEvents != 0 {
			t.Errorf("recordFaultWithDetail must not change the lost events count, got %d", snap.lostEvents)
		}
	})

	t.Run("prefix and snapshot are both deep copies", func(t *testing.T) {
		s := newInstanceState(0x0D04, 128, 1_048_576)
		s.publishReady()
		prefix := bytes.Repeat([]byte{0xAB}, 64)
		s.recordFaultWithDetail(fmt.Errorf("fault: %w", native.ErrProtocol), prefix)

		// 原地改写调用者缓冲:不得穿透已保存前缀。
		for i := range prefix {
			prefix[i] = 0xCD
		}
		snap1 := s.faultDiagnostics()
		if !bytes.Equal(snap1.rawPrefix, bytes.Repeat([]byte{0xAB}, 64)) {
			t.Fatal("overwriting the caller's raw data affected the snapshot: the prefix is not a deep copy")
		}
		// 原地改写快照:不得穿透状态内原值与后续快照。
		for i := range snap1.rawPrefix {
			snap1.rawPrefix[i] = 0xEF
		}
		snap2 := s.faultDiagnostics()
		if !bytes.Equal(snap2.rawPrefix, bytes.Repeat([]byte{0xAB}, 64)) {
			t.Fatal("overwriting the snapshot affected the original value in state: the snapshot is not a deep copy")
		}
	})

	t.Run("fault snapshot without a prefix has nil rawPrefix", func(t *testing.T) {
		s := newInstanceState(0x0D05, 128, 1_048_576)
		s.publishReady()
		s.recordFault(fmt.Errorf("no prefix: %w", native.ErrEventOverflow))
		if snap := s.faultDiagnostics(); snap.rawPrefix != nil {
			t.Errorf("snapshot rawPrefix for a fault without a prefix = %q, want nil", snap.rawPrefix)
		}
	})

	t.Run("fault notification exactly once and normal close does not trigger it", func(t *testing.T) {
		// 正常关闭:closeDone 关闭但故障通知保持开放。
		closed := newClosedState(t, 0x0D06)
		select {
		case <-closed.faultNotify():
			t.Fatal("a normal close must not trigger the fault notification")
		default:
		}
		select {
		case <-closed.done():
		default:
			t.Fatal("a normal close must close closeDone")
		}
		// 首次故障线性化时关闭一次;重复记录不重复关闭(重复 close 会
		// panic,实现若有缺陷将以 panic 暴露)。
		s := newInstanceState(0x0D07, 128, 1_048_576)
		s.publishReady()
		s.recordFault(fmt.Errorf("first time: %w", native.ErrProtocol))
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("the first fault must close the fault notification")
		}
		s.recordFault(fmt.Errorf("second time: %w", native.ErrEventOverflow))
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("the fault notification must remain closed")
		}
	})
}
