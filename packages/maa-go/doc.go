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
// 当前本包仅是最小骨架:只提供公开 ID 类型(TaskID、AsyncID、MessageID)
// 与错误分类(状态哨兵错误、加载/注册结构化错误、聚合错误)。
// 生命周期(Open/Close)、实例、异步 Operation 与事件流等 API 尚不存在,
// 将由后续任务按已冻结的绑定契约逐步提供;在它们被声明并验证之前,
// 调用方不应假设这些 API 可用。
//
// 依赖方向固定且单向:公开层 → internal/native → purego / 系统 loader
// → MaaCore。internal/native 不向公开层导出 raw 函数类型、handle、
// 函数指针或借用内存;原生返回值的解释、数据复制与生命周期控制
// 一律由公开层完成。
package maa
