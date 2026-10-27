// Package native 是 maa-go 的 raw 边界层:raw 符号表、固定宽度类型、
// 平台 loader 与 callback 装配。
//
// 本包不向公开层(Package maa)导出 raw 函数类型、handle(uintptr)、
// 函数指针或任何借用内存;原生返回值的解释、数据复制与生命周期控制
// 由公开层完成。公开层到本包是单向依赖,本包不得反向引用公开层,
// 也不得直接依赖上层调度或业务概念。
//
// 本包提供平台 loader 原语、声明式 raw 符号清单与原子注册事务:按
// 「解析→注册→发布」顺序构造每次事务自己的临时 API 表,必需符号缺失时
// 整体报告且注册器零调用,可选符号缺失只关闭对应 capability,注册期
// panic 转换为结构化错误并丢弃整张临时表;全部成功才一次性只读发布
// APITable 与 Capabilities。Runtime 初始化、实例与 callback trampoline
// 由公开层通过本包的 loader/注册/投递原语装配。
package native
