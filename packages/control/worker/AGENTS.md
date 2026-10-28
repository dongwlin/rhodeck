# worker 监管与协议维护指南

## 客户端所有权

- `Client` 持有子进程、管道和全部 IPC goroutine，只通过 Launch 返回的指针使用。启动可执行文件使用绝对路径和 `exec.Command`，不引入 shell 或设备发现。
- Launch 的 context 只约束启动；返回后的会话生命周期由 Close 管理。startup/request/shutdown 超时值为 0 时用默认值，负值在启动前拒绝。
- 并发请求经同一写队列维持 request ID 发送顺序；响应按 ID 和 name 匹配，可乱序返回。保留 pending 上限、ID 耗尽及重复/未知响应的显式拒绝。
- 区分请求入队前拒绝与入队后取消：预取消不破坏已有会话；入队后取消/超时可能已经执行，必须终止会话并保留 `Failure.Unknown`，不能只删 pending 然后继续复用。
- Close 并发/重复调用共享清理；必须同时确认 close 响应和进程退出。即使已收到响应，进程仍存活也须受 shutdown 期限约束；终止后 Wait 回收，并结束 IPC 后再关闭 Done。
- 事件预算同时约束数量和保留字节，不能只限制 channel 长度。
- stderr 仅保存有界尾部，可能含连接和原生诊断隐私；不自动向外部日志系统转发。远端命令错误是 `*protocol.Error`，进程/传输故障是 `*Failure`，不要混淆二者。

## protocol 子包

- wire DTO 与稳定常量集中定义。修改版本、命令、事件、字段或预算时同步检查两端兼容性与 schema 校验。
- 保留严格 JSONL 解码：完整换行帧、合法 UTF-8、版本匹配、控制字段唯一、拒绝未知控制字段和尾随数据。
- request ID 在会话内严格递增且非零；事件有自己的连续 sequence，CoreSequence 单独校验。Operation 结果必须匹配已接受的 request/async ID，不能只检查 JSON 可解码。
- `Queue.Send` 不等待消费者；数量、编码字节和读取帧分配均有界。改变预算或记账时覆盖出队释放、消费者停读、超限帧与阻塞写入路径。

## 测试选择

- 编解码与队列修改补 `protocol/codec_test.go`，包含破坏输入与边界反例；大整数、未知原生字段不能只用普通小整数正例验证。
- 生命周期、期限、响应关联和故障处理使用 `client_test.go` 的真实 helper 子进程；覆盖无 hello、坏响应、断管、卡住或未退出等相关路径，断言进程已回收。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [监管客户端说明](README.md) | 修改 Launch/Close、超时、事件消费、诊断或故障语义时读取调用契约 |
