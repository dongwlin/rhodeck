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
// Instance、Operation、事件流与 Close 仍不存在,由 M3 承接;在它们
// 被声明并验证之前,调用方不应假设这些 API 可用。公开面没有任何
// 可创建半成品 Instance 的入口。
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
