// 本文件是有界事件分发器与故障诊断的受控用例(MAA-17):白盒同包
// 验证绑定设计 §7、§7.1、§8.1 的分发契约——内部队列与公开缓冲各自
// 计入预算且越界明确报告溢出故障、消费者完全停读时入口与分发器都
// 不无限阻塞且故障通知独立可观察、首次溢出 sticky 保留并拒绝新业务
// 调用、未知消息/未知字段/非法 UTF-8/JSON 与关联字段异常的原文保留
// 与结算纪律、排空不等待外部消费者且不静默丢弃完成信息,以及 slot
// 原语、饱和丢失计数与首因前缀的深拷贝纪律。全部用例以受控屏障
// (channel/WaitGroup/一次性阻塞结算)确定性同步,不加载任何原生库、
// 不触达 loader/Runtime,CGO_ENABLED=0 可跑;防挂死只作兜底,不作
// 同步手段。

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

// dispatchBudgets 是事件分发器的五项预算(MAA-03 冻结的字段集合):
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
	s := newInstanceState(0x0D17000)
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
		t.Fatalf("准入消息 %d 不得失败: %v", messageID, err)
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
		t.Fatal("读取公开事件超时:分发器疑似阻塞或未投递(防挂死上限触发)")
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
		t.Fatal("drain 未在防挂死上限内返回:排空疑似在等待外部消费者")
	}
}

// closeAndDrain 走「关门 → 排空」的标准收尾顺序(§8 第 4、5 步):
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
		t.Fatal("预算越界必须返回错误")
	}
	if !errors.Is(err, native.ErrEventOverflow) {
		t.Errorf("errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
	}
	if errors.Is(err, native.ErrProtocol) {
		t.Errorf("预算越界不得误标为 ErrProtocol: %v", err)
	}
}

