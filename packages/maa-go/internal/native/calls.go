// 本文件是 APITable 的 raw 调用包装边界:为 Runtime 初始化所需的 6 个
// raw 函数、实例创建所需的 2 个 raw 函数、实例同步业务调用所需的 8 个
// raw 函数、异步提交所需的 4 个 raw 函数与输出缓冲区读取所需的 4 个
// raw 函数提供直接转发的方法。函数指针与函数表仍不外泄——APITable 只经
// 公开层 Runtime 私有持有,本层方法不向公开层暴露任何 func 值;*byte
// 参数的编码(构造带 NUL 的 UTF-8 buffer 与保活)、返回值的解释与复制
// 全部由公开层完成。raw 签名保持冻结的 C 宽度,不做 Go 化改写。
//
// 转发即全部语义:本层不重试、不解释返回值、不为 C void 函数
// (SetConnectionExtras、Log)虚构成功码。

package native

import "unsafe"

// GetVersion 对应 AsstGetVersion():无参数,返回静态版本字符串的
// 借用指针。原生内存由 Core 持有,调用方必须立即有界复制为 Go
// 数据,不 free、不外泄视图。
func (t *APITable) GetVersion() *byte { return t.api.GetVersion() }

// GetNullSize 对应 AsstGetNullSize():输出缓冲区协议的失败哨兵值。
// 锁定 profile(maa-v6.20.1)约定为 math.MaxUint64,由公开层核验。
func (t *APITable) GetNullSize() uint64 { return t.api.GetNullSize() }

// SetUserDir 对应 AsstSetUserDir(path):设置进程级用户目录。返回 0
// 表示原生拒绝;C bool=false 通常没有结构化原因,调用方不得编造
// LastError 或其他不存在的诊断信息。
func (t *APITable) SetUserDir(path *byte) uint8 { return t.api.SetUserDir(path) }

// SetStaticOption 对应 AsstSetStaticOption(key, value):设置资源初始
// 化前的全局静态选项。返回 0 表示原生拒绝,语义同 SetUserDir。
func (t *APITable) SetStaticOption(key int32, value *byte) uint8 {
	return t.api.SetStaticOption(key, value)
}

// LoadResource 对应 AsstLoadResource(path):加载包含 resource/ 的
// 根目录。返回 0 表示原生拒绝,语义同 SetUserDir。
func (t *APITable) LoadResource(path *byte) uint8 { return t.api.LoadResource(path) }

// SetConnectionExtras 对应 AsstSetConnectionExtras(name, extras):
// C void 函数,没有成功码可查,本包装同样无返回值——不得伪造成功,
// 也不得把「调用已返回」解释成原生确认。
func (t *APITable) SetConnectionExtras(name *byte, extras *byte) {
	t.api.SetConnectionExtras(name, extras)
}

// Stop 对应 AsstStop(handle):请求停止实例的全部任务。返回 0 表示
// 原生拒绝;C bool=false 通常没有结构化原因,调用方不得编造
// LastError 或其他不存在的诊断信息。返回 0 不证明 handle 已被销毁,
// 也不证明任务队列已停止,解释权在调用方(关闭流程按仅将其
// 保留为清理诊断)。
func (t *APITable) Stop(handle uintptr) uint8 { return t.api.Stop(handle) }

// Destroy 对应 AsstDestroy(handle):C void 函数,没有成功码可查,
// 本包装同样无返回值——不得伪造成功,也不得把「调用已返回」解释成
// 原生确认或资源已回收。锁定 Core 的实现(Assistant 析构)在
// join 工作线程后于本调用线程同步触发 Destroyed 回调,回调经实例
// 的 callback 准入门照常入队。
func (t *APITable) Destroy(handle uintptr) { t.api.Destroy(handle) }

// CreateEx 对应 AsstCreateEx(callback, custom_arg):以进程唯一的
// callback trampoline 地址与整数 token 创建实例。返回 0(nullptr)
// 表示创建失败,没有结构化原因,调用方不得编造诊断;返回的非零值
// 是实例 handle 的唯一来源。custom_arg 是原生按值持有、回调按值收
// 回的整数 token(不是 Go 地址,绝不解引用);token 的登记必须先于
// 本调用(创建过程中的早到回调也要能找到实例状态),由公开层
// 创建事务保证。
func (t *APITable) CreateEx(callback uintptr, customArg uintptr) uintptr {
	return t.api.CreateEx(callback, customArg)
}

// SetInstanceOption 对应 AsstSetInstanceOption(handle, key, value):
// 设置实例级选项。返回 0 表示原生拒绝;C bool=false 通常没有结构化
// 原因,调用方不得编造 LastError 或其他不存在的诊断信息。调用契约:
// 仅在 CreateEx 已返回有效非零 handle 之后、由创建事务按配置顺序调
// 用(NewInstance)。
func (t *APITable) SetInstanceOption(handle uintptr, key int32, value *byte) uint8 {
	return t.api.SetInstanceOption(handle, key, value)
}

