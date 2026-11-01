# CLI 维护指南

## 维护边界

- `main.go` 只保留版本注入与入口委托；实现和测试放在 `internal/cli`。`Run` 处理进程信号，`runCLI` 接收显式参数、context 和标准流。运行状态、任务关联、停止与记录恢复留在 control，参数编译和业务结果解释留在相应 adapter。
- 计划读取复用 `control.DecodePlan`；`run` 与 `dry-run` 共用适配器注册表和 `control.PreparePlan`。前置输入校验必须早于目录创建、加锁和 worker 启动，不能在 CLI 另建计划或适配器校验规则。
- 适配器仅由精确的 `task.adapter` 选择，不能按熟悉的任务类型自动启用。修改 `cliAdapterSpecs` 时同步输入说明、策略选择和未验证条件；help 与 capabilities 的名称、版本从 `Descriptor()` 派生，逐任务 `objective_policy` 从准备结果读取。
- 保持命令的副作用边界：help、version、capabilities 不检查安装；dry-run（含 `run --dry-run`）不创建目录、加锁或启动 worker；doctor 不准备适配器或提交设备动作，但可以初始化 Core、创建目录、加锁与恢复记录。路径检查须保留“ADB 不可用但仍可诊断 Core”的分支。

## 输出与关闭

- stdout 只写机器 JSON，stderr 只写稳定诊断码，不回显输入或 worker 日志尾部。原始 callback `details` 仅由 `--include-raw-events` 启用；typed 事件始终使用过滤后的业务事实。修改命令、输出或退出码时同步维护使用契约。
- 消费完缓冲事件并等待 `run.Done()` 后读取 `Snapshot()`，关闭 runtime、释放目录锁后再输出最终 `terminal`；不要直接转发内部 terminal 事件。退出码在 `resultExit`、`errorExit` 集中映射，独立检查 cleanup 与持久化错误。
- 运行输出保持单一 writer、有界排队和每次写入期限。stdout 阻塞或断开后仍须取消运行、关闭 runtime 并释放目录所有权；输出失败后不再向同一 stdout 写错误或 terminal。SIGINT、SIGTERM 和 context 取消沿用 control 停止路径。

## 测试选择

- `cli_test.go` 的 `TestMain` 通过环境开关复用测试可执行文件，分别运行 CLI 和协议夹具，不依赖真实 core-worker 或 MaaCore。输入与适配器的前置拒绝用例检查机器输出、stderr 隐私及未创建目录；文件和 stdin 入口均须覆盖。
- 注册表、能力声明和逐任务策略变更覆盖输入选择与预览；typed 执行变更覆盖同类型任务隔离、失败与不完整结果、终态与记录一致性。沿用跨进程夹具验证关联和输出，不能只测静态能力描述。
- 输出阻塞、断管、取消和超时使用受控 writer 与 helper 进程，检查中止后不再开始写入、目录锁释放与 worker 回收。Unix 信号测试保持相应平台约束；进程退出检查复用 `requireWorkerExited` 的平台处理。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [CLI 契约](README.md) | 修改命令、参数、JSON 输出、退出码或交付范围时读取 |
| [安装来源记录](installation-provenance.md) | 修改安装诊断、快照和资源来源说明时读取；附 manifest 模板入口 |
