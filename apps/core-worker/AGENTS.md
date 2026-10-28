# core-worker 维护指南

## 进程与适配边界

- 保持 `main` 负责参数、stdout 隔离与信号，`session` 负责协议与命令调度，`backend` 通过 `engine` / `operation` 接口适配 maa。
- 首帧 hello 不加载 MaaCore；initialize 才创建 runtime/instance。到达 engine factory 的初始化尝试失败即结束会话，恢复使用新进程；不要改成同会话反复初始化。
- 初始化使用显式绝对路径，资源根顺序保留。安装快照由调用方保持不可变；库摘要和资源版本文件摘要不能代表整个资源树或设备所有权。
- stdout 专用于协议。加载 MaaCore 前保留专用协议句柄并把 Go/原生 stdout 重定向到 stderr；平台实现还须阻止 ADB 等子进程继承协议句柄，不能只替换 `os.Stdout`。

## 会话与关闭

- native 命令由单一命令循环串行执行；`ping` 留在读取侧响应，不排在可能卡住的原生调用之后。
- Observe 独立消费绑定事件，Operation 等待不阻塞命令循环。先入队 acceptance response，再发送相应 operation event；session sequence 和 Core callback sequence 保持独立。
- 事件生产者只向有界队列交付持有独立数据的消息；不在 callback 线程写 IPC。队列/输出错误进入会话失败，不丢弃事件继续运行。
- close 停止并销毁实例、排空观察与 Operation 后，刷新响应并退出。EOF、信号、协议损坏与故障停止准入，沿用现有清理路径和进程退出期限；不要用提前关闭 stdin 代替 close 交互。
- 正常 close 退出 0，失败会话退出 1，命令行用法错误退出 2。修改退出路径时验证响应已刷新，且原生卡住不能无限拖延进程退出。
- 新增命令需贯通协议 DTO、worker 路由、native 适配和客户端调用；capability 报告仅说明符号在场，不表示已有对应 wire 命令。

## 测试选择

- 命令状态与时序通过 `session_test.go` 的 fake engine 验证；错误分类补 `backend_test.go`。涉及管道、stdout、退出或信号时，补真实 worker 子进程用例，不能仅以 fake 会话测试替代。
- 原生方法新增或更改时覆盖接受/拒绝、异步结果与清理；mock 不能证明生产 native 适配或 ABI 正确。
- 真实 Core 用例 `TestRealCoreWorkerNoDevice` 仅在显式配置库、资源、SHA256 和版本后运行；它不连接设备、不启动任务。修改该用例不能引入隐式设备发现或任务执行。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [worker 使用与协议说明](README.md) | 修改初始化、命令、事件、预算、错误或关闭时读取；也提供版本注入与真实 Core 用例的运行参数 |