// AsyncConnect 对应 AsstAsyncConnect(handle, adb_path, address,
// config, block):异步提交设备连接。返回值是 AsstAsyncCallId
// (int32):0(InvalidId)表示提交失败;非零只证明提交已被 Core 接受
// 入队,最终结果以 AsyncCallInfo(async_call_id=该 ID)关联确认
// (AsstCaller.h:91–96),解释权在调用方。block 参数由公开层固定传 0
// (公共异步方法统一用 block=false,避免把 block 的完成标记当成
// 成功);adb_path/address/config 指向的 buffer 只需存活到本调用返回
// (锁定 Core 在入队前把连接参数复制到 std::string,Assistant.cpp
// async_connect)。
func (t *APITable) AsyncConnect(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32 {
	return t.api.AsyncConnect(handle, adbPath, address, config, block)
}

// AsyncClick 对应 AsstAsyncClick(handle, x, y, block):异步提交一次
// 点击,x/y 带符号原样透传。返回值语义同 AsyncConnect
// (AsstCaller.h:127);block 参数由公开层固定传 0。
func (t *APITable) AsyncClick(handle uintptr, x int32, y int32, block uint8) int32 {
	return t.api.AsyncClick(handle, x, y, block)
}

// AsyncScreencap 对应 AsstAsyncScreencap(handle, block):异步提交
// 一次截图,结果(缓存图像)另经 GetImage 读取。返回值语义同
// AsyncConnect(AsstCaller.h:128);block 参数由公开层固定传 0。
func (t *APITable) AsyncScreencap(handle uintptr, block uint8) int32 {
	return t.api.AsyncScreencap(handle, block)
}

// AppendTask 对应 AsstAppendTask(handle, type, params):向实例任务
// 队列追加一个任务,type 与 params 原样透传。返回值是 AsstTaskId
// (int32):0(InvalidId)表示提交失败;非零只证明任务已入队,入
// 队不等于任务已开始或执行成功(AsstCaller.h:81),解释权在调用方。
func (t *APITable) AppendTask(handle uintptr, taskType *byte, params *byte) int32 {
	return t.api.AppendTask(handle, taskType, params)
}

// SetTaskParams 对应 AsstSetTaskParams(handle, id, params):更新已
// 提交任务的原始 JSON 参数。返回 0 表示原生拒绝;C bool=false 通常
// 没有结构化原因,调用方不得编造 LastError 或其他不存在的诊断信息。
// 修改请求被接受同样不代表任务执行成功。
func (t *APITable) SetTaskParams(handle uintptr, id int32, params *byte) uint8 {
	return t.api.SetTaskParams(handle, id, params)
}

// Start 对应 AsstStart(handle):请求启动实例的任务队列。返回 0 表示
// 原生拒绝,语义同 SetTaskParams;返回非零只证明请求被接受。
func (t *APITable) Start(handle uintptr) uint8 { return t.api.Start(handle) }

// Running 对应 AsstRunning(handle):查询任务队列是否正在执行。
// 返回 0(false)是正常查询结果(未在执行,或实例未初始化),不是
// 可判别的错误;解释权在调用方。
func (t *APITable) Running(handle uintptr) uint8 { return t.api.Running(handle) }

// Connected 对应 AsstConnected(handle):查询实例是否已连接设备。
// 返回 0(false)是正常查询结果,语义同 Running。
func (t *APITable) Connected(handle uintptr) uint8 { return t.api.Connected(handle) }

// BackToHome 对应 AsstBackToHome(handle):请求实例回到主界面。返回
// 0 表示原生拒绝,语义同 SetTaskParams。
func (t *APITable) BackToHome(handle uintptr) uint8 { return t.api.BackToHome(handle) }

// Log 对应 AsstLog(level, message):进程级全局日志写入,level 与
// message 非 printf 变参、原样透传。C void 函数,没有成功码可查,
// 本包装同样无返回值——不得伪造成功,也不得把「调用已返回」解释成
// 原生确认(AsstCaller.cpp 的实现只把文本交给内部日志器)。
func (t *APITable) Log(level *byte, message *byte) { t.api.Log(level, message) }

// GetImage 对应 AsstGetImage(handle, buff, buff_size):读取 Core 缓
// 存图像的 PNG 编码。容量与返回值均为字节数;buff 必须指向实际分配
// 的无 Go 指针字节缓冲区,仅本次调用使用,保活责任在调用方(以
// runtime.KeepAlive 持有至 raw 返回)。返回 NullSize 哨兵表示失败、
// 0 是合法空结果(未连接设备时图像 getter 也可能合法返回 0),两者
// 的解释权在调用方——公开层的有界缓冲区探测按固定顺序校验,
// 不得先传 nil/0 查询所需容量。读取的是最近一次采集的缓存图像,本
// 调用不触发任何截图,也不承诺缓存采集时间。
func (t *APITable) GetImage(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
	return t.api.GetImage(handle, buff, buffSize)
}

// GetImageBgr 对应 AsstGetImageBgr(handle, buff, buff_size):读取
// Core 缓存图像的 BGR 像素数据,契约同 GetImage——容量与返回值均为
// 字节数,buff 保活责任在调用方,NullSize 哨兵与合法 0 的解释权在
// 调用方;像素布局(BGR 通道序、步长)由 Core 决定,本层不解释。
// 符号是可选项,缺失时字段为 nil,调用方必须先经能力快照判定。
func (t *APITable) GetImageBgr(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64 {
	return t.api.GetImageBgr(handle, buff, buffSize)
}

// AsyncAttachWindow 对应 AsstAsyncAttachWindow(handle, hwnd,
// screencap_method, mouse_method, keyboard_method, block):异步提交
// Win32 窗口绑定。返回值语义同 AsyncConnect——0(InvalidId)表示提交
// 失败,非零只证明提交已被 Core 接受入队,最终结果以
// AsyncCallInfo(async_call_id=该 ID,what=AttachWindow)关联确认,
// 解释权在调用方;block 参数由公开层固定传 0。符号仅 Windows 参与查
// 找(windowsOnly),缺失时字段为 nil,调用方必须先经能力快照判定;
// 该符号在场的非 Windows 库不被查找,方法层面只在 Windows 生效。
func (t *APITable) AsyncAttachWindow(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32 {
	return t.api.AsyncAttachWindow(handle, hwnd, screencapMethod, mouseMethod, keyboardMethod, block)
}

// GetMapLevelKey 对应 AsstGetMapLevelKey(key):按关卡名、code、
// stageId 或 levelId 查找匹配关卡,按值返回 struct AsstMapLevelKey
// 的 raw 同构。返回结构体的成员指针由 Core 借出(实现写入
// thread_local 存储,只在本次调用与同线程的下一次调用之间有效),
// 调用方必须立即有界复制为 Go 数据,不 free、不缓存、不外泄视图;
// 找不到匹配关卡(key 为 nil 或无匹配)时全部成员为 nil,由公开层
// 解释为「未找到」。符号是 ASST_WITH_EXTRA_CALLERS 条件的实验导出,
// 缺失时字段为 nil,调用方必须先经能力快照判定。
func (t *APITable) GetMapLevelKey(key *byte) mapLevelKey { return t.api.GetMapLevelKey(key) }

// GetItemName 对应 AsstGetItemName(id):按物品 Id 查找物品名称,
// 返回借用字符串指针——由 Core 持有(实现写入 thread_local 存储),
// 调用方必须立即有界复制为 Go 数据,不 free、不缓存、不外泄视图。
// C 契约:空 Id 返回 "Unknown",找不到的 Id 返回空串,指针恒非 nil,
// nil 返回是协议破坏,由公开层按 ErrProtocol 处理。符号是
// ASST_WITH_EXTRA_CALLERS 条件的实验导出,缺失时字段为 nil,调用方
// 必须先经能力快照判定。
func (t *APITable) GetItemName(id *byte) *byte { return t.api.GetItemName(id) }

// GetUUID 对应 AsstGetUUID(handle, buff, buff_size):读取设备 UUID。
// 容量与返回值均为字节数,返回长度是有效内容的全部——不写尾部 NUL,
// 调用方必须只按返回长度读取,不得在缓冲区内寻找或要求 NUL(
// 按返回长度读取,不依赖尾部 NUL)。NullSize 哨兵与合法 0 的解释权
// 在调用方,契约同 GetImage。
func (t *APITable) GetUUID(handle uintptr, buff *byte, buffSize uint64) uint64 {
	return t.api.GetUUID(handle, buff, buffSize)
}

// GetTasksList 对应 AsstGetTasksList(handle, buff, buff_size):读取
// 实例任务列表。容量与返回值均为 AsstTaskId 元素数——不是字节数、
// 不做 ×4 换算,换算只发生在公开层分配前的 Go int 守卫(每元
// 素 4 字节为冻结值)。NullSize 哨兵与合法 0 的解释权在调用
// 方,契约同 GetImage。
func (t *APITable) GetTasksList(handle uintptr, buff *int32, buffSize uint64) uint64 {
	return t.api.GetTasksList(handle, buff, buffSize)
}