// TestEventDispatchBudgetOverflowReportsFault 钉住验收条件「内部队列
// 和公开缓冲均计入预算,事件数或总字节数越界会明确报告故障」:单条
// CallbackBytes、内部事件计数、内部字节、公开缓冲计数与公开字节五
// 个预算各自独立越界(子用例互不混用),每次越界都以
// errors.Is(native.ErrEventOverflow) 明确报告故障,入口不阻塞、不预
// 留任何额度。
func TestEventDispatchBudgetOverflowReportsFault(t *testing.T) {
	t.Run("单条超过 CallbackBytes", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 8
		d, s := newTestDispatcher(t, nil, b)

		slot, err := d.admit(MessageID(100), 9)
		assertOverflowClass(t, err)
		if slot.d != nil {
			t.Error("被拒绝的准入不得返回可用 slot")
		}
		// 不预留任何额度:计数与字节都保持 0。
		if count, hold := internalBudgetHold(d); count != 0 || hold != 0 {
			t.Errorf("拒绝后内部预算占用 = (%d, %d), want (0, 0)", count, hold)
		}
		// 越界按溢出契约上报故障并计 1 条丢失;故障通知独立关闭。
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
		if n := s.currentLostEvents(); n != 1 {
			t.Errorf("丢失计数 = %d, want 1", n)
		}
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("首次溢出必须关闭一次性故障通知")
		}
		// 恰好等于 CallbackBytes 的消息仍在界内:准入成功(随后退回)。
		boundary, err := d.admit(MessageID(100), 8)
		if err != nil {
			t.Fatalf("恰好等于 CallbackBytes 的准入不得失败: %v", err)
		}
		boundary.abort()
	})

	t.Run("内部事件计数越界", func(t *testing.T) {
		b := generousBudgets()
		b.internalEvents = 2
		d, s := newTestDispatcher(t, nil, b)

		first, err := d.admit(MessageID(101), 4)
		if err != nil {
			t.Fatalf("第 1 条准入不得失败: %v", err)
		}
		second, err := d.admit(MessageID(101), 4)
		if err != nil {
			t.Fatalf("第 2 条准入不得失败: %v", err)
		}
		_, err = d.admit(MessageID(101), 4)
		assertOverflowClass(t, err)
		// 已持有的预留不受拒绝影响:计数仍为 2,字节仍为 8。
		if count, hold := internalBudgetHold(d); count != 2 || hold != 8 {
			t.Errorf("拒绝后内部预算占用 = (%d, %d), want (2, 8)", count, hold)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
		// 退回预留后同一预算可再次准入:拒绝不污染预算。
		first.abort()
		second.abort()
		again, err := d.admit(MessageID(101), 4)
		if err != nil {
			t.Fatalf("预留退回后的准入不得失败: %v", err)
		}
		again.abort()
	})

	t.Run("内部字节越界", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 8
		b.internalBytes = 16
		d, s := newTestDispatcher(t, nil, b)

		var slots []eventSlot
		for i := 0; i < 4; i++ {
			slot, err := d.admit(MessageID(102), 4)
			if err != nil {
				t.Fatalf("第 %d 条 4 字节准入不得失败(字节预算未满): %v", i+1, err)
			}
			slots = append(slots, slot)
		}
		// 4 条恰好装满 16 字节字节预算;事件计数预算(64)远未触顶,
		// 第 5 条越界确由字节触发。
		if count, hold := internalBudgetHold(d); count != 4 || hold != 16 {
			t.Fatalf("装满后内部预算占用 = (%d, %d), want (4, 16)", count, hold)
		}
		_, err := d.admit(MessageID(102), 4)
		assertOverflowClass(t, err)
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
		for i := range slots {
			slots[i].abort()
		}
		if count, hold := internalBudgetHold(d); count != 0 || hold != 0 {
			t.Errorf("全部退回后内部预算占用 = (%d, %d), want (0, 0)", count, hold)
		}
	})

	t.Run("公开缓冲事件计数越界(消费者停读)", func(t *testing.T) {
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
			t.Errorf("公开缓冲滞留事件 = %d, want 恰为 PublicEvents 2", n)
		}
		if lost := s.currentLostEvents(); lost != 3 {
			t.Errorf("丢失计数 = %d, want 3", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
		// 计数越界与字节越界互不混用:本用例字节预算(4096)远未触顶。
	})

	t.Run("公开缓冲字节越界(消费者停读)", func(t *testing.T) {
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
			t.Errorf("公开缓冲滞留事件 = %d, want 2(恰容纳 14/16 字节)", n)
		}
		if lost := s.currentLostEvents(); lost != 3 {
			t.Errorf("丢失计数 = %d, want 3", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
	})
}

// TestEventDispatchBoundedWhenConsumerStopsReading 钉住验收条件「外
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
		t.Fatal("消费者停读导致的溢出故障未关闭故障通知")
	}
	if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
		t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
	}
	// 恰好 PublicEvents 条已入缓冲,其余全部计入丢失。
	if n := len(d.events()); n != 3 {
		t.Errorf("公开缓冲滞留事件 = %d, want 3", n)
	}
	if lost := s.currentLostEvents(); lost != total-3 {
		t.Errorf("丢失计数 = %d, want %d", lost, total-3)
	}
	// 关闭后已缓冲事件仍可读:序号 1,2,3、原文完整、FIFO。
	for i := 0; i < 3; i++ {
		ev := mustReadEvent(t, d.events())
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("第 %d 条缓冲事件序号 = %d, want %d", i+1, ev.Sequence, i+1)
		}
		if want := fmt.Sprintf(`{"n":%d}`, i); string(ev.JSON) != want {
			t.Fatalf("第 %d 条缓冲事件原文 = %s, want %s", i+1, ev.JSON, want)
		}
	}
	if ev, ok := <-d.events(); ok {
		t.Fatalf("排空后公开通道必须已关闭,却读到 %+v", ev)
	}
}

