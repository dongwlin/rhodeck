# worker 维护指南

## 进程与适配边界

- `main.go` 只保留版本注入与入口委托；实现位于 `internal/app`。进程入口负责 stdout 隔离与信号，session 调度协议命令，backend 通过 `engine` / `operation` 接口适配 maa。
- 首帧 hello 不加载 MaaCore；initialize 才创建 runtime/instance。payload 解码与 wire 预算校验在 factory 前，拒绝后保留会话；到达 engine factory 的初始化失败即结束会话，不能在失败后复用该会话。
- 初始化映射保留资源根和选项顺序。省略 limits 选择整套默认值，显式 limits 完整传递，不能逐字段补默认或把零值填回；响应报告实际应用配置。修改绑定默认值或字段时同步检查映射与 wire 预算。
- stdout 专用于协议。加载 MaaCore 前保留专用协议句柄并把 Go/原生 stdout 重定向到 stderr；平台实现还须阻止 ADB 等子进程继承协议句柄，不能只替换 `os.Stdout`。

## 会话与关闭

- native 命令由单一命令循环串行执行；`ping` 留在读取侧响应，不排在可能卡住的原生调用之后。
- Observe 独立消费绑定事件，Operation 等待不阻塞命令循环。先入队 acceptance response，再发送相应 operation event；session sequence 和 Core callback sequence 保持独立。
- 事件生产者只向有界队列交付持有独立数据的消息；不在 callback 线程写 IPC。队列/输出错误进入会话失败，不丢弃事件继续运行。
- close 先释放保留的图像快照，再关闭实例、等待 Observe 与 Operation 结束，刷新响应后退出。故障路径同样释放快照；不要用提前关闭 stdin 代替 close 交互。修改退出路径时验证响应已刷新，且原生卡住不能无限拖延进程退出。
- 新增命令需贯通协议 DTO、worker 路由、native 适配和客户端调用；capability 报告仅说明符号在场，不表示已有对应 wire 命令。

## 测试选择

- 命令状态与时序通过 `session_test.go` 的 fake engine 验证；字段映射、默认值与错误分类补 `backend_test.go`。涉及管道、stdout、退出或信号时，补真实 worker 子进程用例。
- 原生适配变更使用相应 Linux C 夹具验证接受、拒绝、异步结果与清理；fake engine 不能替代生产适配路径。图像变更覆盖快照所有权、并发准入、分块边界与关闭释放。
- `TestRealCoreWorkerNoDevice` 位于 `internal/app` 包，使用显式库、资源、SHA256 和版本。保留未连接状态下的本地拒绝探测，不引入设备发现、连接或任务启动。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [worker 使用与协议说明](README.md) | 修改初始化、命令、事件、预算、错误或关闭时读取；也提供版本注入与真实 Core 用例的运行参数 |
