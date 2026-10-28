# maa-go 维护指南

## 分层与 ABI

- 公开包名是 `maa`。公开层负责校验、快照、状态门和错误语义；`internal/native` 负责 raw 调用、符号注册、loader 与 callback 装配，不承载任务业务模型。
- 当前 ABI profile 锁定 `maa-v6.20.1`。raw 字段名对应 C 符号去掉 `Asst` 前缀；保持 C 宽度、参数顺序、指针角色和 void 返回语义，不用 Go `bool` / `int` 替换 ABI 类型。
- `api.go`、`symbols.go`、`callback_types.go` 必须无条件参与构建。声明 checker 要求可静态提取的直接声明，不改成 helper、循环或运行时拼接清单。
- 新增/改变绑定时同步核对 raw 声明、required/optional 与平台策略、注册包装、公开入口和 C 夹具；保留 checker 独立期望表，不能只让 Go 声明与其派生清单自洽。
- optional 符号缺席只关闭对应 capability，调用返回 `ErrUnsupported`；窗口绑定只在 Windows 生效。实验查询无稳定兼容保证，不用版本字符串猜测能力。

## 生命周期与并发

- 每进程仅一个 Runtime、一个活动实例。预占后初始化失败或关闭会终止本进程的 binding 使用；不增加 Dlclose、热重载或同进程重试。
- `Runtime`、`Instance`、`Operation` 只经构造返回的指针使用，不按值复制。新增 API 复用既有输入快照与输出复制，借用的原生指针及时有界复制，不泄露内存视图。
- 业务调用先取得 lease，再排队进入 raw 串行门；lease 覆盖等待、原生调用、复制和结果登记。状态锁不跨原生调用或等待持有，遵守 Runtime 状态门 → Instance 状态门的锁序。
- callback 先取得准入再读取原生内存；native 侧 customArg 传整数 token，不能传 Go 对象指针。token 不复用，关闭 callback 准入并等待在途 callback 排空后才注销。
- 创建回滚与正常关闭共用单次清理路径，只有唯一关闭者调用 Stop/Destroy。修改顺序时保留 callback 排空、Operation 结算、最终错误稳定、Events 关闭及分发器退出完成后再通知 Done 的保证。
- `Operation.Wait(ctx)` 取消只终止该次等待，不撤销动作、不提前释放 pending 槽位；已完成结果可重复读取。binding 的 Close 可能被原生调用卡住，不在包内伪造强制回收完成。
- callback/分发器不得等待读者。所有缓存、扫描、队列和诊断保留预算，溢出显式故障，不静默截断或丢关键事件。
- 输出缓冲区沿用有界探测：真实非空容量，先判 NullSize，再判实际容量与 Go int 上限；Tasks 容量以 int32 元素计。PNG/BGR 只读缓存，不隐式发起截图。

## 修改与验证

- Go 状态与故障注入用例复用已有 fake 环境、局部生命周期门和调用接口；不要为方便测试重置生产进程级状态或打开逃逸配置。
- 真实 FFI 用例使用 `internal/ffitest` 的新子进程和外部超时。loader fixture 的空导出仅用于装载/注册，绝不能调用；ABI 与 callback 行为使用各自签名正确的 C 夹具。
- 签名、符号策略或 checker 修改先跑 `static`，再按影响选 `loader`、`abi`、`callback`；这些均是 `go run ./tools/maa-go-check --layers <layer>` 的层名。增加验证用例时确认对应层确实选中它们。
- 并发修改追加 `go run ./tools/maa-go-check --layers race`；需要 C 工具链和 cgo race 运行时，该层排除真实 Core。
- 升级 Core/profile 时同步锁定头、`inputs.json`、checker profile/期望表与夹具，再运行 `go run ./tools/maa-go-check --layers core` 验证真实库。真实库依赖锁定发行包及其完整原生依赖，不能只复制主库或跳过摘要验证。
- 分层工具二进制退出 1 表示 FAIL、2 表示 UNVERIFIED；需要区分时先构建工具再执行，`go run` 不保留这两种 shell 退出码的区别。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [公开包契约](doc.go) | 修改公开 API、输入/输出、并发、取消和关闭行为前读取 |
| [包说明](README.md) | 核对部署依赖、支持矩阵和升级复验流程时读取；公开能力同时与源码核对 |
| [夹具与真实库说明](testdata/README.md) | 编写 FFI 用例、准备 C 工具链或真实发行包时读取 |
| [分层工具说明](../../tools/maa-go-check/README.md) | 选择层、处理 skip/退出码或修改检查入口时读取 |
| [声明 checker 说明](../../tools/check-maa-api/README.md) | 修改 ABI 声明或 checker 时读取输入语法、目标与失败规则 |
| [锁定输入说明](../../tools/check-maa-api/testdata/locked/README.md) | 更新上游版本、头文件、摘要或许可证时读取 |