// TestInstanceFaultFirstOverflowStickyAndCallRejection 钉住验收条件
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

	t.Run("首因保留 ErrEventOverflow 且丢失计数可读", func(t *testing.T) {
		d, s, payload := setup(t)
		for i := 0; i < 3; i++ {
			enqueue(t, d, MessageID(220), payload)
		}
		closeAndDrain(t, d)

		first := s.currentFault()
		if first == nil {
			t.Fatal("首次溢出必须记录故障")
		}
		if !errors.Is(first, native.ErrEventOverflow) {
			t.Errorf("errors.Is(首因, native.ErrEventOverflow) = false; 首因 = %v", first)
		}
		snap := s.faultDiagnostics()
		if snap.lostEvents != 2 {
			t.Errorf("丢失计数 = %d, want 2(第 2、3 条投递失败)", snap.lostEvents)
		}
		if snap.rawPrefix != nil {
			t.Errorf("溢出故障不得携带原文前缀,得到 %q", snap.rawPrefix)
		}
		if got := s.currentPhase(); got != phaseFaulted {
			t.Fatalf("首次溢出后阶段 = %d, want faulted", got)
		}
	})

	t.Run("faulted 后 admitCall 拒绝并沿链到达溢出", func(t *testing.T) {
		d, s, payload := setup(t)
		for i := 0; i < 2; i++ {
			enqueue(t, d, MessageID(221), payload)
		}
		closeAndDrain(t, d)

		_, err := s.admitCall("溢出后的新调用")
		if err == nil {
			t.Fatal("faulted 实例必须拒绝新业务调用")
		}
		if !errors.Is(err, native.ErrEventOverflow) {
			t.Errorf("errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
		}
		// 拒绝与实例首因共享同一错误身份。
		if !errors.Is(err, s.currentFault()) {
			t.Errorf("errors.Is(err, 首因) = false; err = %v, 首因 = %v", err, s.currentFault())
		}
		if errors.Is(err, native.ErrClosing) || errors.Is(err, native.ErrClosed) {
			t.Errorf("溢出拒绝不得误标为 closing/closed: %v", err)
		}
	})

	t.Run("重复故障只累计丢失,首因与前缀不变", func(t *testing.T) {
		d, s, payload := setup(t)
		for i := 0; i < 3; i++ {
			enqueue(t, d, MessageID(222), payload)
		}
		closeAndDrain(t, d)

		before := s.faultDiagnostics()
		if before.lostEvents != 2 {
			t.Fatalf("重复溢出应累计丢失 = 2,得到 %d", before.lostEvents)
		}
		// 后到的异类故障与带前缀记录都不得覆盖首因、前缀或丢失计数。
		s.recordFaultWithDetail(
			fmt.Errorf("后到的协议故障: %w", native.ErrProtocol),
			[]byte("后到前缀"),
		)
		after := s.faultDiagnostics()
		if !errors.Is(after.firstFault, native.ErrEventOverflow) {
			t.Errorf("首因被后到故障覆盖: %v", after.firstFault)
		}
		if errors.Is(after.firstFault, native.ErrProtocol) {
			t.Errorf("后到故障混入首因: %v", after.firstFault)
		}
		if !bytes.Equal(after.rawPrefix, before.rawPrefix) {
			t.Errorf("前缀被后到记录覆盖 = %q, want 保持 %q", after.rawPrefix, before.rawPrefix)
		}
		if after.lostEvents != before.lostEvents {
			t.Errorf("重复记录后丢失计数 = %d, want 保持 %d(重复故障只累计丢失,不改诊断)", after.lostEvents, before.lostEvents)
		}
	})
}

