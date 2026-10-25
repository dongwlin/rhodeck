// Package native 将承载 maa-go 的 raw 边界层:raw 符号表、固定宽度
// 类型、平台 loader 与 callback 装配。
//
// 本包不向公开层(Package maa)导出 raw 函数类型、handle(uintptr)、
// 函数指针或任何借用内存;原生返回值的解释、数据复制与生命周期控制
// 由公开层完成。公开层到本包是单向依赖,本包不得反向引用公开层,
// 也不得直接依赖上层调度或业务概念。
//
// 当前本任务只建立这一边界:尚未声明任何 raw 符号。符号表、加载
// 事务与 callback ABI 将由后续任务按已冻结的绑定契约(设计 §3、§4)
// 逐步提供。
package native
