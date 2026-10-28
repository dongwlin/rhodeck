// 本文件实现实例的单次关闭流程:私有 instance 把共享状态与调用准入、
// 有界事件分发器、callback 准入与 token 注册表、pending Operation 结算
// 组装为唯一清理流程,阶段固定如下——
//
//  1. 在状态锁内选主:恰一个申请者把 ready/faulted 转为 closing 并成为
//     所有者,拒绝新业务调用;其余申请者经 waitClose 等待同一 closeDone
//     并取得同一最终错误。
//  2. 不持状态锁等待全部已准入调用退出,含尚在 raw 串行门禁前排队的
//     调用;不用 context 取消强行使 native 调用返回。
//  3. 所有者读取受门控的 handle:有效非零 handle 无条件执行一次 Stop
//     再一次 Destroy,不先查询 Running,也不因之前调用过公开 Stop 而
//     省略;CreateEx=0 的路径不调用 Stop/Destroy。native 调用期间不持
//     状态锁、callback 门、注册表锁或事件消费所需的锁;callback 准入门
//     仍开放,允许同步 Destroyed 复制入队。Stop 返回 0 时保存含接口与
//     原始 bool 的 ErrNativeRejected 诊断后继续 Destroy;Destroy 是 C
//     void 函数,只证明调用已返回。
//  4. Destroy 返回后关闭 callback 准入门,等待全部已准入 callback 退出,
//     再移除 token。关门、等待退空先行,token 移除在后。
//  5. 排空分发器并结算剩余:关闭入口并请求排空,等待发送者停止点;
//     dispatcher 对内部队列每项先结算可确认结果、再非阻塞投递公开通道,
//     无法投递的按溢出契约计丢失并记录故障;到达停止点后把剩余 pending
//     以 ErrClosed 结算一次,先发布最终错误和 closed 状态,最后由通道
//     所有者关闭 Events,分发器退出后再发布关闭完成通知。
//
// 同一流程供创建失败回滚(已取得 handle 后失败执行同一 Stop/Destroy)与
// Runtime.Close 复用;并发 Instance.Close、Runtime.Close 与创建失败回滚
// 共享该清理所有权。
//
// 无超时契约:Close 不接受强制回收超时,也不承诺在 native 卡住时返回;
// Stop 或 Destroy 不返回时保留状态、token 与已准入引用,由外部 supervisor
// 执行有期限的退出监督并最终终止进程。binding 不把任何等待超时描述为
// 原生资源已安全回收。
//
// 锁纪律:close 全程在状态锁、callback 门、注册表锁与分发器锁之外执行
// native 调用;Destroyed 等同步回调经仍开放的准入门照常准入。错误组合:
// 已有首次故障/创建失败是主因,停止拒绝作为唯一附加清理诊断保留,两者
// 均沿错误链可辨;没有主因时停止拒绝成为 Close 的原因;正常 Stop 和
// Destroy 返回且无其他故障则 Close 为 nil。

package maa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"unicode/utf8"
	"unsafe"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// instanceCloser 是关闭流程的 native 注入边界(对照 runtime.go 的
// initSession 先例):生产实现转发 calls.go 的 raw 调用包装,fake 实
// 现供受控用例计数与屏障,不替换生产包的全局函数变量。接口刻意不
// 暴露 Running——:关闭所有者无条件执行一次 Stop,不先查询
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

// instance 是一个实例的私有组装:持有共享状态、事件分发
// 器、callback 准入接缝与 token()及 native 停止/
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
// 登记;0 不会被注册表接受),违反即接线层编程错误,panic(对照
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
// 误(多次 Close 返回同一终态)。该方法即公开 Instance.Close
// 的唯一实现,创建失败回滚与 Runtime.Close 复用同一流程。
func (in *instance) close() error {
	// 1. 选主(第 1 步):败者与幂等申请共享同一 closeDone 与最终
	//    错误存储,不重复执行清理。
	if !in.state.electClose() {
		return in.state.waitClose()
	}

	// 2. 等待在途调用清零(第 2 步):Cond.Wait 挂起期间不持状态
	//    锁;raw 门禁前的排队者已计入在途计数,不会被漏掉。
	in.state.waitCallsDrained()

	// 3. 一次 Stop、一次 Destroy():handle=0(CreateEx=0 的回滚
	//    路径)不调用 Stop/Destroy;native 调用不持任何锁,callback 门
	//    仍开放,同步 Destroyed 照常入队。Stop 拒绝只保存清理诊断,
	//    继续 Destroy;Destroy 无成功码,不制造成功证据。
	handle, ok := in.state.closeHandle()
	if ok {
		if in.closer.stop(handle) == 0 {
			in.state.recordStopDiagnostic(fmt.Errorf("maa: AsstStop returned 0 (false) for handle %#x during instance close: %w",
				handle, native.ErrNativeRejected))
		}
		in.closer.destroy(handle)
	}
	return closeTail(in.state, in.disp, in.token)
}

// closeTail 是单次关闭流程在「选主、在途调用清零、一次 Stop 与一次
// Destroy」之后的公共尾段(第 4、5 步),供关闭流程本体与创建回
// 滚的 token 登记失败路径(token=0,不存在可注销的登记)复用,保证
// 两条路径的关门、退空、token 移除、排空、结算与终态发布顺序逐字一
// 致——这是 「复用同一流程、不建立第二套清理」的实现方式:
//
//   - Destroy 返回后关闭 callback 准入门,等待全部已准入 callback
//     退出,再移除 token(第 4 步);token=0 表示登记从未成功,
//     无 token 可移除(注册表里也从不可能出现该 token,不会有任何回
//     调准入);
//   - 排空分发器并结算剩余,发布最终错误(composeCloseError 组合首
//     因与停止拒绝诊断)与 closed 状态,关闭 Events 后发布关闭完成通知。
//
// 调用契约:状态已处于 closing(选主胜出)、在途调用已清零、native
// 停止/销毁段已结束,Stop 拒绝的清理诊断已保存至状态。
func closeTail(s *instanceState, d *eventDispatcher, token uintptr) error {
	// 4. 关门、等待退空、移除 token(第 4 步): Destroy 返回后才
	//    关闭 callback 准入;最后一个已准入 callback 退出后才移除
	//    token,释放注册表对投递函数(及其捕获的实例状态)的强引用。
	s.closeCallbackGate()
	s.waitCallbacksDrained()
	if token != 0 {
		native.UnregisterCallbackTarget(token)
	}

	// 5. 排空分发器并结算剩余(第 5 步、):关闭入口并请求排
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
	s.publishFinalError(s.currentError())
	s.publishClosed()
	// 6. 关闭 Events、等待分发器退出,再通知 Done/closeDone。
	//    并发 Close 的等待者不能早于 Events 关闭返回;Done 的观察者
	//    无需消费 Events 即可见完整关闭结果与稳定的 Err/Operation。
	d.closeEvents()
	s.publishCloseDone()
	return s.finalError()
}