// TestEventDispatchUnknownAndInvalidMessages 钉住验收条件「未知消息
// 及字段可观察;非法 JSON 保留原文,不能将无法关联的完成信息视为成
// 功」:未知 MessageID/未知 what/未知字段按原样逐字节交付且不故障;
// 非法 UTF-8/JSON 以 ErrProtocol 故障、保留至多 4096 字节原文前缀且
// 原文仍尝试交付;AsyncCallInfo 关联字段异常绝不调用结算通知;合法
// 完成恰好结算一次、字段精确且先结算后投递。
func TestEventDispatchUnknownAndInvalidMessages(t *testing.T) {
	t.Run("未知 MessageID 与未知字段按原样交付", func(t *testing.T) {
		d, s := newTestDispatcher(t, nil, generousBudgets())
		d.start()

		want := []byte(`{"unknown":[1,2,{"deep":null}],"what":"Whatever","big":9007199254740993}`)
		original := bytes.Clone(want)
		slot, err := d.admit(MessageID(99), len(original))
		if err != nil {
			t.Fatalf("未知消息准入不得失败: %v", err)
		}
		slot.commit(original)
		// commit 已逐字节克隆:此后原地改写调用者缓冲不影响队列记录。
		for i := range original {
			original[i] = 'x'
		}

		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, want) {
			t.Fatalf("未知消息原文 = %s, want 逐字节相等的 %s", ev.JSON, want)
		}
		if ev.MessageID != MessageID(99) {
			t.Errorf("MessageID = %d, want 99", ev.MessageID)
		}
		if ev.Sequence != 1 {
			t.Errorf("序号 = %d, want 1(未知消息同样分配正常序号)", ev.Sequence)
		}
		closeAndDrain(t, d)
		// 未知消息与未知字段不进故障流程:不故障、不丢失。
		select {
		case <-s.faultNotify():
			t.Fatal("未知消息与未知字段不得触发故障")
		default:
		}
		if n := s.currentLostEvents(); n != 0 {
			t.Errorf("丢失计数 = %d, want 0", n)
		}
	})

	t.Run("未知 what 的合法完成原样结算与交付", func(t *testing.T) {
		rec := newSettleRecorder(2)
		d, s := newTestDispatcher(t, rec.record, generousBudgets())
		d.start()

		payload := []byte(`{"what":"SomeFutureWhat","async_call_id":7,"details":{"ret":true},"extra":"保留"}`)
		enqueue(t, d, asyncCallInfoMessageID, payload)
		closeAndDrain(t, d)

		if got := rec.total(); got != 1 {
			t.Fatalf("结算通知调用 %d 次, want 1(未知 what 仍按原样交付结算方)", got)
		}
		if got := rec.all()[0]; got != (completionInfo{ID: 7, What: "SomeFutureWhat", OK: true}) {
			t.Fatalf("结算信息 = %+v, want {7 SomeFutureWhat true}", got)
		}
		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, payload) {
			t.Fatalf("事件原文 = %s, want 未知 what 与未知字段逐字节保留的 %s", ev.JSON, payload)
		}
		select {
		case <-s.faultNotify():
			t.Fatal("未知 what 不得触发故障")
		default:
		}
	})

	t.Run("非法 UTF-8 与非法 JSON 保留原文前缀且原文仍交付", func(t *testing.T) {
		cases := []struct {
			name string
			raw  []byte
		}{
			// 5000 字节非法 UTF-8(孤立续字节):前缀必须截取到 4096。
			{"非法 UTF-8", bytes.Repeat([]byte{0xA5}, 5000)},
			{"非法 JSON", []byte(`{"what":"w","async_call_id":1,`)},
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
					t.Fatal("非法原文必须逐字节交付,不得截断或替换")
				}
				closeAndDrain(t, d)

				// 无法关联的完成信息绝不结算为成功:结算通知零调用。
				if got := rec.total(); got != 0 {
					t.Fatalf("非法消息不得调用结算通知,实际 %d 次", got)
				}
				// 协议故障 + 原文前缀(至多 faultRawPrefixBytes 字节)。
				if !errors.Is(s.currentFault(), native.ErrProtocol) {
					t.Errorf("currentFault() = %v, want 含 native.ErrProtocol", s.currentFault())
				}
				wantLen := len(original)
				if wantLen > faultRawPrefixBytes {
					wantLen = faultRawPrefixBytes
				}
				snap := s.faultDiagnostics()
				if len(snap.rawPrefix) != wantLen {
					t.Fatalf("原文前缀长度 = %d, want %d", len(snap.rawPrefix), wantLen)
				}
				if !bytes.Equal(snap.rawPrefix, original[:wantLen]) {
					t.Fatal("原文前缀内容与原文前缀不一致")
				}
				// 深拷贝:改写调用者原文不得穿透已保存前缀。
				for i := range tc.raw {
					tc.raw[i] = 0x00
				}
				if again := s.faultDiagnostics(); !bytes.Equal(again.rawPrefix, original[:wantLen]) {
					t.Fatal("改写调用者原文影响了已保存前缀:前缀不是深拷贝")
				}
				// 交付成功:该条不计丢失。
				if n := s.currentLostEvents(); n != 0 {
					t.Errorf("丢失计数 = %d, want 0(原文已完整交付)", n)
				}
			})
		}
	})

	t.Run("AsyncCallInfo 关联字段异常故障且绝不结算", func(t *testing.T) {
		cases := []struct{ name, payload string }{
			{"缺失 what", `{"async_call_id":1,"details":{"ret":true}}`},
			{"缺失 async_call_id", `{"what":"w","details":{"ret":true}}`},
			{"缺失 details", `{"what":"w","async_call_id":1}`},
			{"缺失 details.ret", `{"what":"w","async_call_id":1,"details":{"cost":1}}`},
			{"重复 what", `{"what":"a","what":"b","async_call_id":1,"details":{"ret":true}}`},
			{"重复 async_call_id", `{"what":"w","async_call_id":1,"async_call_id":2,"details":{"ret":true}}`},
			{"重复 details", `{"what":"w","async_call_id":1,"details":{"ret":true},"details":{"ret":false}}`},
			{"重复 ret", `{"what":"w","async_call_id":1,"details":{"ret":true,"ret":false}}`},
			{"id 超出 int32 上界", `{"what":"w","async_call_id":2147483648,"details":{"ret":true}}`},
			{"id 低于 int32 下界", `{"what":"w","async_call_id":-2147483649,"details":{"ret":true}}`},
			{"id 非精确整数", `{"what":"w","async_call_id":1.5,"details":{"ret":true}}`},
			{"what 类型错误", `{"what":1,"async_call_id":1,"details":{"ret":true}}`},
			{"id 类型错误", `{"what":"w","async_call_id":"1","details":{"ret":true}}`},
			{"details 类型错误", `{"what":"w","async_call_id":1,"details":[]}`},
			{"ret 类型错误", `{"what":"w","async_call_id":1,"details":{"ret":"true"}}`},
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
					t.Fatal("关联字段异常的原文必须逐字节交付")
				}
				closeAndDrain(t, d)

				// 无法关联的完成信息不得视为成功:绝不调用结算通知。
				if got := rec.total(); got != 0 {
					t.Fatalf("结算通知被调用 %d 次, want 0(无法关联的完成信息不得结算)", got)
				}
				if !errors.Is(s.currentFault(), native.ErrProtocol) {
					t.Errorf("currentFault() = %v, want 含 native.ErrProtocol", s.currentFault())
				}
				// 交付成功:该条不计丢失。
				if n := s.currentLostEvents(); n != 0 {
					t.Errorf("丢失计数 = %d, want 0", n)
				}
			})
		}
	})

	t.Run("合法完成恰好一次结算且先结算后投递", func(t *testing.T) {
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
			t.Fatalf("结算调用 %d 次, want 恰 1", len(calls))
		}
		got := calls[0]
		mu.Unlock()
		// json.Number 精确解析:int32 上界 ID 精确读出,不经 float64 往返。
		if want := (completionInfo{ID: 2147483647, What: "AsyncCallFinished", OK: true}); got != want {
			t.Fatalf("结算信息 = %+v, want %+v", got, want)
		}
		// 结算未返回,公开通道此刻不可能有事件。
		select {
		case ev := <-d.events():
			t.Fatalf("结算未返回就完成投递,违反先结算后投递: %+v", ev)
		default:
		}
		close(release) // 放行投递

		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, payload) {
			t.Fatalf("事件原文 = %s, want 大整数与未知字段逐字节保留的 %s", ev.JSON, payload)
		}
		if ev.Sequence != 1 {
			t.Errorf("序号 = %d, want 1", ev.Sequence)
		}
		closeAndDrain(t, d)
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n != 1 {
			t.Fatalf("放行后结算调用累计 %d 次, want 恰 1(结算只能发生一次)", n)
		}
		select {
		case <-s.faultNotify():
			t.Fatal("合法完成不得触发故障")
		default:
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("丢失计数 = %d, want 0", lost)
		}
	})

	t.Run("ret=false 正常完成结算 OK=false 且不故障", func(t *testing.T) {
		rec := newSettleRecorder(2)
		d, s := newTestDispatcher(t, rec.record, generousBudgets())
		d.start()

		payload := []byte(`{"what":"AsyncCallFinished","async_call_id":5,"details":{"ret":false}}`)
		enqueue(t, d, asyncCallInfoMessageID, payload)
		closeAndDrain(t, d)

		if got := rec.total(); got != 1 {
			t.Fatalf("结算通知调用 %d 次, want 1", got)
		}
		if got := rec.all()[0]; got != (completionInfo{ID: 5, What: "AsyncCallFinished", OK: false}) {
			t.Fatalf("结算信息 = %+v, want {5 AsyncCallFinished false}", got)
		}
		// ret=false 是正常完成:不使实例故障,事件照常交付。
		select {
		case <-s.faultNotify():
			t.Fatal("ret=false 是正常完成,不得使实例故障")
		default:
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("丢失计数 = %d, want 0", lost)
		}
		ev := mustReadEvent(t, d.events())
		if !bytes.Equal(ev.JSON, payload) {
			t.Fatalf("事件原文 = %s, want %s", ev.JSON, payload)
		}
	})
}

