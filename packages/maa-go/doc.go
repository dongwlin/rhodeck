// Package maa 是 MaaCore C ABI 的 Go 薄绑定。
//
// 本包经 purego 在运行时动态加载原生库并按符号调用其 C ABI,
// 不使用 cgo。生产包可完全以 CGO_ENABLED=0 构建;只有测试 fixture
// (模拟原生库的 C 代码)才需要 C 编译器,这是运行依赖与测试环境的
// 区别,不构成对生产构建的要求。
//
// 首期目标平台为 Linux amd64。其他平台的支持状态由后续任务按能力
// 探测契约另行声明,本包不承诺任何未经验证的平台。
//
// # 已交付的公开面
//
// Open(RuntimeConfig) 执行单进程初始化事务并返回 *Runtime;Runtime
// 提供只读元数据 Version、Capabilities、NullSize——全部读取初始化时
// 复制的不可变数据,绝不触发任何原生调用。配置与输入限制类型
// (RuntimeConfig、Limits、StaticOption、ConnectionExtra、BufferLimits)
// 的单位、默认值与校验语义由 MAA-03 冻结:Limits == nil 使用整套
// 默认值,非 nil 表示完整显式配置,越界与跨字段矛盾返回带字段路径
// 的 ErrInvalidInput,不静默截断或夹取。
//
// Runtime.NewInstance(InstanceConfig) 执行实例创建事务并返回
// *Instance:校验并复制实例配置 → 预留唯一实例名额并登记创建 lease
// → 建立实例状态、事件入口、callback 门与 token → CreateEx → 按序
// 应用实例选项 → 在 Runtime 同一状态门内复核可发布性与早到故障并发
// 布 ready Instance。全部步骤成功才返回实例;CreateEx=0、选项被原生
// 拒绝、早到故障或发布被 closing 抢先都走统一单次关闭/回滚流程清理,
// Runtime 终止使用,调用者拿不到半初始化实例。
//
// Instance 的公开面为同步生命周期、任务方法与异步提交:Close 同步、
// 幂等的单次关闭流程,返回后不再允许原生调用;AppendTask(taskType,
// params) 原样透传未知任务类型与 JSON 字段并返回 TaskID(0 明确表示
// 提交失败);SetTaskParams、Start、Stop、BackToHome 解释对应
// C bool——返回 nil 只表示请求被 Core 接受,一律不赋予任务开始/停
// 止/完成等执行语义(接受请求不等于任务执行完成,任务进度经事件流
// 观察);Running、Connected 的 false 是正常查询结果,不转换成错误。
// Connect(ConnectConfig)、Click(x, y)、Screencap 以 block=false 异
// 步提交设备动作并返回 *Operation:非零 ID 只表示提交被接受,最终结
// 果由 AsyncCallInfo 按 async_call_id 关联确认后经 Operation.Wait 取
// 得(可重复等待,取消只结束本次等待,不撤销已提交的原生动作);
// 完成回调先于 raw 返回时按 token + ID + 预期 what 接合,不依赖消息
// 到达顺序。未结清 Operation 数受 Limits.PendingOperations 约束,重
// 复、冲突或无法解释的完成信息使实例故障(ErrProtocol),不伪造成
// 功。实例关闭后 Runtime 进入终止使用状态,后续 Open/NewInstance 返
// 回 ErrRuntimeTerminated——每进程单 Runtime、单活动实例,不在同进
// 程重建实例或重试资源组合。
//
// Runtime.Log(level, message) 经 Core 全局日志接口(AsstLog,进程级、
// 无 handle)写入一条日志:C void 函数,返回 nil 只表示调用已发出并
// 返回,不构成原生确认成功的证明;错误返回只来自输入校验或 Runtime
// 生命周期终止。
//
// 实例与连接、任务输入的数据类型(InstanceConfig、InstanceOption、
// ConnectConfig)已定义,对应的纯 Go 校验与快照辅助同样遵循 MAA-03
// 冻结的语义(option key 非 0 且不重复、value 允许空串;ADBPath
// 非空绝对路径、Address 非空、Config 允许空串并原样传空 C 字符串;
// taskType 非空、params 为非空完整 JSON object);公开输出数据类型
// Event(MessageID、原始 JSON、本地事件序号)与 AsyncResult(ID、
// what、ret、原始 details)也已定义,内部记录持有独立的 JSON 缓冲区。
//
// 快照所有权边界(绑定设计 §6、§6.1):调用者在方法返回前不得并发
// 修改输入;返回后可修改原配置或原 RawMessage(包括原地改写底层
// 字节),不影响已冻结的快照。所有快照与输出副本都是独立的深拷贝:
// Event、AsyncResult 与每次重复/并发取得的结果副本可安全长期持有、
// 修改,不影响内部缓存、其他副本或之后的任何快照,实例关闭后仍然
// 有效。原始 JSON 不经 map[string]any 的 float64 往返,未知任务
// 类型、未知字段、未知消息 ID 与超过 2^53 的整数按原文字节保留。
//
// Instance 的图像、UUID 与任务列表读取已接入:GetImagePNG 读取 Core
// 缓存图像的 PNG 编码——读取的是最近一次采集的缓存结果,不触发任何
// 隐式截图,也不承诺缓存采集时间,需要新截图时调用方必须先提交
// Screencap 并等待其成功结果再读取;UUID 按原生返回的长度读取并转
// 换为 string,不寻找、不要求尾部 NUL,内容含 NUL 或非法 UTF-8 为
// ErrProtocol;Tasks 返回已入队任务的 ID 列表,容量与返回值均按
// int32 元素数处理。三者都复用有界输出缓冲区探测:每次调用分配实
// 际非空 buffer 并传真实容量(不以 nil/0 查询所需容量),返回值校验
// 顺序固定为 NullSize 哨兵 → 实际容量 → Go int 上限 → 转换/切片,
// 耗尽返回可辨识的 ErrBufferLimit(NullSize 原因不明),原生返回值
// 超过实际容量为 ErrProtocol;合法 0 长度返回空结果。返回的都是独
// 立 Go 快照,可长期持有、修改,不影响后续调用。可选 BGR(=
// AsstGetImageBgr)首期只报告能力(Runtime.Capabilities().ImageBgr),
// 不公开读取方法、不调用、不分配生产 buffer。事件流公开面
// (Events/Done/Err/Fault/Diagnostics)尚未接入,在它们被声明并验证
// 之前,调用方不应假设这些 API 可用。
//
// # Open 的进程级契约
//
//   - 每进程单 Runtime:进程级生命周期门串行化全部 Open。已有预占
//     或活动 Runtime 时,并发或重复 Open 返回 ErrRuntimeInUse;
//     binding 不复用已有对象、也不忽略新配置。
//   - 预占即不可回头:预占之后的任何失败(库加载、符号解析、注册、
//     初始化步骤)都使进程进入 terminated 使用状态,后续 Open 一律
//     返回包装 ErrRuntimeTerminated 的错误(附首个失败原因文本作
//     诊断);失败本身以原始错误返回,保留结构化错误类型与错误链。
//   - 不得同进程重试:原生库保持加载到进程退出,没有 Dlclose、
//     热重载或同进程重试原生初始化;恢复只能来自退出并启动新
//     worker 进程。
//   - 校验失败不消耗生命周期:纯 Go 的配置校验发生在预占之前,
//     失败返回带字段路径的 ErrInvalidInput,修正输入后仍可进行首次
//     原生加载。
//   - 配置快照所有权:校验通过后、预占之前,配置被深拷贝冻结为
//     快照;Open 返回后修改原配置(包括 slice 元素、RawMessage 与
//     Limits 字段)不影响已初始化 Runtime。调用者在 Open 返回前不得
//     并发修改输入。
//   - 初始化步骤顺序:Version 复制与 NullSize 核验 → 用户目录 →
//     静态选项(先于资源加载)→ 资源根(按配置顺序)→ 连接 extras
//     (按配置顺序);extras 为 C void 调用,不伪造原生成功码。
//
// Runtime 经 Open 返回的指针使用:零值不可用,不按值复制。元数据
// 方法并发安全;Open 并发安全(由生命周期门串行化预占)。
//
// 依赖方向固定且单向:公开层 → internal/native → purego / 系统 loader
// → MaaCore。internal/native 不向公开层导出 raw 函数类型、handle、
// 函数指针或借用内存;原生返回值的解释、数据复制与生命周期控制
// 一律由公开层完成。
package maa