// composeCloseError 组合关闭的最终错误:已有首次故障/创建失
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
// 能经 Runtime.NewInstance 返回的指针使用,不按值复制(仅通过构
// 造函数返回的指针使用)。在 NewInstance 全部创建步骤成功之前,调用
// 者无法从公开面获得本类型——不存在可获得的半初始化实例。
//
// 公开业务方法包括同步任务/请求/查询方法(AppendTask、
// SetTaskParams、Start、Stop、BackToHome、Running、Connected)、异
// 步提交方法(Connect、Click、Screencap 与仅 Windows 生效的
// AttachWindow,返回 *Operation)与图像/UUID/任务列表读取
// (GetImagePNG、GetImageBGR、UUID、Tasks)。
type Instance struct {
	// inst 是私有单次关闭组件:持有共享状态、事件分发器、
	// callback token 与 native 停止/销毁接缝,Close 委托它的唯一流程。
	inst *instance
	// rt 是创建本实例的 Runtime:实例关闭后经它的生命周期门进入终止
	// 使用状态(一个实例关闭后 Runtime 进入终止态);limits 快照
	// 供同步方法的输入预算校验使用。
	rt *Runtime
	// caller 是同步业务调用的 native 接缝:生产接线转发
	// Runtime 的 API 表,受控用例注入 fake;公开方法先经实例状态准入
	// 再触达它,handle 只在准入 lease 与 raw 门禁段内读取。
	caller instanceCaller
}