// TestEventDispatchDrainAndTerminate 钉住验收条件「dispatcher 的排空
// 与终止不等待外部消费者,也不静默丢弃完成信息后宣称结果完整」:
// closeIntake+drain 在消费者停读时有限步完成,已缓冲事件关闭后仍可
// 消费;排空期间已排队的合法完成先结算再尝试投递,投不出的计丢失并
// 按溢出契约报告故障。
func TestEventDispatchDrainAndTerminate(t *testing.T) {
	t.Run("消费者停读时排空有限步完成且缓冲事件仍可消费", func(t *testing.T) {
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
				t.Fatalf("缓冲事件 %d 的 MessageID = %d, want %d(FIFO)", i, ev.MessageID, MessageID(230)+MessageID(i))
			}
			if string(ev.JSON) != string(payloads[i]) {
				t.Fatalf("缓冲事件 %d 原文 = %s, want %s", i, ev.JSON, payloads[i])
			}
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("缓冲事件 %d 序号 = %d, want %d", i, ev.Sequence, i+1)
			}
		}
		if ev, ok := <-d.events(); ok {
			t.Fatalf("排空后公开通道必须已关闭,却读到 %+v", ev)
		}
		// 无法投递的两条计丢失并按溢出契约报告故障。
		if lost := s.currentLostEvents(); lost != 2 {
			t.Errorf("丢失计数 = %d, want 2", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
	})

	t.Run("排空期间排队完成先结算再投递,投不出的计丢失", func(t *testing.T) {
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
			t.Fatalf("结算通知调用 %d 次, want 3(排空期间排队完成同样先结算)", got)
		}
		for i, info := range rec.all() {
			if want := (completionInfo{ID: 9, What: "AsyncCallFinished", OK: true}); info != want {
				t.Fatalf("第 %d 次结算信息 = %+v, want %+v", i+1, info, want)
			}
		}
		// 只有第 1 条进入公开缓冲;投不出的 2 条计丢失并报告溢出故障。
		if n := len(d.events()); n != 1 {
			t.Errorf("公开缓冲滞留事件 = %d, want 1", n)
		}
		if lost := s.currentLostEvents(); lost != 2 {
			t.Errorf("丢失计数 = %d, want 2", lost)
		}
		if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
			t.Errorf("currentFault() = %v, want 含 native.ErrEventOverflow", s.currentFault())
		}
	})
}

