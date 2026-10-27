# maa-go

`maa-go` 是 MaaCore C ABI 的 Go 薄绑定，Go 包名为 `maa`。它在运行时
经 purego 动态加载 `libMaaCore.so`，不把 MaaCore 链接进 Go 二进制，
生产 Go 包可以 `CGO_ENABLED=0` 构建。

**首版支持声明（2026-10-27）**：在 Linux amd64 / glibc 上，本包对下方
“公开面”列出的 API 提供支持。其他平台、未列出的能力与所有未通过分层
检查的层均未验证；未验证不等于支持。

## 支持矩阵

| 平台 | 架构 | 运行环境 | 结论 |
|---|---|---|---|
| Linux | amd64 | glibc 2.43（Fedora 44）实测 | **支持**（本页公开面） |
| Linux | arm64 | 未执行 | 未验证；交叉编译成功不构成运行支持 |
| macOS | amd64/arm64 | 未执行 | 未验证 |
| Windows | amd64 | 只运行声明层 checker | 未验证运行支持；Windows 声明检查通过不代表可加载/调用 |

已实测组合（精确源码/摘要清单由锁定 checker 输入与真实库测试维护，
本页不重复验收摘要）：

- Go `1.26.9`，purego `v0.11.2`；
- MaaCore `v6.20.1`；锁定源码 commit 与三个头的 SHA-256 见
  `tools/check-maa-api/testdata/locked/README.md`，整包与成员摘要由
  真实库测试在运行时钉扎；
- 发行包 `MAA-v6.20.1-linux-x86_64.tar.gz`；
- 同一发行包内的 `resource/` 资源；`resource/version.json` 的
  `last_updated` 为 `2026-10-20 00:59:49.000`，其 SHA-256 由真实库测试
  的锁定常量校验，具体摘要不在此重复（验收摘要按归属规则维护）；
- glibc 2.43（Fedora 44）；
- ADB：Android SDK platform-tools `37.0.1-15733141`（`adb version`
  1.0.41）；
- 测试夹具与 race 层另需 C 编译器（本轮为 cc/GCC 16.2.1）：前者编译
  测试用 `.so`，后者是 `-race` 所需的 cgo 运行时；生产
  `CGO_ENABLED=0` 构建不使用它；
- `AsstGetNullSize()` 在锁定基线返回 `math.MaxUint64`。

本组合上实际执行并通过的层包括：静态 checker 双目标、Go 故障注入、
loader fixture、真实 ABI fixture、原生线程 callback fixture、真实库
无设备 M1/M3 链路，以及显式设备的 Depot precondition/success/stop
（其中 precondition 含 Connect/Screencap）。各层由
`go run ./tools/maa-go-check --layers …` 独立报告；没有运行的层保持
未验证。

## 公开面

| 对象 | 方法 / 类型 |
|---|---|
| Runtime | `Open(RuntimeConfig)`、`Version()`、`Capabilities()`、`NullSize()`、`Log(level, message)`、`NewInstance(InstanceConfig)`、`Close()` |
| Instance | `Close()`、`AppendTask(taskType, params)`、`SetTaskParams(id, params)`、`Start()`、`Stop()`、`BackToHome()`、`Running()`、`Connected()`、`Connect(ConnectConfig)`、`Click(x, y)`、`Screencap()`、`GetImagePNG()`、`UUID()`、`Tasks()` |
| Operation | `ID()`、`Wait(ctx)` |
| 数据与配置 | `RuntimeConfig`、`InstanceConfig`、`ConnectConfig`、`Limits`、`BufferLimits`、`StaticOption`、`InstanceOption`、`ConnectionExtra`、`Event`、`AsyncResult`、`TaskID`/`AsyncID`/`MessageID`、状态哨兵错误与加载/符号/注册错误类型 |

C 侧的 `AsstConnect` 与 `AsstAttachWindow` 是 deprecated 对照项，不提供
公开方法；`AsstCallerExtra.h` 的实验接口不在首期范围。`AsstGetImageBgr`
只报告 `Capabilities().ImageBgr()`，不提供读取方法，也不分配生产 BGR
缓冲区。

