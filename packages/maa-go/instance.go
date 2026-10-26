// 本文件实现实例的单次关闭流程(MAA-20;绑定设计 §7、§8、§8.1,
// 停止策略已由 MAA-03 冻结):私有 instance 把 MAA-16 的共享状态与
// 调用准入、MAA-17 的有界事件分发器、MAA-18 的 callback 准入与
// token 注册表、MAA-19 的 pending Operation 结算组装为唯一清理流程,
// 阶段固定如下——
//
//  1. 在状态锁内选主(§8 第 1 步):恰一个申请者把 ready/faulted 转
//     为 closing 并成为所有者,拒绝新业务调用;其余申请者(并发竞争
//     者与之后的幂等申请)经 waitClose 等待同一 closeDone 并取得同一
//     最终错误。不照搬参考实现「忙时返回 ErrInUse」的销毁策略。
//  2. 不持状态锁等待全部已准入调用退出(§8 第 2 步),含尚在 raw 串
//     行门禁前排队的调用(先取得 lease 再排队,已计入在途计数);不
//     用 context 取消强行使 native 调用返回。
//  3. 所有者读取受门控的 handle:有效非零 handle 无条件执行一次
//     Stop 再一次 Destroy(§8.1:不先查询 Running,不因之前调用过公
//     开 Stop 而省略;CreateEx=0 的路径不调用 Stop/Destroy)。native
//     调用期间不持状态锁、callback 门、注册表锁或事件消费所需的锁;
//     callback 准入门仍开放,允许同步 Destroyed 复制入队(§8 第 3
//     步;锁定 Core 在销毁线程 join 后于 Destroy 调用线程同步触发
//     Destroyed——Assistant 析构,MaaAssistantArknights
//     src/MaaCore/Assistant.cpp)。Stop 返回 0 保存含接口与原始 bool
//     的 ErrNativeRejected 诊断后继续 Destroy,不证明 handle 已被销
//     毁,也不把剩余 Operation 结为成功;Destroy 是 C void 函数,只
//     有「调用已返回」这个事实,不从 void 返回制造成功码(§8.1)。
//  4. Destroy 返回后关闭 callback 准入门,等待全部已准入 callback
//     退出,再移除 token(§8 第 4 步;最后一个回调的复制/投递与关
//     闭通道不能竞态——关门、等待退空先行,token 移除在后)。
//  5. 排空分发器并结算剩余(§8 第 5 步、§8.1):关闭入口并请求排
//     空,等待发送者停止点——dispatcher 对内部队列每项先结算可确认
//     结果、再非阻塞投递公开通道,无法投递的按溢出契约计丢失并记录
//     故障;到达停止点(队列清空)后,把剩余 pending 以 ErrClosed 结
//     算一次(sticky 首因场景下首因线性化扫描已统一结算,此处为无害
//     空操作,不覆盖已发布终态),先发布最终错误和 closed 状态,最后
//     才由通道所有者关闭 Events(§8.1:Events 的发送者停止、所有
//     callback 退出、Operation 全部结算后,才关闭 Events;固定容量
//     和关闭顺序是本次冻结的验收要求)。Done 与 closeDone 由
//     state.go 冻结的 publishClosed 原子发布——§8 第 5 步「再由通道
//     所有者关闭 Events、Done 和 closeDone」据此落实为「发布(含
//     Done/closeDone)先于 Events 关闭」。
//
// 复用边界(§8.1):同一流程供创建失败回滚(MAA-21,已取得 handle
// 后失败执行同一 Stop/Destroy)与 Runtime.Close(MAA-25,委托同一流
// 程,不重复 Destroy)复用;并发 Instance.Close、Runtime.Close 与创
// 建失败回滚共享该清理所有权。公开 Instance 类型与 Close 公开方法、
// NewInstance 创建事务与统一失败回滚入口由创建接线任务(MAA-21)在
// 本文件交付;close 与 closeTail 保持同一流程、不建立第二套清理。
//
// 无超时契约(§8):Close 不接受强制回收超时,也不承诺在 native 卡
// 住时返回;Stop 或 Destroy 不返回时保留状态、token 与已准入引用,
// 由 control 对 worker 执行有期限的退出监督,最终终止进程。binding
// 不把任何等待超时描述为原生资源已安全回收,后续先核对设备状态,不
// 自动重试可能消耗资源的动作。
//
// 锁纪律:close 全程在状态锁、callback 门、注册表锁与分发器锁之外
// 执行 native 调用;Destroyed 等同步回调经仍开放的准入门照常准入,
// 与关闭流程无共享锁段,不锁死(§8:普通消息线程内直接 Destroy 会
// join 自己,Destroyed 由 Destroy 调用线程同步触发,fake native 用例
// 复现该形态)。错误组合(§8.1):已有首次故障/创建失败是主因,停
// 止拒绝作为唯一附加清理诊断保留,两者均沿错误链可辨;没有主因时停
// 止拒绝成为 Close 的原因;正常 Stop 和 Destroy 返回且无其他故障则
// Close 为 nil。