// TestEventDispatchSlotDiscipline 钉住 slot 原语纪律(MAA-17 文件头
// panic 边界):commit/abort 各只能结算一次、零值不可用、commit 长度
// 必须与预留一致;abort 精确退还计数与字节预算;序号单调且从 1 开始。
func TestEventDispatchSlotDiscipline(t *testing.T) {
	t.Run("commit 与 abort 各只能调用一次", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(300), 2)
		if err != nil {
			t.Fatalf("准入不得失败: %v", err)
		}
		slot.commit([]byte("ok"))
		// 已 commit 的额度由内部队列持有,直到分发器处理。
		if count, hold := internalBudgetHold(d); count != 1 || hold != 2 {
			t.Fatalf("commit 后内部预算占用 = (%d, %d), want (1, 2)", count, hold)
		}
		assertPanics(t, "commit 后重复 commit", func() { slot.commit([]byte("ok")) })
		assertPanics(t, "commit 后 abort", func() { slot.abort() })
	})

	t.Run("abort 后重复 abort panic", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(300), 2)
		if err != nil {
			t.Fatalf("准入不得失败: %v", err)
		}
		slot.abort()
		assertPanics(t, "abort 后重复 abort", func() { slot.abort() })
	})

	t.Run("零值 slot 不可用", func(t *testing.T) {
		var zero eventSlot
		assertPanics(t, "零值 slot commit", func() { zero.commit([]byte("x")) })
		assertPanics(t, "零值 slot abort", func() { zero.abort() })
	})

	t.Run("commit 长度与预留不符 panic", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(300), 2)
		if err != nil {
			t.Fatalf("准入不得失败: %v", err)
		}
		assertPanics(t, "commit 长度 3 与预留 2 不符", func() { slot.commit([]byte("abc")) })
		// 长度不符的 commit 未结算 slot:额度仍按预留持有,可正常 abort。
		if count, hold := internalBudgetHold(d); count != 1 || hold != 2 {
			t.Fatalf("长度不符 panic 后内部预算占用 = (%d, %d), want (1, 2)", count, hold)
		}
		slot.abort()
	})

	t.Run("abort 退还计数与字节预算并可再次准入", func(t *testing.T) {
		b := generousBudgets()
		b.internalEvents = 1
		b.internalBytes = 4
		d, _ := newTestDispatcher(t, nil, b)

		slot, err := d.admit(MessageID(301), 4)
		if err != nil {
			t.Fatalf("准入不得失败: %v", err)
		}
		if count, hold := internalBudgetHold(d); count != 1 || hold != 4 {
			t.Fatalf("预留后内部预算占用 = (%d, %d), want (1, 4)", count, hold)
		}
		slot.abort()
		if count, hold := internalBudgetHold(d); count != 0 || hold != 0 {
			t.Fatalf("abort 后内部预算占用 = (%d, %d), want (0, 0):计数与字节都必须精确退回", count, hold)
		}
		// 额度已退回:同样的预算可再次准入(此后 abort 收尾)。
		again, err := d.admit(MessageID(301), 4)
		if err != nil {
			t.Fatalf("abort 退回后的准入不得失败: %v", err)
		}
		again.abort()
	})

	t.Run("序号单调且从 1 开始", func(t *testing.T) {
		d, s := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		for i := 0; i < 3; i++ {
			enqueue(t, d, MessageID(302)+MessageID(i), []byte(fmt.Sprintf(`{"i":%d}`, i)))
		}
		for i := 0; i < 3; i++ {
			ev := mustReadEvent(t, d.events())
			if ev.Sequence != uint64(i+1) {
				t.Fatalf("第 %d 条事件序号 = %d, want %d(单调且从 1 开始)", i+1, ev.Sequence, i+1)
			}
			if ev.MessageID != MessageID(302)+MessageID(i) {
				t.Fatalf("第 %d 条事件 MessageID = %d, want %d(FIFO 交付)", i+1, ev.MessageID, MessageID(302)+MessageID(i))
			}
		}
		closeAndDrain(t, d)
		select {
		case <-s.faultNotify():
			t.Fatal("正常交付不得触发故障")
		default:
		}
	})
}

