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
// 建失败回滚共享该清理所有权。公开 Instance 类型与 Close 公开方法
// 由创建接线任务引入,本单元保持公开面(导出符号)零变化。
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
// 误(§8.1:多次 Close 返回同一终态)。该方法即未来公开 Instance.
// Close 的唯一实现,创建失败回滚与 Runtime.Close 复用同一流程。
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

	// 4. 关门、等待退空、移除 token(§8 第 4 步): Destroy 返回后才
	//    关闭 callback 准入;最后一个已准入 callback 退出后才移除
	//    token,释放注册表对投递函数(及其捕获的实例状态)的强引用。
	in.state.closeCallbackGate()
	in.state.waitCallbacksDrained()
	native.UnregisterCallbackTarget(in.token)

	// 5. 排空分发器并结算剩余(§8 第 5 步、§8.1):关闭入口并请求排
	//    空,等待发送者停止点——dispatcher 对每项先结算可确认结果、
	//    再非阻塞投递公开通道,无法投递的按溢出契约计丢失并记录故障;
	//    停止点之后把剩余 pending 以 ErrClosed 结算一次(首因场景下表
	//    已空,无害),发布最终错误与 closed 状态,最后才由通道所有者
	//    触发 Events 关闭——Events 的关闭晚于发送者停止、全部
	//    Operation 结算与终态发布,Done 的观察者无需先消费 Events。
	in.disp.closeIntake()
	in.disp.requestDrain()
	in.disp.waitProcessed()
	in.state.settleRemainingPending()
	in.state.publishFinalError(composeCloseError(in.state.currentFault(), stopDiag))
	in.state.publishClosed()
	// 6. 关闭 Events 并收敛(§8 第 5 步):通道所有者触发公开通道关
	//    闭并等待分发器退出,不残留悬挂 goroutine;返回值即已存储的最
	//    终错误——发布先于 Events 关闭与任一 Close 返回,观察者见到
	//    Done 或 Close 返回即见稳定 Err 与全部 Operation 终态。
	in.disp.closeEvents()
	return in.state.finalError()
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
