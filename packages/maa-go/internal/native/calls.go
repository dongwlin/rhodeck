// 本文件是 APITable 的 raw 调用包装边界:为 Runtime 初始化(Open
// 事务)所需的 6 个 raw 函数与实例创建(NewInstance 事务)所需的
// 2 个 raw 函数提供直接转发的方法。函数指针与函数表仍不外泄——
// APITable 只经公开层 Runtime 私有持有,本层方法不向公开层暴露任何
// func 值;*byte 参数的编码(构造带 NUL 的 UTF-8 buffer 与保活)、
// 返回值的解释与复制全部由公开层完成。raw 签名保持绑定设计 §3 冻
// 结的 C 宽度,不做任何 Go 化改写。
//
// 转发即全部语义:本层不重试、不解释返回值、不为 C void 函数
// (SetConnectionExtras)虚构成功码。其余 raw 字段暂无调用包装,
// 由实例与回调里程碑按需添加;api.go/symbols.go/callback_types.go
// 及其静态测试保持冻结,不得因新增包装而改动。

package native

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
// 也不证明任务队列已停止,解释权在调用方(关闭流程按 §8.1 仅将其
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
// 本调用(§4:创建过程中的早到回调也要能找到实例状态),由公开层
// 创建事务保证。
func (t *APITable) CreateEx(callback uintptr, customArg uintptr) uintptr {
	return t.api.CreateEx(callback, customArg)
}

// SetInstanceOption 对应 AsstSetInstanceOption(handle, key, value):
// 设置实例级选项。返回 0 表示原生拒绝;C bool=false 通常没有结构化
// 原因,调用方不得编造 LastError 或其他不存在的诊断信息。调用契约:
// 仅在 CreateEx 已返回有效非零 handle 之后、由创建事务按配置顺序调
// 用(§8 NewInstance)。
func (t *APITable) SetInstanceOption(handle uintptr, key int32, value *byte) uint8 {
	return t.api.SetInstanceOption(handle, key, value)
}