// TestEventDispatchLifecyclePanics 逐条钉住 dispatch.go 声明的接线层
// 编程错误 panic:closeIntake 至多一次、关闭后 admit 即错、start 至多
// 一次、drain 前置契约(start、closeIntake、至多一次)与 drain 请求
// 后 commit 即错。违反契约一律 panic,不允许静默继续。
func TestEventDispatchLifecyclePanics(t *testing.T) {
	t.Run("closeIntake 至多一次", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.closeIntake()
		assertPanics(t, "重复 closeIntake", func() { d.closeIntake() })
	})

	t.Run("closeIntake 后 admit panic", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.closeIntake()
		assertPanics(t, "closeIntake 后 admit", func() { _, _ = d.admit(MessageID(1), 1) })
	})

	t.Run("start 至多一次", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		assertPanics(t, "重复 start", func() { d.start() })
		closeAndDrain(t, d) // 清理分发循环:关门后排空
	})

	t.Run("未 start 即 drain panic", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		assertPanics(t, "未 start 即 drain", func() { d.drain() })
	})

	t.Run("未 closeIntake 即 drain panic", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		assertPanics(t, "intake 未关即 drain", func() { d.drain() })
		// 契约检查失败未登记 drain 请求:正常关门排空,清理分发循环。
		d.closeIntake()
		mustDrain(t, d)
	})

	t.Run("drain 至多一次", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		d.start()
		d.closeIntake()
		d.drain()
		assertPanics(t, "重复 drain", func() { d.drain() })
	})

	t.Run("drain 请求后 commit panic", func(t *testing.T) {
		d, _ := newTestDispatcher(t, nil, generousBudgets())
		slot, err := d.admit(MessageID(303), 2)
		if err != nil {
			t.Fatalf("准入不得失败: %v", err)
		}
		d.start()
		d.closeIntake()
		d.requestDrain()
		assertPanics(t, "drain 请求后 commit", func() { slot.commit([]byte("ok")) })
		// 分发器在空队列 + drain 请求下自行退出并关闭公开通道,无需再
		// 排空(重复 drain 属契约违约)。
	})
}

// TestEventDispatchConcurrentAdmitAndConsume 钉住验收条件「并发
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
					t.Errorf("并发准入不得失败: %v", err)
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
		t.Fatalf("交付事件 = %d, want 恰 %d(每条恰好交付一次)", len(seen), workers*perWorker)
	}
	seqSeen := make(map[uint64]bool, len(seen))
	for _, ev := range seen {
		if seqSeen[ev.Sequence] {
			t.Fatalf("序号 %d 重复分配", ev.Sequence)
		}
		seqSeen[ev.Sequence] = true
	}
	select {
	case <-s.faultNotify():
		t.Fatal("充足预算下的并发准入与消费不得产生溢出故障")
	default:
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0", lost)
	}
}