package maa

import (
	"fmt"
	"runtime"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// instanceCloser 是关闭流程的 native 注入边界(对照 runtime.go 的
// initSession 先例):生产实现转发 calls.go 的 raw 调用包装,fake 实
// 现供受控用例计数与屏障,不替换生产包的全局函数变量。接口刻意不
// 暴露 Running——§8.1:关闭所有者无条件执行一次 Stop,不先查询
// Running;实现若想查询必须显式扩展接口,膨胀即被审查发现。
type instanceCloser interface {
	// stop 对应 AsstStop(handle):返回 0 表示原生拒绝(C bool=false,
	// 无结构化原因)。调用契约:仅在 electClose 胜出、全部调用 lease
	// 退出后由关闭所有者调用一次。
	stop(handle uintptr) uint8
	// destroy 对应 AsstDestroy(handle):C void 函数,只证明调用已返回,
	// 没有成功码;锁定 Core 在销毁线程 join 后于本调用线程同步触发
	// Destroyed 回调。调用契约:紧随 stop 之后由关闭所有者调用一次。
	destroy(handle uintptr)
}

// apiTableCloser 是 instanceCloser 的生产实现:转发 *native.APITable
// 的 Stop/Destroy raw 调用包装,不解释返回值、不重试、不编造诊断。
type apiTableCloser struct {
	table *native.APITable
}

func (c apiTableCloser) stop(handle uintptr) uint8 { return c.table.Stop(handle) }

func (c apiTableCloser) destroy(handle uintptr) { c.table.Destroy(handle) }

// instance 是一个实例的私有组装:持有共享状态(MAA-16)、事件分发
// 器(MAA-17)、callback 准入接缝与 token(MAA-18)及 native 停止/
// 销毁接缝。零值不可用,只能经 newInstance 构造,以指针使用,不按值
// 复制。token 是经 native.RegisterCallbackTarget 登记的进程内唯一
// 非零 token,登记的投递函数必须挂接同一 state/disp 的 callback 准
// 入接缝;close 恰好把它移除一次。
type instance struct {
	state  *instanceState
	disp   *eventDispatcher
	token  uintptr
	closer instanceCloser
}

// newInstance 构造私有实例组装:state 与 disp 均不得为 nil(状态与
// 分发是核心语义),closer 不得为 nil(native 停止/销毁不可缺席),
// token 必须非零(生产接线在 CreateEx 前经 native.RegisterCallbackTarget
// 登记;0 不会被注册表接受,§4),违反即接线层编程错误,panic(对照
// newCallbackTarget/newEventDispatcher 先例)。state 必须已 publishReady
// (或创建期故障路径的 faulted),disp 必须已 start:close 的排空段
// 以已启动的分发循环为前置契约,违反由 drain 按编程错误 panic 暴露。
func newInstance(state *instanceState, disp *eventDispatcher, token uintptr, closer instanceCloser) *instance {
	if state == nil {
		panic("maa: instance created without instance state")
	}
	if disp == nil {
		panic("maa: instance created without event dispatcher")
	}
	if closer == nil {
		panic("maa: instance created without a native closer")
	}
	if token == 0 {
		panic("maa: instance created without a registered callback token")
	}
	return &instance{state: state, disp: disp, token: token, closer: closer}
}

// close 执行实例的单次关闭:幂等,返回稳定的最终错误(正常关闭为
// nil)。阶段与纪律见文件头;多次调用(含并发)只有一个所有者执行
// 一次 Stop/Destroy,其余申请者等待同一 closeDone 并取得同一最终错
// 误(§8.1:多次 Close 返回同一终态)。该方法即公开 Instance.Close
// 的唯一实现,创建失败回滚与 Runtime.Close 复用同一流程。
func (in *instance) close() error {
	// 1. 选主(§8 第 1 步):败者与幂等申请共享同一 closeDone 与最终
	//    错误存储,不重复执行清理。
	if !in.state.electClose() {
		return in.state.waitClose()
	}

	// 2. 等待在途调用清零(§8 第 2 步):Cond.Wait 挂起期间不持状态
	//    锁;raw 门禁前的排队者已计入在途计数,不会被漏掉。
	in.state.waitCallsDrained()

	// 3. 一次 Stop、一次 Destroy(§8.1):handle=0(CreateEx=0 的回滚
	//    路径)不调用 Stop/Destroy;native 调用不持任何锁,callback 门
	//    仍开放,同步 Destroyed 照常入队。Stop 拒绝只保存清理诊断,
	//    继续 Destroy;Destroy 无成功码,不制造成功证据。
	handle, ok := in.state.closeHandle()
	var stopDiag error
	if ok {
		if in.closer.stop(handle) == 0 {
			stopDiag = fmt.Errorf("maa: AsstStop returned 0 (false) for handle %#x during instance close: %w",
				handle, native.ErrNativeRejected)
		}
		in.closer.destroy(handle)
	}
	return closeTail(in.state, in.disp, in.token, stopDiag)
}

// closeTail 是单次关闭流程在「选主、在途调用清零、一次 Stop 与一次
// Destroy」之后的公共尾段(§8 第 4、5 步),供关闭流程本体与创建回
// 滚的 token 登记失败路径(token=0,不存在可注销的登记)复用,保证
// 两条路径的关门、退空、token 移除、排空、结算与终态发布顺序逐字一
// 致——这是 §8.1「复用同一流程、不建立第二套清理」的实现方式:
//
//   - Destroy 返回后关闭 callback 准入门,等待全部已准入 callback
//     退出,再移除 token(§8 第 4 步);token=0 表示登记从未成功,
//     无 token 可移除(注册表里也从不可能出现该 token,不会有任何回
//     调准入);
//   - 排空分发器并结算剩余,发布最终错误(composeCloseError 组合首
//     因与停止拒绝诊断)与 closed 状态,最后关闭 Events。
//
// 调用契约:状态已处于 closing(选主胜出)、在途调用已清零、native
// 停止/销毁段已结束;stopDiag 是 Stop 拒绝的清理诊断(无则 nil)。
func closeTail(s *instanceState, d *eventDispatcher, token uintptr, stopDiag error) error {
	// 4. 关门、等待退空、移除 token(§8 第 4 步): Destroy 返回后才
	//    关闭 callback 准入;最后一个已准入 callback 退出后才移除
	//    token,释放注册表对投递函数(及其捕获的实例状态)的强引用。
	s.closeCallbackGate()
	s.waitCallbacksDrained()
	if token != 0 {
		native.UnregisterCallbackTarget(token)
	}

	// 5. 排空分发器并结算剩余(§8 第 5 步、§8.1):关闭入口并请求排
	//    空,等待发送者停止点——dispatcher 对每项先结算可确认结果、
	//    再非阻塞投递公开通道,无法投递的按溢出契约计丢失并记录故障;
	//    停止点之后把剩余 pending 以 ErrClosed 结算一次(首因场景下表
	//    已空,无害),发布最终错误与 closed 状态,最后才由通道所有者
	//    触发 Events 关闭——Events 的关闭晚于发送者停止、全部
	//    Operation 结算与终态发布,Done 的观察者无需先消费 Events。
	d.closeIntake()
	d.requestDrain()
	d.waitProcessed()
	s.settleRemainingPending()
	s.publishFinalError(composeCloseError(s.currentFault(), stopDiag))
	s.publishClosed()
	// 6. 关闭 Events 并收敛(§8 第 5 步):通道所有者触发公开通道关
	//    闭并等待分发器退出,不残留悬挂 goroutine;返回值即已存储的最
	//    终错误——发布先于 Events 关闭与任一 Close 返回,观察者见到
	//    Done 或 Close 返回即见稳定 Err 与全部 Operation 终态。
	d.closeEvents()
	return s.finalError()
}

// composeCloseError 组合关闭的最终错误(§8.1):已有首次故障/创建失
// 败是主因,停止拒绝作为唯一附加清理诊断保留,两者均沿错误链可辨
// (Go 1.20+ 多 %w 包装,errors.Is 对两者可达);没有主因时停止拒绝
// 成为 Close 的原因;两者皆无返回 nil(正常 Stop 和 Destroy 返回且
// 无其他故障)。fault 原样返回时保持错误值同一,失败者与所有者共享
// 同一存储值。
func composeCloseError(fault, stopDiag error) error {
	switch {
	case fault != nil && stopDiag != nil:
		return fmt.Errorf("maa: instance close kept the first fault and the stop diagnostic: %w; stop diagnostic: %w",
			fault, stopDiag)
	case fault != nil:
		return fault
	default:
		return stopDiag
	}
}

// Instance 是一个已成功创建的 MaaCore 实例绑定:全部字段未导出,只
// 能经 Runtime.NewInstance 返回的指针使用,不按值复制(§5:仅通过构
// 造函数返回的指针使用)。在 NewInstance 全部创建步骤成功之前,调用
// 者无法从公开面获得本类型——不存在可获得的半初始化实例(§8)。
//
// 业务方法(连接、任务、事件流与结果读取)由后续里程碑接入;当前
// 公开面只有 Close。
type Instance struct {
	// inst 是私有单次关闭组件(MAA-20):持有共享状态、事件分发器、
	// callback token 与 native 停止/销毁接缝,Close 委托它的唯一流程。
	inst *instance
	// rt 是创建本实例的 Runtime:实例关闭后经它的生命周期门进入终止
	// 使用状态(§8:一个实例关闭后 Runtime 进入终止态)。
	rt *Runtime
}

// Close 同步销毁实例:幂等,返回稳定的最终错误(正常关闭为 nil)。
// 它是单次关闭流程的公开入口(§5:幂等、同步销毁;返回后不再允许
// 原生调用;§8:并发 Close 共享同一 closeDone 和最终错误,只有一个
// 关闭所有者执行 Stop/Destroy):多次或并发调用只有一个所有者执行一
// 次 Stop、一次 Destroy,其余申请者等待同一关闭结果并取得同一最终
// 错误;Close 不接受强制回收超时,Stop/Destroy 卡住时由 control 对
// worker 执行有期限的退出监督(§8)。
//
// 实例关闭完成(包括关闭失败返回错误)后,创建它的 Runtime 进入终
// 止使用状态(§8:一个实例关闭后 Runtime 进入终止态,不再允许
// NewInstance/LoadResource):后续 Open 与 NewInstance 一律返回包装
// ErrRuntimeTerminated 的错误;不在同进程重建实例。
func (in *Instance) Close() error {
	err := in.inst.close()
	// §8:关闭错误保留首个故障及停止诊断,不从 Destroy 的 void 返回
	// 值制造错误码;正常关闭 err 为 nil,终止使用不携带失败原因。
	in.rt.gate.terminate(err)
	return err
}

// instanceCreator 是创建事务的 native 注入边界(对照 instanceCloser
// 先例):生产实现转发 calls.go 的 raw 调用包装,fake 实现供受控用例
// 记录调用与注入失败,不替换生产包的全局函数变量。
type instanceCreator interface {
	// createEx 对应 AsstCreateEx(callback, customArg):callback 是
	// 进程唯一的 callback trampoline 地址,customArg 是已登记的整数
	// token;返回 0(nullptr)表示创建失败。调用契约:仅在唯一实例
	// 名额已预留、实例状态/事件入口/callback 门/token 已建立之后由
	// 创建者调用一次(§4、§8)。
	createEx(callback uintptr, customArg uintptr) uintptr
	// setInstanceOption 对应 AsstSetInstanceOption(handle, key, value):
	// 返回 0 表示原生拒绝。调用契约:仅在 CreateEx 返回有效非零
	// handle 之后由创建者按配置顺序调用。
	setInstanceOption(handle uintptr, key int32, value *byte) uint8
}

// apiTableCreator 是 instanceCreator 的生产实现:转发 *native.APITable
// 的 CreateEx/SetInstanceOption raw 调用包装,不解释返回值、不重试、
// 不编造诊断。
type apiTableCreator struct {
	table *native.APITable
}

func (c apiTableCreator) createEx(callback uintptr, customArg uintptr) uintptr {
	return c.table.CreateEx(callback, customArg)
}

func (c apiTableCreator) setInstanceOption(handle uintptr, key int32, value *byte) uint8 {
	return c.table.SetInstanceOption(handle, key, value)
}

// createInstance 是 NewInstance 的完整创建事务(§8 初始化第 2 阶段),
// 执行顺序严格按绑定设计:
//
//  1. 纯 Go 配置校验与深拷贝快照(§6.1:在预留名额前复制;失败不消
//     耗 Runtime,发生在触碰生命周期门之前);
//  2. 预留唯一实例名额并登记创建 lease(Runtime 状态门内);
//  3. 建立实例共享状态、事件入口与 callback 准入接缝,登记 token
//     (§4:注册表、事件队列先于 CreateEx 建立,创建期间的早到回调
//     也能找到实例状态,不遗失状态);
//  4. CreateEx(callback=进程唯一 trampoline,customArg=token);返回
//     0 走统一回滚(零 handle:不调用无效 handle,不执行 Stop/Destroy);
//  5. 按序应用实例选项,任何一次原生拒绝走统一回滚;
//  6. 在 Runtime 同一状态门内复核可发布性并原子复核早到故障:已
//     closing 或已有早到故障都不发布,由创建者完成统一回滚;
//  7. 全部成功才发布 ready 并返回 *Instance,释放创建 lease。
//
// 预占名额后的任何失败(§8:预占名额后的创建失败使 Runtime 终止使
// 用)都经 rollbackCreation 收尾:记录创建失败、复用单次关闭流程清
// 理、终止 Runtime 使用、释放创建 lease;调用者只拿到错误,拿不到
// 半初始化实例。锁序:Runtime 状态门(lifecycleGate.mu)→ 实例状态
// 锁(publishInstance 内),无反向获取。
func createInstance(rt *Runtime, cfg InstanceConfig, creator instanceCreator, closer instanceCloser) (*Instance, error) {
	// 1. 预占前:纯 Go 校验 + 深拷贝快照(预算取 Open 时已校验冻结的
	//    Limits 快照,InstanceConfig 不覆盖 Runtime 的限制,§5.1)。
	snap, err := snapshotInstanceConfig(cfg, rt.limits)
	if err != nil {
		return nil, err
	}
	// 2. 预留唯一实例名额并登记创建 lease(§8 NewInstance 第 2 步):
	//    已有实例、Runtime 已 closing 或已终止使用都在此拒绝。
	if err := rt.gate.reserveInstance(); err != nil {
		return nil, err
	}
	// 以下任何失败都必须经 rollbackCreation 收尾,不允许半途返回。

	// 3. 建立实例共享状态与事件入口(handle=0:CreateEx 返回前尚未
	//    取得句柄),启动分发循环,构造 callback 准入接缝并登记 token
	//    (§8 NewInstance 第 3 步;callback 门从构造起开放,早到消息
	//    照常准入入队)。
	s := newInstanceState(0, rt.limits.PendingOperations)
	d := newEventDispatcher(s, s.settleCompletion,
		rt.limits.CallbackBytes, rt.limits.InternalEvents, rt.limits.InternalBytes,
		rt.limits.PublicEvents, rt.limits.PublicBytes)
	d.start()
	target := newCallbackTarget(s, d, rt.limits.CallbackBytes)
	token, err := native.RegisterCallbackTarget(target.deliver)
	if err != nil {
		// token 登记失败:尚无 instance 组装、无 handle、注册表里也没
		// 有本实例的 token(不可能有回调准入);统一回滚按 token=0 收尾。
		return nil, rollbackCreation(rt, s, d, 0, closer, err)
	}
	in := newInstance(s, d, token, closer)

	// 4. CreateEx(§8 NewInstance 第 4 步):callback 与 token 已可用。
	handle := creator.createEx(native.CallbackTrampoline(), token)
	if handle == 0 {
		// 零 handle(§8:CreateEx 返回 0 时清理临时 token 和 Go 状态):
		// 回滚不调用无效 handle,也不执行 Stop/Destroy(§8.1)。
		return nil, rollbackCreation(rt, s, d, token, closer,
			fmt.Errorf("maa: AsstCreateEx returned 0 (null handle): %w", native.ErrNativeRejected))
	}
	// 登记句柄:早到故障可能已先进入 faulted,回滚仍需要句柄销毁。
	s.publishHandle(handle)

	// 5. 按序应用实例选项(§5.1:有序 Options;option value 允许空串,
	//    编码为空 C 字符串,不传 nil)。返回 0 即原生拒绝,无结构化
	//    原因,不编造 LastError。
	for i, opt := range snap.options {
		value, valueBuf := cString(opt.Value)
		ret := creator.setInstanceOption(handle, opt.Key, value)
		runtime.KeepAlive(valueBuf)
		if ret == 0 {
			return nil, rollbackCreation(rt, s, d, token, closer,
				fmt.Errorf("maa: SetInstanceOption(key=%d, Options[%d]) returned failure: %w",
					opt.Key, i, native.ErrNativeRejected))
		}
	}

	// 6. Runtime 同一状态门内复核可发布性并原子复核早到故障(§8:创
	//    建者在 Runtime 的同一状态门内决定发布):已 closing 返回
	//    ErrClosing,已有早到故障返回首因;两者都不发布,由创建者完
	//    成统一回滚后释放 lease,Runtime.Close 等待该流程。
	if err := rt.gate.publishInstance(in); err != nil {
		return nil, rollbackCreation(rt, s, d, token, closer, err)
	}
	// 7. 发布成功:释放创建 lease(§8:Runtime.Close 等待该流程,不重
	//    复 Destroy)并返回公开实例——至此全部步骤成功。
	rt.gate.releaseCreationLease()
	return &Instance{inst: in, rt: rt}, nil
}

// rollbackCreation 是创建事务的统一失败回滚入口(§8:CreateEx 返回 0
// 时清理临时 token 和 Go 状态;已创建 handle 后失败则执行同一
// Stop/Destroy 流程;预占名额后的创建失败使 Runtime 终止使用):
//
//  1. 以 cause 记录创建失败故障(sticky:早到故障已是首因时保留首因,
//     本 cause 不覆盖,阶段转入/保持 faulted);
//  2. token 已登记时复用 MAA-20 单次关闭流程(electClose 由创建者胜
//     出——尚无公开实例,不存在其他关闭者):handle 非零执行一次
//     Stop、一次 Destroy,零 handle 跳过;关门、等待退空、移除 token、
//     排空结算、发布终态全部照常;token 登记失败(token=0)时由创建
//     者代行唯一关闭所有者走同一 closeTail 尾段,无 token 可移除;
//  3. 终止 Runtime 使用(后续 Open/NewInstance 返回
//     ErrRuntimeTerminated,附首个失败原因);
//  4. 释放创建 lease(§8:回滚流程完成后释放,Runtime.Close 的等待
//     此后才能返回)。
//
// 返回值是单次关闭的最终错误(与首因同一存储值,停止拒绝诊断沿错误
// 链附加),即返回给 NewInstance 调用者的失败原因。
func rollbackCreation(rt *Runtime, s *instanceState, d *eventDispatcher, token uintptr, closer instanceCloser, cause error) error {
	s.recordFault(cause)
	var err error
	if token != 0 {
		err = newInstance(s, d, token, closer).close()
	} else {
		// token 登记失败:没有 instance 组装,也没有任何回调能找到本
		// 实例(注册表无 token);创建者代行唯一关闭所有者,handle 为 0,
		// 尾段不执行 Stop/Destroy、不注销 token。
		if !s.electClose() {
			err = s.waitClose()
		} else {
			s.waitCallsDrained()
			err = closeTail(s, d, 0, nil)
		}
	}
	rt.gate.terminate(err)
	rt.gate.releaseCreationLease()
	return err
}
