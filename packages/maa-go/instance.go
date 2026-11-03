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

type instanceCloser interface {
	stop(handle uintptr) uint8
	destroy(handle uintptr)
}

type apiTableCloser struct {
	table *native.APITable
}

func (c apiTableCloser) stop(handle uintptr) uint8 { return c.table.Stop(handle) }

func (c apiTableCloser) destroy(handle uintptr) { c.table.Destroy(handle) }

type instance struct {
	state  *instanceState
	disp   *eventDispatcher
	token  uintptr
	closer instanceCloser
}

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

func (in *instance) close() error {
	if !in.state.electClose() {
		return in.state.waitClose()
	}

	in.state.waitCallsDrained()

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

func closeTail(s *instanceState, d *eventDispatcher, token uintptr) error {
	s.closeCallbackGate()
	s.waitCallbacksDrained()
	if token != 0 {
		native.UnregisterCallbackTarget(token)
	}

	d.closeIntake()
	d.requestDrain()
	d.waitProcessed()
	s.settleRemainingPending()
	s.publishFinalError(s.currentError())
	s.publishClosed()
	d.closeEvents()
	s.publishCloseDone()
	return s.finalError()
}

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

// Instance 是一个已成功创建的 MaaCore 实例绑定:全部字段未导出,只能
// 经 Runtime.NewInstance 返回的指针使用,不按值复制。
type Instance struct {
	inst   *instance
	rt     *Runtime
	caller instanceCaller
}

// Close 同步销毁实例:幂等,返回稳定的最终错误(正常关闭为 nil)。
// 多次或并发调用只有一个所有者执行一次 Stop、一次 Destroy,其余申请者
// 等待同一关闭结果并取得同一最终错误;Close 不接受强制回收超时,
// Stop/Destroy 卡住时由 control 对 worker 执行有期限的退出监督。
//
// 实例关闭完成(包括关闭失败返回错误)后,创建它的 Runtime 进入终止
// 使用状态:后续 Open 与 NewInstance 一律返回包装 ErrRuntimeTerminated
// 的错误;不在同进程重建实例。
func (in *Instance) Close() error {
	err := in.inst.close()
	in.rt.gate.terminate(err)
	return err
}

type instanceCreator interface {
	createEx(callback uintptr, customArg uintptr) uintptr
	setInstanceOption(handle uintptr, key int32, value *byte) uint8
}

type apiTableCreator struct {
	table *native.APITable
}

func (c apiTableCreator) createEx(callback uintptr, customArg uintptr) uintptr {
	return c.table.CreateEx(callback, customArg)
}

func (c apiTableCreator) setInstanceOption(handle uintptr, key int32, value *byte) uint8 {
	return c.table.SetInstanceOption(handle, key, value)
}

type instanceCaller interface {
	appendTask(handle uintptr, taskType *byte, params *byte) int32
	setTaskParams(handle uintptr, id int32, params *byte) uint8
	start(handle uintptr) uint8
	stop(handle uintptr) uint8
	backToHome(handle uintptr) uint8
	running(handle uintptr) uint8
	connected(handle uintptr) uint8
	asyncConnect(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32
	asyncClick(handle uintptr, x int32, y int32, block uint8) int32
	asyncScreencap(handle uintptr, block uint8) int32
	asyncAttachWindow(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32
	getImage(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	getImageBgr(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	getUUID(handle uintptr, buff *byte, buffSize uint64) uint64
	getTasksList(handle uintptr, buff *int32, buffSize uint64) uint64
}

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

func createInstance(rt *Runtime, cfg InstanceConfig, creator instanceCreator, closer instanceCloser, caller instanceCaller) (*Instance, error) {
	if creator == nil || closer == nil || caller == nil {
		panic("maa: instance created without complete native seams (creator/closer/caller)")
	}
	snap, err := snapshotInstanceConfig(cfg, rt.limits)
	if err != nil {
		return nil, err
	}
	if err := rt.gate.reserveInstance(); err != nil {
		return nil, err
	}

	s := newInstanceState(0, rt.limits.PendingOperations, rt.limits.EarlyBytes)
	d := newEventDispatcher(s, s.settleCompletion,
		rt.limits.CallbackBytes, rt.limits.InternalEvents, rt.limits.InternalBytes,
		rt.limits.PublicEvents, rt.limits.PublicBytes)
	d.start()
	target := newCallbackTarget(s, d, rt.limits.CallbackBytes)
	token, err := native.RegisterCallbackTarget(target.deliver)
	if err != nil {
		return nil, rollbackCreation(rt, s, d, 0, closer, err)
	}
	in := newInstance(s, d, token, closer)

	handle := creator.createEx(native.CallbackTrampoline(), token)
	if handle == 0 {
		return nil, rollbackCreation(rt, s, d, token, closer,
			fmt.Errorf("maa: AsstCreateEx returned 0 (null handle): %w", native.ErrNativeRejected))
	}
	s.publishHandle(handle)

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

	if err := rt.gate.publishInstance(in); err != nil {
		return nil, rollbackCreation(rt, s, d, token, closer, err)
	}
	rt.gate.releaseCreationLease()
	return &Instance{inst: in, rt: rt, caller: caller}, nil
}

func rollbackCreation(rt *Runtime, s *instanceState, d *eventDispatcher, token uintptr, closer instanceCloser, cause error) error {
	s.recordFault(cause)
	var err error
	if token != 0 {
		err = newInstance(s, d, token, closer).close()
	} else {
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

func (in *Instance) boolCall(what string, call func(handle uintptr) uint8) error {
	return in.syncCall(what, func(handle uintptr) error {
		if call(handle) == 0 {
			return fmt.Errorf("maa: Asst%s returned 0 (false), request rejected: %w", what, native.ErrNativeRejected)
		}
		return nil
	})
}

func (in *Instance) queryCall(what string, call func(handle uintptr) uint8) (bool, error) {
	var ret uint8
	err := in.syncCall(what, func(handle uintptr) error {
		ret = call(handle)
		return nil
	})
	return ret != 0, err
}

// AppendTask 向实例任务队列追加一个任务:taskType 原样透传,不做业务
// 枚举校验,未知任务类型保留;params 必须是非空、完整的 JSON object
// 原文,未知字段与超过 2^53 的整数按原文字节透传,不经
// map[string]any 往返、不丢弃字段。
//
// 返回的 TaskID 非 0 只证明任务已被 Core 接受入队,不表示任务已开始或
// 执行完成;任务进度经事件流观察。返回 (0, err) 且错误包装
// ErrNativeRejected 表示提交被原生拒绝;输入校验失败返回包装
// ErrInvalidInput 的错误,不进入 native;closing/faulted/closed 的拒绝
// 经错误链可辨(ErrClosing/首因/ErrClosed)。
func (in *Instance) AppendTask(taskType string, params json.RawMessage) (TaskID, error) {
	snap, err := snapshotTaskInput(taskType, params, in.rt.limits)
	if err != nil {
		return 0, err
	}
	var id int32
	err = in.syncCall("AppendTask", func(handle uintptr) error {
		t, tBuf := cString(snap.taskType)
		p, pBuf := cString(string(snap.params))
		id = in.caller.appendTask(handle, t, p)
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
// 完整的 JSON object 原文,与任务 params 同一校验语义;id 原样透传
// (保留 int32 符号位),不存在或已结束的任务由 Core 原生拒绝。
//
// 返回 nil 只表示修改请求被 Core 接受,不等于任务执行成功,也不等于
// 参数已按新值生效于进行中的步骤;任务进度经事件流观察。返回包装
// ErrNativeRejected 的错误表示请求被原生拒绝;输入与生命周期错误语义
// 同 AppendTask。
func (in *Instance) SetTaskParams(id TaskID, params json.RawMessage) error {
	if err := validateJSONObject("params", params, in.rt.limits.InputJSONBytes); err != nil {
		return err
	}
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

// Start 请求启动实例的任务队列。返回 nil 只表示请求被 Core 接受,不表示
// 任务队列已开始执行,更不表示任何任务执行成功;任务进度经事件流观察。
// 返回包装 ErrNativeRejected 的错误表示请求被原生拒绝(C bool=false,
// 无结构化原因)。被拒绝时只向本次调用返回拒绝:不自动 Destroy、不撤销
// 异步队列。
func (in *Instance) Start() error {
	return in.boolCall("Start", in.caller.start)
}

// Stop 请求停止实例的全部任务:停止请求作用于任务队列,不是按 async ID
// 取消单个异步操作(C API 无按 async ID 取消接口)。返回 nil 只表示停止
// 请求被 Core 接受,不表示任务已停止或队列已排空,停止进度经事件流观察;
// 返回包装 ErrNativeRejected 的错误表示请求被原生拒绝:不自动 Destroy、
// 不撤销异步队列。输入与生命周期错误语义同 AppendTask。
func (in *Instance) Stop() error {
	return in.boolCall("Stop", in.caller.stop)
}

// BackToHome 请求实例回到主界面。返回 nil 只表示请求被 Core 接受,
// 不表示界面动作已完成;返回包装 ErrNativeRejected 的错误表示请求被
// 原生拒绝。输入与生命周期错误语义同 AppendTask。
func (in *Instance) BackToHome() error {
	return in.boolCall("BackToHome", in.caller.backToHome)
}

// Running 查询实例的任务队列是否正在执行:false 是正常查询结果,返回
// (false, nil),不转换成错误;生命周期失败(closing/faulted/closed 的
// 准入拒绝)单独经错误返回,与正常 false 可辨,错误包装 ErrClosing、
// 实例首因或 ErrClosed。
func (in *Instance) Running() (bool, error) {
	return in.queryCall("Running", in.caller.running)
}

// Connected 查询实例是否已连接设备:false 是正常查询结果,返回
// (false, nil),不转换成错误;生命周期失败单独经错误返回,语义同
// Running。
func (in *Instance) Connected() (bool, error) {
	return in.queryCall("Connected", in.caller.connected)
}

const asyncBlockFalse = uint8(0)

const (
	asyncWhatConnect      = "Connect"
	asyncWhatClick        = "Click"
	asyncWhatScreencap    = "Screencap"
	asyncWhatAttachWindow = "AttachWindow"
)

func (in *Instance) asyncCall(what string, call func(handle uintptr) int32) (*Operation, error) {
	lease, err := in.inst.state.admitCall(what)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	op, err := in.inst.state.reservePendingOperation(what)
	if err != nil {
		return nil, err
	}
	in.inst.state.raw.enter()
	_, joinErr := func() (int32, error) {
		defer in.inst.state.raw.exit()
		if err := in.inst.state.beginAsyncRaw(); err != nil {
			return 0, err
		}
		id := call(lease.handle())
		return id, in.inst.state.finishAsyncRaw(op, what, id)
	}()
	if joinErr != nil {
		return nil, joinErr
	}
	return &Operation{op: op}, nil
}

// Connect 异步提交设备连接(AsstAsyncConnect,block=false):返回的
// *Operation 持有关联键 async ID,连接的最终结果由 AsyncCallInfo 按该 ID
// 关联确认后经 Wait 取得——非零 ID 只表示提交被接受,不是连接已建立的
// 证明。ADBPath/Address/Config 的输入校验与快照语义同同步入口(路径非空
// 绝对、Address 非空、Config 允许空串并原样传空 C 字符串);快照只在
// raw 调用期间保活,方法返回后即可修改原输入。
//
// 错误语义:输入校验失败包装 ErrInvalidInput 且不进入 native;容量满包装
// ErrOperationLimit(不调用 native,实例不故障);生命周期拒绝经错误链可辨
// (ErrClosing/首因/ErrClosed);raw 返回 0 包装 ErrNativeRejected(明确的
// 提交失败,已回收预留);ID 复用/倒退或与早到结果矛盾等协议破坏包装
// ErrProtocol 并使实例故障。
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
		runtime.KeepAlive(adbBuf)
		runtime.KeepAlive(addrBuf)
		runtime.KeepAlive(confBuf)
		return id
	})
}

// Click 异步提交一次点击(AsstAsyncClick,block=false):x/y 带符号原样
// 透传,返回的 *Operation 语义同 Connect——最终结果经 Wait 取得,非零 ID
// 只表示提交被接受。错误语义同 Connect(无输入校验步骤)。
func (in *Instance) Click(x, y int32) (*Operation, error) {
	return in.asyncCall(asyncWhatClick, func(handle uintptr) int32 {
		return in.caller.asyncClick(handle, x, y, asyncBlockFalse)
	})
}

// Screencap 异步提交一次截图(AsstAsyncScreencap,block=false):返回的
// *Operation 语义同 Connect——提交被接受只表示采集请求已入队,结果经
// Wait 确认后另以图像读取接口取缓存图像(获取新截图需先等待 Screencap 的
// 成功结果,再取 PNG;读取接口见 GetImagePNG)。错误语义同 Connect。
func (in *Instance) Screencap() (*Operation, error) {
	return in.asyncCall(asyncWhatScreencap, func(handle uintptr) int32 {
		return in.caller.asyncScreencap(handle, asyncBlockFalse)
	})
}

const (
	outputNameImage    = "GetImage"
	outputNameImageBGR = "GetImageBgr"
	outputNameUUID     = "GetUUID"
	outputNameTasks    = "GetTasksList"
)

// GetImagePNG 读取 Core 缓存图像的 PNG 编码(AsstGetImage):返回的是
// 最近一次采集图像的缓存结果——本方法不触发任何隐式截图,也不承诺缓存
// 采集时间(任务执行期间缓存仍可能更新,扩容重试也不固定同一张图像);
// 需要新截图时,调用方必须先提交 Screencap 并等待其成功结果,再调用本
// 方法读取。未连接设备时也可能合法返回 0 长度(空、非 nil 切片),binding
// 不将其伪造成截图成功或失败。返回独立 Go 快照,可长期持有、修改。
//
// 容量探测按 Limits.ImagePNG(字节)执行,返回错误语义:探测耗尽包装
// ErrBufferLimit;原生返回值大于实际容量包装 ErrProtocol;生命周期拒绝
// (closing/faulted/closed)经错误链可辨。
func (in *Instance) GetImagePNG() ([]byte, error) {
	var png []byte
	err := in.syncCall("GetImagePNG", func(handle uintptr) error {
		var err error
		png, err = readOutputBytes(outputNameImage, func(buf []byte) uint64 {
			ret := in.caller.getImage(handle, unsafe.Pointer(&buf[0]), uint64(len(buf)))
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
// 可选能力):缓存语义与 GetImagePNG 完全一致——读取的是最近一次采集
// 图像的缓存结果,不触发任何隐式截图,也不承诺缓存采集时间;需要新截图
// 时先提交 Screencap 并等待其成功结果再读取。返回值是原始 BGR 字节
// (通道序与步长由 Core 决定,binding 不解释,也不返回图像尺寸),独立
// Go 快照,可长期持有、修改。
//
// 能力门:符号未随本次加载的原生库导出时,在准入前直接返回包装
// ErrUnsupported 的错误(不进入 native、不占在途计数、不分配 BGR
// buffer)。在场时容量探测按 Limits.ImageBGR(字节)执行,错误语义同
// GetImagePNG。
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
// string——不寻找、不要求尾部 NUL,缓冲区容量内的其余内容一律丢弃。
// 转换前校验内容:含 NUL 字节或非法 UTF-8 为协议破坏,返回包装
// ErrProtocol 的错误,不把矛盾证据伪装成成功。合法 0 长度返回空串。
// 返回独立 Go 快照(string 按语言语义共享)。
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
// 列表——入队中的任务(含尚未 Start 的)也会出现,列表内容与元素顺序由
// Core 决定,binding 不解释任务状态。容量与返回值均按 int32 元素数处理、
// 不当作字节数;id 保留 int32 符号位转换为 TaskID。合法 0 长度返回空、
// 非 nil 切片。返回独立 Go 快照,可长期持有、修改。
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

type runtimeLogger interface {
	log(level *byte, message *byte)
}

type apiTableLogger struct {
	table *native.APITable
}

func (l apiTableLogger) log(level *byte, message *byte) { l.table.Log(level, message) }

// Log 经 Core 全局日志接口(AsstLog)写入一条日志:level 与 message
// 原样透传,必须是合法 UTF-8、无内嵌 NUL、各不超过单字符串预算,合计
// 不超过 ConfigBytes;允许空串,由 Core 解释。
//
// C void 函数:没有成功码可查。返回 nil 只表示调用已发出并返回,不构成
// 原生确认成功的证明;错误返回只来自输入校验(ErrInvalidInput)或 Runtime
// 生命周期终止(ErrRuntimeTerminated)。本方法不经过实例状态,也不持有
// 实例状态锁。
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
	case lifecycleTerminated:
		lifecycleErr = rt.gate.terminatedErrLocked()
	default:
		lifecycleErr = fmt.Errorf("maa: runtime is not ready, log rejected: %w", native.ErrRuntimeInUse)
	}
	rt.gate.mu.Unlock()
	if lifecycleErr != nil {
		return lifecycleErr
	}
	lv, lvBuf := cString(level)
	msg, msgBuf := cString(message)
	rt.logger.log(lv, msg)
	runtime.KeepAlive(lvBuf)
	runtime.KeepAlive(msgBuf)
	return nil
}