**已知缺口：公开事件流 API。** 本版没有公开 `Events()`、`Done()`、
`Err()`、`Fault()`、`Diagnostics()`。`Event`/`AsyncResult` 数据类型与
内部有界分发器已经实现并有测试，但没有接线到公开方法；调用方不能从
公开面读取事件流或 `Done`/`Err`。接入后的契约是单消费者：`Events`
通道只能由一个读取者推进，分发器不等待读者，`Close` 也不依赖读者
存活。该缺口不影响下方已列出的同步/异步/读取 API，但不能被描述为
“事件流已交付”。

## 原生依赖与部署

`CGO_ENABLED=0` 只描述 Go 构建方式，**不**表示 MaaCore 没有原生依赖：

- 运行时仍需显式配置的 `libMaaCore.so`、至少一个包含 `resource/` 的
  资源根、可写的用户目录与 ADB 可执行文件；
- 发行包根目录的 9 个 `.so` 必须放在同一目录并一起部署：
  `libMaaCore.so` 与 8 个捆绑依赖 `libMaaAdbControlUnit.so`、
  `libMaaUtils.so`、`libfastdeploy_ppocr.so`、`libonnxruntime.so.1`、
  `libunwind.so.1`、`libopencv_world4.so.412`、`libc++.so.1`、
  `libc++abi.so.1`。`libMaaCore.so` 的 `DT_RUNPATH=$ORIGIN` 依赖同目录
  解析；只复制 `libMaaCore.so` 会在 dlopen 时失败；
- MaaCore 自己依赖系统库（本基线为 glibc 2.43）；不保证 musl、静态
  单文件、无系统动态库或 Wails desktop 免 cgo；这些组合需要各自验收；
- 原生库与资源不随本仓库分发，也不在 `go test` 中打包；真实库测试从
  锁定发行包缓存读取并校验摘要，缺失时明确报告受阻，不冒充通过；
- 连接参数（ADB 路径、地址、profile）由调用方提供；本包不做设备发现、
  不做 shell 展开、不记录设备地址。

每进程只支持一个 Runtime 和一个活动实例。`Open` 预占生命周期后，库
加载/符号解析/初始化失败都会终止本进程的 binding 使用；没有 Dlclose、
热重载或同进程重试，恢复只能退出进程并由 supervisor 启动新 worker。
worker/control、IPC、HTTP、持久化、Web、Wails 与 CLI 不在本包范围内，
本页不把它们列为已交付。

## 生命周期、并发与所有权

本节的完整代码级契约见 `doc.go`；这里给出调用方必须遵守的边界。

- **只通过指针使用，不按值复制。** `Runtime`、`Instance`、`Operation` 只能使用
  构造函数返回的指针；零值不可用。不要按值复制、不要从字段重建、不要
  在多个所有者之间散布副本；复制会复制内部状态/通道引用，破坏单次
  关闭与并发准入。
- **快照所有权。** `Open`/`NewInstance` 在方法返回后冻结配置的深拷贝，
  调用方随后修改原 slice/map/`RawMessage` 不影响已创建对象。方法返回
  前调用方不得并发修改输入。`Event`、`AsyncResult`、PNG、UUID 与任务
  列表都是独立 Go 副本，可长期持有和修改，不影响后续调用、其他副本或
  内部缓存；实例关闭后副本仍有效。
- **并发。** `Runtime` 的只读元数据方法并发安全；`Open` 由进程级生命
  周期门串行化；`Runtime.Close` 与 `Instance.Close` 可并发，只有一个
  关闭所有者执行原生 Stop/Destroy，其余返回同一最终结果。`Instance`
  的业务方法可并发调用，受实例调用准入与原生调用串行门禁约束；
  `Operation.Wait` 可重复、可并发。