// TestLostEventsSaturatingAndFaultSnapshot 钉住 MAA-17 在实例状态上
// 增补的故障通知与丢失诊断(§7.1):丢失计数饱和不回绕、首因原文前
// 缀截取至多 4096 字节且深拷贝隔离、重复故障不覆盖首因、诊断快照为
// 独立深拷贝、一次性故障通知恰在首次故障线性化时关闭且正常关闭不触
// 发。
func TestLostEventsSaturatingAndFaultSnapshot(t *testing.T) {
	t.Run("丢失计数饱和不回绕", func(t *testing.T) {
		s := newInstanceState(0x0D01)
		s.recordLostEvents(math.MaxUint64 - 1)
		s.recordLostEvents(1)
		if got := s.currentLostEvents(); got != math.MaxUint64 {
			t.Fatalf("丢失计数 = %d, want %d(恰好到达上限)", got, uint64(math.MaxUint64))
		}
		// 越过上限:夹到 MaxUint64,不回绕。
		s.recordLostEvents(1)
		if got := s.currentLostEvents(); got != math.MaxUint64 {
			t.Fatalf("饱和后再计 1 条 = %d, want 保持 %d(不回绕)", got, uint64(math.MaxUint64))
		}
		s.recordLostEvents(math.MaxUint64)
		if got := s.faultDiagnostics().lostEvents; got != math.MaxUint64 {
			t.Fatalf("快照丢失计数 = %d, want 保持饱和 %d", got, uint64(math.MaxUint64))
		}
	})

	t.Run("前缀截取至多 4096 字节", func(t *testing.T) {
		s := newInstanceState(0x0D02)
		s.publishReady()
		prefix := make([]byte, faultRawPrefixBytes+904)
		for i := range prefix {
			prefix[i] = byte(i % 251)
		}
		cause := fmt.Errorf("首因: %w", native.ErrProtocol)
		s.recordFaultWithDetail(cause, prefix)

		snap := s.faultDiagnostics()
		if len(snap.rawPrefix) != faultRawPrefixBytes {
			t.Fatalf("前缀长度 = %d, want 至多 %d", len(snap.rawPrefix), faultRawPrefixBytes)
		}
		if !bytes.Equal(snap.rawPrefix, prefix[:faultRawPrefixBytes]) {
			t.Fatal("前缀内容与原文前 4096 字节不一致")
		}
		if !errors.Is(snap.firstFault, cause) {
			t.Errorf("快照首因 = %v, want 含 %v", snap.firstFault, cause)
		}
		if got := s.currentPhase(); got != phaseFaulted {
			t.Fatalf("ready 阶段记录故障后阶段 = %d, want faulted", got)
		}
	})

	t.Run("重复故障不覆盖首因与前缀", func(t *testing.T) {
		s := newInstanceState(0x0D03)
		s.publishReady()
		first := fmt.Errorf("首因: %w", native.ErrEventOverflow)
		firstPrefix := []byte("first-prefix")
		s.recordFaultWithDetail(first, firstPrefix)
		second := fmt.Errorf("后到: %w", native.ErrProtocol)
		s.recordFaultWithDetail(second, bytes.Repeat([]byte("second!"), 1000))

		snap := s.faultDiagnostics()
		if !errors.Is(snap.firstFault, first) {
			t.Errorf("首因被后到故障覆盖: %v", snap.firstFault)
		}
		if errors.Is(snap.firstFault, native.ErrProtocol) {
			t.Errorf("后到故障混入首因: %v", snap.firstFault)
		}
		if !bytes.Equal(snap.rawPrefix, firstPrefix) {
			t.Errorf("前缀被后到记录覆盖 = %q, want %q", snap.rawPrefix, firstPrefix)
		}
		if snap.lostEvents != 0 {
			t.Errorf("recordFaultWithDetail 不得改丢失计数,得到 %d", snap.lostEvents)
		}
	})

	t.Run("前缀与快照都是深拷贝", func(t *testing.T) {
		s := newInstanceState(0x0D04)
		s.publishReady()
		prefix := bytes.Repeat([]byte{0xAB}, 64)
		s.recordFaultWithDetail(fmt.Errorf("故障: %w", native.ErrProtocol), prefix)

		// 原地改写调用者缓冲:不得穿透已保存前缀。
		for i := range prefix {
			prefix[i] = 0xCD
		}
		snap1 := s.faultDiagnostics()
		if !bytes.Equal(snap1.rawPrefix, bytes.Repeat([]byte{0xAB}, 64)) {
			t.Fatal("改写调用者原文影响了快照:前缀不是深拷贝")
		}
		// 原地改写快照:不得穿透状态内原值与后续快照。
		for i := range snap1.rawPrefix {
			snap1.rawPrefix[i] = 0xEF
		}
		snap2 := s.faultDiagnostics()
		if !bytes.Equal(snap2.rawPrefix, bytes.Repeat([]byte{0xAB}, 64)) {
			t.Fatal("改写快照影响了状态内原值:快照不是深拷贝")
		}
	})

	t.Run("无前缀故障快照 rawPrefix 为 nil", func(t *testing.T) {
		s := newInstanceState(0x0D05)
		s.publishReady()
		s.recordFault(fmt.Errorf("无前缀: %w", native.ErrEventOverflow))
		if snap := s.faultDiagnostics(); snap.rawPrefix != nil {
			t.Errorf("无前缀故障的快照 rawPrefix = %q, want nil", snap.rawPrefix)
		}
	})

	t.Run("故障通知恰一次且正常关闭不触发", func(t *testing.T) {
		// 正常关闭:closeDone 关闭但故障通知保持开放。
		closed := newClosedState(t, 0x0D06)
		select {
		case <-closed.faultNotify():
			t.Fatal("正常关闭不得触发故障通知")
		default:
		}
		select {
		case <-closed.done():
		default:
			t.Fatal("正常关闭必须关闭 closeDone")
		}
		// 首次故障线性化时关闭一次;重复记录不重复关闭(重复 close 会
		// panic,实现若有缺陷将以 panic 暴露)。
		s := newInstanceState(0x0D07)
		s.publishReady()
		s.recordFault(fmt.Errorf("第一次: %w", native.ErrProtocol))
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("首次故障必须关闭故障通知")
		}
		s.recordFault(fmt.Errorf("第二次: %w", native.ErrEventOverflow))
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("故障通知必须保持已关闭")
		}
	})
}