// Close 同步销毁实例:幂等,返回稳定的最终错误(正常关闭为 nil)。
// 它是单次关闭流程的公开入口(幂等、同步销毁;返回后不再允许
// 原生调用;:并发 Close 共享同一 closeDone 和最终错误,只有一个
// 关闭所有者执行 Stop/Destroy):多次或并发调用只有一个所有者执行一
// 次 Stop、一次 Destroy,其余申请者等待同一关闭结果并取得同一最终
// 错误;Close 不接受强制回收超时,Stop/Destroy 卡住时由 control 对
// worker 执行有期限的退出监督。
//
// 实例关闭完成(包括关闭失败返回错误)后,创建它的 Runtime 进入终
// 止使用状态(一个实例关闭后 Runtime 进入终止态,不再允许
// NewInstance/LoadResource):后续 Open 与 NewInstance 一律返回包装
// ErrRuntimeTerminated 的错误;不在同进程重建实例。
func (in *Instance) Close() error {
	err := in.inst.close()
	// 关闭错误保留首个故障及停止诊断,不从 Destroy 的 void 返回
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
	// 创建者调用一次。
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

// instanceCaller 是同步业务调用(任务、请求与状态查询)的
// native 注入边界(对照 instanceCloser 先例):生产实现转发 calls.go
// 的 raw 调用包装,fake 实现供受控用例记录原始参数、调用次数与注入
// 返回值,不替换生产包的全局函数变量。与关闭接缝分开定义:关闭流程
// 的 instanceCloser 刻意不暴露业务符号(关闭所有者无条件执行
// 一次 Stop,不先查询 Running),业务路径也不经关闭接缝触达原生;
// 业务 Stop 与关闭 Stop 各有本接口与 instanceCloser 的独立方法,转
// 发同一 raw 包装但准入纪律不同。方法只接收 handle 参数并原样转发:
// handle 的唯一来源是已准入 callLease.handle(),本接口不持有、不缓
// 存任何 handle(不能另存一份裸 handle 绕过状态检查)。
type instanceCaller interface {
	// appendTask 对应 AsstAppendTask(handle, taskType, params):返回
	// 0(InvalidId)表示提交失败。调用契约:仅在调用 lease 已准入、
	// raw 串行门禁已进入后调用一次。
	appendTask(handle uintptr, taskType *byte, params *byte) int32
	// setTaskParams 对应 AsstSetTaskParams(handle, id, params):返回
	// 0 表示原生拒绝。调用契约同 appendTask。
	setTaskParams(handle uintptr, id int32, params *byte) uint8
	// start 对应 AsstStart(handle):返回 0 表示原生拒绝。
	start(handle uintptr) uint8
	// stop 对应 AsstStop(handle):业务语义的停止请求,返回 0 表示原
	// 生拒绝;与 instanceCloser.stop(关闭所有者的销毁前置停止)转发
	// 同一 raw 包装,准入纪律互不相同。
	stop(handle uintptr) uint8
	// backToHome 对应 AsstBackToHome(handle):返回 0 表示原生拒绝。
	backToHome(handle uintptr) uint8
	// running 对应 AsstRunning(handle):返回 0(false)是正常查询
	// 结果,不是错误。
	running(handle uintptr) uint8
	// connected 对应 AsstConnected(handle):返回 0(false)是正常查
	// 询结果,不是错误。
	connected(handle uintptr) uint8
	// asyncConnect 对应 AsstAsyncConnect(handle, adbPath, address,
	// config, block):异步提交连接,block 由公开层固定传 0;返回
	// 0(InvalidId)表示提交失败,非零是关联键 async ID。调用契约:
	// 仅在调用 lease 已准入、raw 串行门禁已进入、早到结果窗口已打开
	// 后调用一次。
	asyncConnect(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32
	// asyncClick 对应 AsstAsyncClick(handle, x, y, block):异步提交
	// 点击,契约同 asyncConnect。
	asyncClick(handle uintptr, x int32, y int32, block uint8) int32
	// asyncScreencap 对应 AsstAsyncScreencap(handle, block):异步提
	// 交截图,契约同 asyncConnect。
	asyncScreencap(handle uintptr, block uint8) int32
	// asyncAttachWindow 对应 AsstAsyncAttachWindow(handle, hwnd,
	// screencapMethod, mouseMethod, keyboardMethod, block):异步提交
	// Win32 窗口绑定,契约同 asyncConnect;符号仅 Windows 参与查找,
	// 调用方必须先经平台与能力门判定。调用契约同 asyncConnect。
	asyncAttachWindow(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32
	// getImage 对应 AsstGetImage(handle, buff, buffSize):读取 Core
	// 缓存图像的 PNG 编码,容量与返回值均为字节数。buff 指向实际分配
	// 的无 Go 指针字节缓冲区(不以 nil/0 查询所需容量),由调
	// 用方保活至 raw 返回。调用契约:仅在调用 lease 已准入、raw 串行
	// 门禁已进入后调用(扩容重试的多次调用同属一次准入)。
	getImage(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	// getImageBgr 对应 AsstGetImageBgr(handle, buff, buffSize):读取
	// Core 缓存图像的 BGR 像素数据,契约同 getImage;符号是可选项,
	// 调用方必须先经能力门判定。调用契约同 getImage。
	getImageBgr(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	// getUUID 对应 AsstGetUUID(handle, buff, buffSize):读取设备
	// UUID,容量与返回值均为字节数,按返回长度读取、不依赖尾部 NUL。
	// 调用契约同 getImage。
	getUUID(handle uintptr, buff *byte, buffSize uint64) uint64
	// getTasksList 对应 AsstGetTasksList(handle, buff, buffSize):读
	// 取任务 ID 列表,容量与返回值均为 int32 元素数(不是字节数)。
	// 调用契约同 getImage。
	getTasksList(handle uintptr, buff *int32, buffSize uint64) uint64
}

// apiTableCaller 是 instanceCaller 的生产实现:转发 *native.APITable
// 的同步业务 raw 调用包装,不解释返回值、不重试、不编造诊断。
type apiTableCaller struct {
	table *native.APITable
}

func (c apiTableCaller) appendTask(handle uintptr, taskType *byte, params *byte) int32 {
	return c.table.AppendTask(handle, taskType, params)
}

func (c apiTableCaller) setTaskParams(handle uintptr, id int32, params *byte) uint8 {
	return c.table.SetTaskParams(handle, id, params)
}

func (c apiTableCaller) start(handle uintptr) uint8 { return c.table.Start(handle) }

func (c apiTableCaller) stop(handle uintptr) uint8 { return c.table.Stop(handle) }

func (c apiTableCaller) backToHome(handle uintptr) uint8 { return c.table.BackToHome(handle) }

func (c apiTableCaller) running(handle uintptr) uint8 { return c.table.Running(handle) }

func (c apiTableCaller) connected(handle uintptr) uint8 { return c.table.Connected(handle) }

func (c apiTableCaller) asyncConnect(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32 {
	return c.table.AsyncConnect(handle, adbPath, address, config, block)
}

func (c apiTableCaller) asyncClick(handle uintptr, x int32, y int32, block uint8) int32 {
	return c.table.AsyncClick(handle, x, y, block)
}

func (c apiTableCaller) asyncScreencap(handle uintptr, block uint8) int32 {
	return c.table.AsyncScreencap(handle, block)
}

func (c apiTableCaller) asyncAttachWindow(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32 {
	return c.table.AsyncAttachWindow(handle, hwnd, screencapMethod, mouseMethod, keyboardMethod, block)
}

func (c apiTableCaller) getImage(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
	return c.table.GetImage(handle, buff, buffSize)
}

func (c apiTableCaller) getImageBgr(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
	return c.table.GetImageBgr(handle, buff, buffSize)
}

func (c apiTableCaller) getUUID(handle uintptr, buff *byte, buffSize uint64) uint64 {
	return c.table.GetUUID(handle, buff, buffSize)
}

func (c apiTableCaller) getTasksList(handle uintptr, buff *int32, buffSize uint64) uint64 {
	return c.table.GetTasksList(handle, buff, buffSize)
}

// createInstance 是 NewInstance 的完整创建事务(初始化第 2 阶段),
// 执行顺序严格按:
//
//  1. 纯 Go 配置校验与深拷贝快照(在预留名额前复制;失败不消
//     耗 Runtime,发生在触碰生命周期门之前);
//  2. 预留唯一实例名额并登记创建 lease(Runtime 状态门内);
//  3. 建立实例共享状态、事件入口与 callback 准入接缝,登记 token
//     (注册表、事件队列先于 CreateEx 建立,创建期间的早到回调
//     也能找到实例状态,不遗失状态);
//  4. CreateEx(callback=进程唯一 trampoline,customArg=token);返回
//     0 走统一回滚(零 handle:不调用无效 handle,不执行 Stop/Destroy);
//  5. 按序应用实例选项,任何一次原生拒绝走统一回滚;
//  6. 在 Runtime 同一状态门内复核可发布性并原子复核早到故障:已
//     closing 或已有早到故障都不发布,由创建者完成统一回滚;
//  7. 全部成功才发布 ready 并返回 *Instance,释放创建 lease。
//
// 预占名额后的任何失败(预占名额后的创建失败使 Runtime 终止使
// 用)都经 rollbackCreation 收尾:记录创建失败、复用单次关闭流程清
// 理、终止 Runtime 使用、释放创建 lease;调用者只拿到错误,拿不到
// 半初始化实例。锁序:Runtime 状态门(lifecycleGate.mu)→ 实例状态
// 锁(publishInstance 内),无反向获取。
func createInstance(rt *Runtime, cfg InstanceConfig, creator instanceCreator, closer instanceCloser, caller instanceCaller) (*Instance, error) {
	if creator == nil || closer == nil || caller == nil {
		// 三个 native 接缝都不可缺席:缺席即接线层编程错误,在触碰生
		// 命周期门之前 panic(对照 newInstance 的先例)。
		panic("maa: instance created without complete native seams (creator/closer/caller)")
	}
	// 1. 预占前:纯 Go 校验 + 深拷贝快照(预算取 Open 时已校验冻结的
	//    Limits 快照,InstanceConfig 不覆盖 Runtime 的限制)。
	snap, err := snapshotInstanceConfig(cfg, rt.limits)
	if err != nil {
		return nil, err
	}
	// 2. 预留唯一实例名额并登记创建 lease(NewInstance 第 2 步):
	//    已有实例、Runtime 已 closing 或已终止使用都在此拒绝。
	if err := rt.gate.reserveInstance(); err != nil {
		return nil, err
	}
	// 以下任何失败都必须经 rollbackCreation 收尾,不允许半途返回。

	// 3. 建立实例共享状态与事件入口(handle=0:CreateEx 返回前尚未
	//    取得句柄),启动分发循环,构造 callback 准入接缝并登记 token
	//    (NewInstance 第 3 步;callback 门从构造起开放,早到消息
	//    照常准入入队)。
	s := newInstanceState(0, rt.limits.PendingOperations, rt.limits.EarlyBytes)
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

	// 4. CreateEx(NewInstance 第 4 步):callback 与 token 已可用。
	handle := creator.createEx(native.CallbackTrampoline(), token)
	if handle == 0 {
		// 零 handle(CreateEx 返回 0 时清理临时 token 和 Go 状态):
		// 回滚不调用无效 handle,也不执行 Stop/Destroy()。
		return nil, rollbackCreation(rt, s, d, token, closer,
			fmt.Errorf("maa: AsstCreateEx returned 0 (null handle): %w", native.ErrNativeRejected))
	}
	// 登记句柄:早到故障可能已先进入 faulted,回滚仍需要句柄销毁。
	s.publishHandle(handle)

	// 5. 按序应用实例选项(有序 Options;option value 允许空串,
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

	// 6. Runtime 同一状态门内复核可发布性并原子复核早到故障(创
	//    建者在 Runtime 的同一状态门内决定发布):已 closing 返回
	//    ErrClosing,已有早到故障返回首因;两者都不发布,由创建者完
	//    成统一回滚后释放 lease,Runtime.Close 等待该流程。
	if err := rt.gate.publishInstance(in); err != nil {
		return nil, rollbackCreation(rt, s, d, token, closer, err)
	}
	// 7. 发布成功:释放创建 lease(Runtime.Close 等待该流程,不重
	//    复 Destroy)并返回公开实例——至此全部步骤成功。
	rt.gate.releaseCreationLease()
	return &Instance{inst: in, rt: rt, caller: caller}, nil
}

// rollbackCreation 是创建事务的统一失败回滚入口(CreateEx 返回 0
// 时清理临时 token 和 Go 状态;已创建 handle 后失败则执行同一
// Stop/Destroy 流程;预占名额后的创建失败使 Runtime 终止使用):
//
//  1. 以 cause 记录创建失败故障(sticky:早到故障已是首因时保留首因,
//     本 cause 不覆盖,阶段转入/保持 faulted);
//  2. token 已登记时复用单次关闭流程(electClose 由创建者胜
//     出——尚无公开实例,不存在其他关闭者):handle 非零执行一次
//     Stop、一次 Destroy,零 handle 跳过;关门、等待退空、移除 token、
//     排空结算、发布终态全部照常;token 登记失败(token=0)时由创建
//     者代行唯一关闭所有者走同一 closeTail 尾段,无 token 可移除;
//  3. 终止 Runtime 使用(后续 Open/NewInstance 返回
//     ErrRuntimeTerminated,附首个失败原因);
//  4. 释放创建 lease(回滚流程完成后释放,Runtime.Close 的等待
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
			err = closeTail(s, d, 0)
		}
	}
	rt.gate.terminate(err)
	rt.gate.releaseCreationLease()
	return err
}

// ---- 同步业务调用----
//
// 公开同步入口的公共纪律(调用纪律、 输入限制):
//   - 纯 Go 输入校验与快照先行(对照 createInstance 的预占前校验先
//     例):发生在触碰生命周期门与原生调用之前,失败返回带字段路径
//     的 ErrInvalidInput,不进入 native、不占在途计数、不占 pending;
//   - 随后 syncCall 先取得调用 lease、再进入独立的 raw 串行门禁:
//     closing/faulted/closed 在准入处拒绝(错误身份见 admitCall),
//     准入后的调用覆盖排队、native 调用与输出复制全过程;任何
//     native 调用都不持有状态锁;
//   - handle 只在 lease 与门禁段内经 callLease.handle 读取,立即用
//     于本次调用,不另存裸 handle();
//   - 全部实例原生入口经过同一 lease 与同一 raw 串行门禁,无绕过状
//     态的第二通道;instanceCaller 接缝不持有 handle。
//
// 返回值语义基线:C bool=false 的请求类入口(Start/Stop/
// BackToHome/SetTaskParams)包装 ErrNativeRejected——通常无结构化原
// 因,不编造 LastError;接受请求只证明 Core 收下请求,一律不赋予任
// 务开始/完成语义,任务进度经事件流观察。查询类入口(Running/
// Connected)的 false 是正常查询结果,不转换成错误;生命周期失败单
// 独经错误返回。C void 入口(Log,Runtime 全局)只有输入与生命周
// 期错误,不伪造原生成功确认。

// syncCall 执行一次同步业务调用的公共骨架:先取得调用 lease,再等
// 待独立的 raw 串行门禁,在门禁段内读取 handle 并执行 call()。
// defer 顺序保证先退 raw 门禁、后释放在途计数;call 执行期间不持有
// 状态锁。call 收到的 handle 只在本次调用内有效,不得外泄或缓存。
func (in *Instance) syncCall(what string, call func(handle uintptr) error) error {
	lease, err := in.inst.state.admitCall(what)
	if err != nil {
		return err
	}
	defer lease.release()
	in.inst.state.raw.enter()
	defer in.inst.state.raw.exit()
	return call(lease.handle())
}

// boolCall 是「C bool 返回的实例操作请求」的公共解释(Start/Stop/
// BackToHome):nil 只表示请求被 Core 接受(C bool=true),绝不赋予
// 任务开始/停止/完成等执行语义(不伪造任务完成语义);返回 0
// 表示原生拒绝,包装 ErrNativeRejected,无结构化原因,不编造
// LastError。
func (in *Instance) boolCall(what string, call func(handle uintptr) uint8) error {
	return in.syncCall(what, func(handle uintptr) error {
		if call(handle) == 0 {
			return fmt.Errorf("maa: Asst%s returned 0 (false), request rejected: %w", what, native.ErrNativeRejected)
		}
		return nil
	})
}

// queryCall 是「C bool 返回的实例状态查询」的公共解释(Running/
// Connected):false 是正常查询结果,返回 (false, nil),不转换成错
// 误(生命周期错误单独返回,正常 false 不转换成错误)。
func (in *Instance) queryCall(what string, call func(handle uintptr) uint8) (bool, error) {
	var ret uint8
	err := in.syncCall(what, func(handle uintptr) error {
		ret = call(handle)
		return nil
	})
	return ret != 0, err
}

// AppendTask 向实例任务队列追加一个任务:taskType 原样透传,不做业
// 务枚举校验——未知任务类型保留,以免 binding 阻碍 Core 功能演进
// params 必须是非空、完整的 JSON object 原文,未知字
// 段与超过 2^53 的整数按原文字节透传,不经 map[string]any 往返、不
// 丢弃字段。
//
// 返回的 TaskID 非 0 只证明任务已被 Core 接受入队——接受请求不等
// 于任务已开始或执行完成,任务进度经事件流观察(入队不等于完
// 成;本方法不做任务完成推断)。返回 (0, err) 且错误包装
// ErrNativeRejected 表示提交被原生拒绝(TaskID 0 明确表示提交失败);
// 输入校验失败返回包装 ErrInvalidInput 的错误,不进入 native;实例
// closing/faulted/closed 的拒绝经错误链可辨(ErrClosing/首因/
// ErrClosed)。
func (in *Instance) AppendTask(taskType string, params json.RawMessage) (TaskID, error) {
	// 纯 Go 校验与快照先行:taskType 非空、合法 UTF-8、无内嵌 NUL,
	// params 为完整 JSON object,合计不超预算;快照为逐字节独立副本,
	// 方法返回后调用者可原地改写原 RawMessage 而不影响提交。
	snap, err := snapshotTaskInput(taskType, params, in.rt.limits)
	if err != nil {
		return 0, err
	}
	var id int32
	err = in.syncCall("AppendTask", func(handle uintptr) error {
		t, tBuf := cString(snap.taskType)
		p, pBuf := cString(string(snap.params))
		id = in.caller.appendTask(handle, t, p)
		// 带 NUL 的 C buffer 持有至 raw 返回(调用期保活)。
		runtime.KeepAlive(tBuf)
		runtime.KeepAlive(pBuf)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if id == 0 {
		return 0, fmt.Errorf("maa: AsstAppendTask returned 0, task submission rejected: %w", native.ErrNativeRejected)
	}
	return TaskID(id), nil
}

// SetTaskParams 更新已提交任务的原始 JSON 参数:params 必须是非空、
// 完整的 JSON object 原文,与任务 params 同一校验语义,未知
// 字段按原文字节透传;id 原样透传(保留 int32 符号位),不存在或已
// 结束的任务由 Core 原生拒绝。
//
// 返回 nil 只表示修改请求被 Core 接受——接受请求不等于任务执行成
// 功,也不等于参数已按新值生效于进行中的步骤;任务进度经事件流观
// 察(修改请求被 Core 接受不代表任务执行成功)。返回包装
// ErrNativeRejected 的错误表示请求被原生拒绝;输入与生命周期错误
// 语义同 AppendTask。
func (in *Instance) SetTaskParams(id TaskID, params json.RawMessage) error {
	if err := validateJSONObject("params", params, in.rt.limits.InputJSONBytes); err != nil {
		return err
	}
	// 独立快照:方法返回后调用者可原地改写原 RawMessage 而不影响提交
	// (cString 编码前的这份 clone 与任务输入快照同一纪律)。
	snap := bytes.Clone(params)
	return in.syncCall("SetTaskParams", func(handle uintptr) error {
		p, pBuf := cString(string(snap))
		ret := in.caller.setTaskParams(handle, int32(id), p)
		runtime.KeepAlive(pBuf)
		if ret == 0 {
			return fmt.Errorf("maa: AsstSetTaskParams returned 0 (false), param update rejected: %w", native.ErrNativeRejected)
		}
		return nil
	})
}

// Start 请求启动实例的任务队列。返回 nil 只表示请求被 Core 接受
// (C bool=true)——不表示任务队列已开始执行,更不表示任何任务执行
// 成功;任务进度经事件流观察。返回包装 ErrNativeRejected 的错误表示
// 请求被原生拒绝(C bool=false,无结构化原因)。与 Close 不同,本
// 方法被拒绝时只向本次调用返回拒绝:不自动 Destroy、不撤销异步队
// 列(普通公开 Stop 返回 false 只向本次调用返回拒绝)。
func (in *Instance) Start() error {
	return in.boolCall("Start", in.caller.start)
}

// Stop 请求停止实例的全部任务:停止请求作用于任务队列,不是按
// async ID 取消单个异步操作(C API 无按 async ID 取消接口)。
// 返回 nil 只表示停止请求被 Core 接受——不表示任务已停止或队列已
// 排空,停止进度经事件流观察;返回包装 ErrNativeRejected 的错误表示
// 请求被原生拒绝:不自动 Destroy、不撤销异步队列。输入与生
// 命周期错误语义同 AppendTask。
func (in *Instance) Stop() error {
	return in.boolCall("Stop", in.caller.stop)
}

// BackToHome 请求实例回到主界面。返回 nil 只表示请求被 Core 接受,
// 不表示界面动作已完成;返回包装 ErrNativeRejected 的错误表示请求被
// 原生拒绝。输入与生命周期错误语义同 AppendTask。
func (in *Instance) BackToHome() error {
	return in.boolCall("BackToHome", in.caller.backToHome)
}

// Running 查询实例的任务队列是否正在执行:false 是正常查询结果,
// 返回 (false, nil),不转换成错误(AsstRunning 的 false 是正常
// 查询结果;:正常 false 不转换成错误)。生命周期失败(closing/
// faulted/closed 的准入拒绝)单独经错误返回,与正常 false 可辨;
// 错误包装 ErrClosing、实例首因或 ErrClosed。
func (in *Instance) Running() (bool, error) {
	return in.queryCall("Running", in.caller.running)
}

// Connected 查询实例是否已连接设备:false 是正常查询结果,返回
// (false, nil),不转换成错误;生命周期失败单独经错误返回,语义同
// Running。
func (in *Instance) Connected() (bool, error) {
	return in.queryCall("Connected", in.caller.connected)
}

// ---- 异步提交与结果关联----
//
// 四个异步公开入口的公共纪律(提交纪律、 输入限制):
//   - 纯 Go 输入校验与快照先行(对照同步入口先例):失败返回
//     ErrInvalidInput,不触碰生命周期门、不占在途计数、不占 pending;
//     无输入的 Click/Screencap 与按能力门判定后无校验输入的
//     AttachWindow 直接进入准入;
//   - 随后 admitCall 取得调用 lease(生命周期准入,覆盖排队、native
//     调用与结果接合全过程),再在 raw 串行门禁之前预留 pending 容量
//     并登记未绑定 ID 的请求 Operation(pending 在调用进入 raw
//     串行门禁前预留,满时直接拒绝且不调用 native);
//   - 进入 raw 串行门禁后、native 调用前打开早到结果窗口(仅
//     在「已进入 raw、尚未接合返回 ID」窗口内允许临存一个未知 ID 的
//     完成结果,EarlyResults 固定为 1),随后以 block=false 调用
//     native(公共异步方法统一用 block=false,避免把 block 的完成
//     标记当成成功);
//   - raw 返回后在调用 lease 内完成返回 ID 校验与登记
//     (finishAsyncRaw):ID 0 是明确的提交错误并回收预留;负值/复用
//     /倒退为 ErrProtocol 故障;登记后按 token、ID、预期 what 接合早
//     到结果,统一 dispatcher 结算晚到结果并回收 pending 引用;
//   - 任何 native 调用都不持有状态锁;handle 只在 lease 与门禁段内
//     读取。
//
// 预期 what 冻结自锁定 Core 的 Assistant::call_proc(Assistant.cpp):
// 异步项执行后以 AsyncCallInfo(message=4)回报 what=Connect/Click/
// Screencap/AttachWindow、async_call_id 与 details.ret;早到接合按该
// 预期校验,不符为 ErrProtocol。

// asyncBlockFalse 是三个异步 raw 入口统一使用的 block 取值(冻结:
// 公共异步方法统一用 block=false)。
const asyncBlockFalse = uint8(0)

// asyncWhatAttachWindow 等四个异步入口的预期完成 what(raw 返回后按
// token、ID、预期 what 接合;值冻结自 Assistant.cpp call_proc)。
const (
	asyncWhatConnect      = "Connect"
	asyncWhatClick        = "Click"
	asyncWhatScreencap    = "Screencap"
	asyncWhatAttachWindow = "AttachWindow"
)

// asyncCall 执行一次异步提交的公共骨架:生命周期准入 → pending 预留
// → raw 门禁段内(高水位耗尽检查 → 开窗 → block=false native 调用
// → 返回 ID 校验、登记与早到接合)。提交被接受时返回包装已登记记录
// 的公开 *Operation(可能已由早到结果结算);被拒时回收预留、作废
// 记录并返回错误,公开面拿不到半成品 Operation。
func (in *Instance) asyncCall(what string, call func(handle uintptr) int32) (*Operation, error) {
	lease, err := in.inst.state.admitCall(what)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	// pending 预留(raw 门禁前;):满时直接拒绝,不调用 native,
	// 实例不因此故障。预留与登记/释放一一对应,以下任何返回路径都
	// 已在状态锁内回收预留或转为登记。
	op, err := in.inst.state.reservePendingOperation(what)
	if err != nil {
		return nil, err
	}
	in.inst.state.raw.enter()
	_, joinErr := func() (int32, error) {
		defer in.inst.state.raw.exit()
		// 高水位耗尽检查 + 打开早到窗口(门禁内、进入 raw 前)。
		if err := in.inst.state.beginAsyncRaw(); err != nil {
			return 0, err
		}
		id := call(lease.handle())
		// 返回 ID 校验、登记与早到接合:失败路径在状态锁内
		// 回收预留并作废记录。
		return id, in.inst.state.finishAsyncRaw(op, what, id)
	}()
	if joinErr != nil {
		return nil, joinErr
	}
	return &Operation{op: op}, nil
}

// Connect 异步提交设备连接(AsstAsyncConnect,block=false):返回的
// *Operation 持有关联键 async ID,连接的最终结果(成功与否、耗时等
// details)由 AsyncCallInfo 按该 ID 关联确认后经 Wait 取得——非零 ID
// 只表示提交被接受,不是连接已建立的证明。ADBPath/Address/
// Config 的输入校验与快照语义同同步入口(路径非空绝对、
// Address 非空、Config 允许空串并原样传空 C 字符串);快照只持有到
// raw 返回(锁定 Core 在入队前复制连接参数,Assistant.cpp
// async_connect),方法返回后即可修改原输入。
//
// 错误语义:输入校验失败包装 ErrInvalidInput 且不进入 native;容量
// 满包装 ErrOperationLimit(不调用 native,实例不故障);生命周期
// 拒绝经错误链可辨(ErrClosing/首因/ErrClosed);raw 返回 0 包装
// ErrNativeRejected(明确的提交失败,已回收预留);ID 复用/倒退或
// 与早到结果矛盾等协议破坏包装 ErrProtocol 并使实例故障。
func (in *Instance) Connect(cfg ConnectConfig) (*Operation, error) {
	snap, err := snapshotConnectInput(cfg, in.rt.limits)
	if err != nil {
		return nil, err
	}
	return in.asyncCall(asyncWhatConnect, func(handle uintptr) int32 {
		adb, adbBuf := cString(snap.adbPath)
		addr, addrBuf := cString(snap.address)
		conf, confBuf := cString(snap.config)
		id := in.caller.asyncConnect(handle, adb, addr, conf, asyncBlockFalse)
		// 带 NUL 的 C buffer 持有至 raw 返回(调用期保活;连接参
		// 数由 Core 在入队前复制,无需存活到完成)。
		runtime.KeepAlive(adbBuf)
		runtime.KeepAlive(addrBuf)
		runtime.KeepAlive(confBuf)
		return id
	})
}

// Click 异步提交一次点击(AsstAsyncClick,block=false):x/y 带符号
// 原样透传,返回的 *Operation 语义同 Connect——最终结果经 Wait 取得,
// 非零 ID 只表示提交被接受。错误语义同 Connect(无输入校验
// 步骤)。
func (in *Instance) Click(x, y int32) (*Operation, error) {
	return in.asyncCall(asyncWhatClick, func(handle uintptr) int32 {
		return in.caller.asyncClick(handle, x, y, asyncBlockFalse)
	})
}

// Screencap 异步提交一次截图(AsstAsyncScreencap,block=false):返
// 回的 *Operation 语义同 Connect——提交被接受只表示采集请求已入队,
// 结果经 Wait 确认后另以图像读取接口取缓存图像(获取新截图需先
// 等待 Screencap 的成功结果,再取 PNG;读取接口见 GetImagePNG)。
// 错误语义同 Connect。
func (in *Instance) Screencap() (*Operation, error) {
	return in.asyncCall(asyncWhatScreencap, func(handle uintptr) int32 {
		return in.caller.asyncScreencap(handle, asyncBlockFalse)
	})
}

// ---- 图像、UUID 与任务列表读取 ----
//
// 四个输出读取公开入口(GetImagePNG/GetImageBGR/UUID/Tasks)的公共纪律:
//   - 与同步业务调用同一准入:先 admitCall 取得调用 lease,再进入独
//     立的 raw 串行门禁;lease 覆盖探测 buffer 分配、raw 调用(含扩
//     容重试的多次调用)、长度校验、内容校验与最终复制全过程(
//     任务执行步骤 1)。任何 native 调用不持状态锁;handle 只在
//     lease 与门禁段内读取。
//   - 复用有界缓冲区辅助层(readOutputBytes/readOutputInt32s):
//     每次探测分配实际非空的 Go buffer 并传真实容量,不以 nil/0 查
//     询所需容量;返回值校验顺序固定 NullSize → 实际容量 → Go
//     MaxInt → 转换/切片。哨兵值取 Open 时核验并保存的
//     Runtime.nullSize,不硬编码。
//   - 按接口单位计量:PNG/BGR 与 UUID 按字节,Tasks 按 int32 元素数
//     (每元素 4 字节只用于分配前的 Go int 守卫,容量与返回值都不当
//     作字节数)。
//   - raw 接线方必须保活本层分配的 buffer:runtime.KeepAlive 持有至
//     raw 返回(buffer.go 契约:保活责任在 raw 接线方)。
//   - 返回独立 Go 快照:探测 buffer 不外泄,成功结果复制为
//     调用者可长期持有、修改的独立数据;返回后原生内容改写或调用者
//     修改切片都不影响已取得的快照与后续结果。
//   - 可选 BGR(AsstGetImageBgr)与能力门:符号是可选项,缺失时
//     GetImageBGR 在准入前直接返回 ErrUnsupported(不进入 native、
//     不占在途计数、不分配 BGR buffer);在场时按 Limits.ImageBGR
//     走同一探测契约。不存在可绕过能力判断的公开 BGR 读取路径。
//
// 四个接口都不是 nil/0 查询所需容量的协议:未初始化、空
// handle、容量不足均可能返回 NullSize;返回值大于实际容量是
// ErrProtocol,立即终止、不扩容;合法 0 在实际非空 buffer 上返回空
// 结果。耗尽返回可辨识的 ErrBufferLimit(诊断含接口、单位、尝试次
// 数、最后容量、Max 与原始返回值,NullSize 原因不明)。

// 读取接口名常量:四个读取接口在缓冲区探测诊断中的接口名(对应 C
// 符号去掉 Asst 前缀,与 buffer_test.go 的用例命名一致)。
const (
	outputNameImage    = "GetImage"
	outputNameImageBGR = "GetImageBgr"
	outputNameUUID     = "GetUUID"
	outputNameTasks    = "GetTasksList"
)

// GetImagePNG 读取 Core 缓存图像的 PNG 编码(AsstGetImage):返回的
// 是最近一次采集图像的缓存结果——本方法不触发任何隐式截图,也不承
// 诺缓存采集时间(PNG 本身不带采集时间,任务执行期间缓存仍可能更
// 新,扩容重试也不固定同一张图像);需要新截图时,调用方必须先提交
// Screencap 并等待其成功结果,再调用本方法读取。未连接设备时
// 也可能合法返回 0 长度(空、非 nil 切片),binding 不将其伪造成截
// 图成功或失败——截图进度只能经 Screencap 的 Operation 与事件流观
// 察。返回独立 Go 快照,可长期持有、修改。
//
// 容量探测按 Limits.ImagePNG(字节)执行,返回错误语义:探测耗尽
// 包装 ErrBufferLimit;原生返回值大于实际容量包装 ErrProtocol;生命
// 周期拒绝(closing/faulted/closed)经错误链可辨。
func (in *Instance) GetImagePNG() ([]byte, error) {
	var png []byte
	err := in.syncCall("GetImagePNG", func(handle uintptr) error {
		var err error
		png, err = readOutputBytes(outputNameImage, func(buf []byte) uint64 {
			ret := in.caller.getImage(handle, unsafe.Pointer(&buf[0]), uint64(len(buf)))
			// 本层分配的探测 buffer 持有至 raw 返回(保活责任在
			// raw 接线方);PNG/BGR/UUID/Tasks 的扩容重试同属一次 raw
			// 门禁段,每次调用各自保活。
			runtime.KeepAlive(buf)
			return ret
		}, in.rt.nullSize, in.rt.limits.ImagePNG)
		return err
	})
	if err != nil {
		return nil, err
	}
	return png, nil
}

// GetImageBGR 读取 Core 缓存图像的 BGR 像素数据(AsstGetImageBgr,
// 可选能力):缓存语义与 GetImagePNG 完全一致——读取的是最近一次采
// 集图像的缓存结果,不触发任何隐式截图,也不承诺缓存采集时间;需要
// 新截图时先提交 Screencap 并等待其成功结果再读取。返回值是原始
// BGR 字节(通道序与步长由 Core 决定,binding 不解释,也不返回图
// 像尺寸),独立 Go 快照,可长期持有、修改。
//
// 能力门:符号未随本次加载的原生库导出时,在准入前直接返回包装
// ErrUnsupported 的错误(不进入 native、不占在途计数、不分配 BGR
// buffer)。在场时容量探测按 Limits.ImageBGR(字节)执行,错误语义
// 同 GetImagePNG。
func (in *Instance) GetImageBGR() ([]byte, error) {
	if !in.rt.caps.ImageBgr() {
		return nil, fmt.Errorf("maa: AsstGetImageBgr is not available in this capability set: %w", native.ErrUnsupported)
	}
	var bgr []byte
	err := in.syncCall("GetImageBGR", func(handle uintptr) error {
		var err error
		bgr, err = readOutputBytes(outputNameImageBGR, func(buf []byte) uint64 {
			ret := in.caller.getImageBgr(handle, unsafe.Pointer(&buf[0]), uint64(len(buf)))
			runtime.KeepAlive(buf)
			return ret
		}, in.rt.nullSize, in.rt.limits.ImageBGR)
		return err
	})
	if err != nil {
		return nil, err
	}
	return bgr, nil
}

// UUID 读取设备 UUID(AsstGetUUID):按原生返回的长度读取并转换为
// string——不寻找、不要求尾部 NUL,缓冲区容量内的其余内容一律丢弃,
// 因此无尾部 NUL 仍能保留完整有效内容(C 契约:AsstGetUUID
// 不补 NUL)。转换前校验内容:含 NUL 字节或非法 UTF-8 为协议破坏,
// 返回包装 ErrProtocol 的错误,不把矛盾证据伪装成成功。合法 0 长度
// 返回空串。返回独立 Go 快照(string 按语言语义共享)。
//
// 容量探测按 Limits.UUID(字节)执行,错误语义同 GetImagePNG。
func (in *Instance) UUID() (string, error) {
	var uuid string
	err := in.syncCall("UUID", func(handle uintptr) error {
		raw, err := readOutputBytes(outputNameUUID, func(buf []byte) uint64 {
			ret := in.caller.getUUID(handle, &buf[0], uint64(len(buf)))
			runtime.KeepAlive(buf)
			return ret
		}, in.rt.nullSize, in.rt.limits.UUID)
		if err != nil {
			return err
		}
		// 内容校验:只校验按返回长度读取的有效区间,不寻找
		// 尾部 NUL;内容含 NUL 或非法 UTF-8 为 ErrProtocol。
		if bytes.IndexByte(raw, 0x00) >= 0 {
			return fmt.Errorf("maa: %s content contains a NUL byte: %w", outputNameUUID, native.ErrProtocol)
		}
		if !utf8.Valid(raw) {
			return fmt.Errorf("maa: %s content is not valid UTF-8: %w", outputNameUUID, native.ErrProtocol)
		}
		uuid = string(raw)
		return nil
	})
	if err != nil {
		return "", err
	}
	return uuid, nil
}

// Tasks 读取实例任务列表(AsstGetTasksList):返回已入队任务的 ID
// 列表——入队中的任务(含尚未 Start 的)也会出现,列表内容与元素
// 顺序由 Core 决定,binding 不解释任务状态。容量与返回值均按 int32
// 元素数处理、不当作字节数;id 保留 int32 符号位转换为
// TaskID。合法 0 长度返回空、非 nil 切片。返回独立 Go 快照,可长期
// 持有、修改。
//
// 容量探测按 Limits.Tasks(元素)执行,错误语义同 GetImagePNG。
func (in *Instance) Tasks() ([]TaskID, error) {
	var tasks []TaskID
	err := in.syncCall("Tasks", func(handle uintptr) error {
		raw, err := readOutputInt32s(outputNameTasks, func(buf []int32) uint64 {
			ret := in.caller.getTasksList(handle, &buf[0], uint64(len(buf)))
			runtime.KeepAlive(buf)
			return ret
		}, in.rt.nullSize, in.rt.limits.Tasks)
		if err != nil {
			return err
		}
		tasks = make([]TaskID, len(raw))
		for i, id := range raw {
			tasks[i] = TaskID(id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// runtimeLogger 是 Runtime 全局日志(AsstLog:进程级,无 handle)的
// native 注入边界(对照 instanceCaller 先例):生产实现转发 calls.go
// 的 Log 包装,fake 实现供受控用例记录原始参数与调用次数。
type runtimeLogger interface {
	// log 对应 AsstLog(level, message):C void 函数,没有成功码。
	log(level *byte, message *byte)
}

// apiTableLogger 是 runtimeLogger 的生产实现:转发 *native.APITable
// 的 Log raw 调用包装,不解释、不编造成功确认。
type apiTableLogger struct {
	table *native.APITable
}

func (l apiTableLogger) log(level *byte, message *byte) { l.table.Log(level, message) }

// Log 经 Core 全局日志接口(AsstLog)写入一条日志:level 与 message
// 非 printf 变参、原样透传——合法 UTF-8、无内嵌 NUL、各不超过单字
// 符串预算,合计不超过 ConfigBytes(方法输入同样合计);允许
// 空串,由 Core 解释。
//
// C void 函数:没有成功码可查。返回 nil 只表示调用已发出并返回,不
// 构成原生确认成功的证明(AsstLog 只提供输入/生命周期错误);
// 错误返回只来自输入校验(ErrInvalidInput)或 Runtime 生命周期终止
// (ErrRuntimeTerminated)。本方法不经过实例状态:AsstLog 是进程级
// 全局函数,没有 handle,也不持有实例状态锁;生命周期检查在门锁内
// 读取状态,native 调用在锁外执行(任何 native 调用都不持有状
// 态锁)。
func (rt *Runtime) Log(level, message string) error {
	if err := validateStringField("level", level, rt.limits.InputStringBytes, true); err != nil {
		return err
	}
	if err := validateStringField("message", message, rt.limits.InputStringBytes, true); err != nil {
		return err
	}
	total, ok := addChecked(uint64(len(level)), uint64(len(message)))
	if !ok {
		return fmt.Errorf("maa: log input text bytes total overflows uint64: %w", native.ErrInvalidInput)
	}
	if total > rt.limits.ConfigBytes {
		return fmt.Errorf("maa: log input text bytes total %d exceeds Limits.ConfigBytes %d: %w",
			total, rt.limits.ConfigBytes, native.ErrInvalidInput)
	}
	rt.gate.mu.Lock()
	var lifecycleErr error
	switch rt.gate.state {
	case lifecycleReady:
		// ready:正常放行。
	case lifecycleTerminated:
		lifecycleErr = rt.gate.terminatedErrLocked()
	default:
		// idle/reserved 对经 Open 获得的 Runtime 不可达,防御拒绝
		// (对照 reserveInstance 的非 ready 分支)。
		lifecycleErr = fmt.Errorf("maa: runtime is not ready, log rejected: %w", native.ErrRuntimeInUse)
	}
	rt.gate.mu.Unlock()
	if lifecycleErr != nil {
		return lifecycleErr
	}
	lv, lvBuf := cString(level)
	msg, msgBuf := cString(message)
	rt.logger.log(lv, msg)
	// 带 NUL 的 C buffer 持有至 raw 返回(调用期保活)。
	runtime.KeepAlive(lvBuf)
	runtime.KeepAlive(msgBuf)
	return nil
}