- **取消。** `Wait(ctx)` 的超时/取消只结束本次等待，返回 `ctx.Err()`
  与零值结果；它不改写 Operation 终态、不撤销已提交的原生动作。之后
  仍可再次 `Wait` 取得真实结果。已完成的 Operation 即使 ctx 已取消也
  返回终态副本。`Wait(nil)` 返回 `ErrInvalidInput`。C API 没有按异步
  ID 取消的接口，`Stop()` 也不等于取消异步操作队列；取消不会提前回收
  `PendingOperations` 槽位。
- **关闭可能卡住。** `Close()` 不接受强制回收超时，也不承诺在原生
  `Stop`/`Destroy` 卡住时返回。binding 不卸载库、不杀死原生线程、不
  伪造“已回收”；外部 worker supervisor 必须在期限后终止整个进程。
  这适用于 `Instance.Close()` 与 `Runtime.Close()`，也适用于原生
  callback 卡住的情况。
- **错误。** 用 `errors.Is`/`errors.As` 判别，不依赖文本匹配。
  C `bool=false` 通常没有结构化原因，绑定返回 `ErrNativeRejected`，
  不编造 LastError；C `void` 返回（Destroy、SetConnectionExtras、
  AsstLog）只证明调用已返回，不提供成功码。输出缓冲区的 `NullSize`
  不是“所需容量”；耗尽返回 `ErrBufferLimit`，超过实际容量返回
  `ErrProtocol`，合法 0 长度返回空结果。加载、符号查找与注册错误分别
  保留 `LibraryLoadError`、`SymbolLookupError`、`FunctionRegistrationError`
  与可遍历的 `AggregateError`。

## 分层检查

```sh
# CI 与离线默认集合：静态 checker、Go 故障注入、设备门、loader/ABI/
# callback fixture、CGO_ENABLED=0 build/vet 与 gofmt
go run ./tools/maa-go-check --layers ci

# 真实 MaaCore 无设备链路（需要锁定发行包）
go run ./tools/maa-go-check --layers core

# 显式设备场景（仓库外配置；没有配置不会运行）
MAA_M4_DEVICE_CONFIG=/absolute/path/device-acceptance.json \
  go run ./tools/maa-go-check --layers device

# 全部层；任何缺少资源/授权/工具链的层都会使整体非零退出，而不是被算作通过
go run ./tools/maa-go-check --layers all

# race 使用 -race 与 CGO_ENABLED=1，单独记录，不代表生产构建需要 cgo
go run ./tools/maa-go-check --layers race
```

以上 `go run` 示例在 shell 中把任何非零退出统一显示为 1；需要精确区分
`UNVERIFIED`（二进制退出 2）与 `FAIL`（1）时，先
`go build -o maa-go-check ./tools/maa-go-check` 再运行二进制。CI 使用
构建后的二进制，因此保留 2/1 的区分。

CI 工作流 `.github/workflows/maa-go.yml` 只运行 `ci` 与 `race` 两层；它
不运行 `core`/`device`，也不把任何跳过算作通过。

状态含义：`PASS` 表示该层所有检查实际执行并通过；`PASS (expected skips)`
表示该层只有明确声明的预期跳过（例如设备门默认关闭），跳过的测试
没有验证任何行为，详情会列出；`UNVERIFIED` 表示跳过、匹配不到测试或
缺少外部资源，**不是通过**，二进制退出 2（`go run` 统一显示为 1）；
`FAIL` 退出 1；`NOT SELECTED` 未运行、不做声明。空的 `-run` 选择也是
`UNVERIFIED`。

分层不能互相借结论：静态 checker 只证明声明层；loader fixture 只证明
装载/解析/注册，空导出绝不可调用；ABI/callback fixture 只证明手写 C
签名与调用约定；真实库测试证明锁定 MaaCore 的初始化/生命周期；设备
测试证明指定设备与许可范围内的连接/截图/识别。任何一层没有运行，都
不能由另一层替代。

## 设备验收事实

2026-10-27 在 Linux amd64、一台 TCP ADB 设备、TouchMode=adb 上，显式
许可的 Depot 场景真实执行通过：

- precondition：Connect 与 Screencap 各一次（ID 1/2，1280x720），连接
  后 `Connected()==true`；
- success：同一未关闭实例两轮 Depot（TaskID 1/2），任务消息
  （`TaskChainCompleted`、`AllTasksCompleted`、`DepotInfo`）按 TaskID
  关联；
- stop：`TaskChainStopped` 且没有 `AllTasksCompleted`。

设备配置、截图与原始日志保存在仓库外，不进入提交；设备地址不在任何
公开文件中。

允许清单登记 Depot 与入口步骤 StartUp；本轮实际提交并完成的识别
任务只有 Depot。Entry（StartUp）场景在早前一次尝试中以 Go 测试 10
分钟超时（`panic: test timed out after 10m0s`）结束，没有 entry 证据，
不计为通过；最终运行在游戏已处于主界面时按授权没有提交 StartUp，
因此 StartUp 成功路径未复验。该结论的边界：只覆盖这一台设备与 ADB
触摸路径；minitouch/maatouch 未验收；StartUp 成功路径未复验；设备层
不在 CI 中运行，必须显式启用。

## 已知限制

- 公开事件流 API（Events/Done/Err/Fault/Diagnostics）未接线；见上文。
- BGR 读取未公开；只有能力探测。
- 仅 Linux amd64/glibc；musl、静态单文件、macOS、Windows 运行未验证。
- 每进程单 Runtime、单活动实例；无 Dlclose、热重载、同进程重建。
- `Close` 在原生卡住时可能阻塞；没有自设回收超时，依赖外部 supervisor。
- 设备验收范围见上一节；不自动导航、不重放未知结果、不执行未授权任务。
- 任务参数与消息按原文透传；binding 不提供任务类型封闭枚举或培养业务
  模型，未知字段/大整数不被丢弃或改写。

未验证项的去向：公开事件流与 BGR 读取必须作为新的公开面单独接线并
按同一分层门槛验收；macOS/Windows/arm64 需要各自完成 loader、
callback、真实库与设备门槛；设备侧 minitouch/maatouch 与 StartUp
成功路径需在显式授权下另行验收。在这些完成前不更新支持矩阵。

## 版本升级复验

升级 MaaCore/purego/Go 或更换资源包时，按顺序执行并在同一组合上记录
OS/架构、Go、purego、Core 文件摘要、源码 commit 与资源版本：

1. 更新 `tools/check-maa-api` 的锁定 profile、内部期望表与
   `tools/check-maa-api/testdata/locked` 的头文件/`inputs.json`，先跑
   `--layers static`（linux 与 windows 两个目标都必须通过）。
2. 若签名/常量变化，同步 fixture 与 raw 声明；跑 `--layers loader`、
   `--layers abi`、`--layers callback`。
3. 用新发行包跑 `--layers core`，确认真实 Version/NullSize、初始化
   事务、无设备链路与退出。
4. 在显式许可下跑 `--layers device`；没有设备/许可时保持未验证，不得
   引用旧组合的设备结果。
5. 跑 `--layers build`、`--layers fmt`、`--layers race` 与
   `--layers all`；全绿后才更新本页支持矩阵。
6. 任何跳过的层保持“未验证”；升级不能把旧平台的旧证据直接继承。

本包不以版本字符串推断兼容性；符号集合、checker profile、fixture 与
真实库结果必须对同一锁定组合成立。

## 相关文档

- `doc.go`：公开生命周期、并发、所有权、取消、错误与部署边界。
- `tools/check-maa-api/README.md`：静态声明 checker 的输入、输出、
  失败规则与反例覆盖。
- `tools/check-maa-api/testdata/locked/README.md`：仓库内锁定头的
  来源、摘要与许可证。
- `tools/maa-go-check/README.md`：分层入口、状态与退出码。
- `packages/maa-go/testdata/README.md`：C fixture、真实库与设备
  验收的使用方式。
