# maa-go

`maa-go` 是 MaaCore C ABI 的 Go 薄绑定，Go 包名为 `maa`。它在运行时
动态加载 `libMaaCore.so`（Linux）、`libMaaCore.dylib`（macOS）或
`MaaCore.dll`（Windows），不把 MaaCore 链接进 Go 二进制，
生产 Go 包可以 `CGO_ENABLED=0` 构建。

**支持声明**：在 Linux amd64 / glibc 上，本包对下方“公开面”列出的 API
提供支持。其他平台、未列出的能力与所有未通过分层检查的层均未验证；
未验证不等于支持。

## 支持矩阵

| 平台 | 架构 | 运行环境 | 结论 |
|---|---|---|---|
| Linux | amd64 | glibc | **支持**（本页公开面） |
| Linux | arm64 | CI 仅交叉编译包与测试二进制 | 未执行单测或真实 Core |
| macOS | arm64 | CI 执行 Go/fake-native 与原生 loader 测试 | loader 已实现；结果以 CI run 为准 |
| macOS | amd64 | CI 执行 Go/fake-native 与原生 loader 测试 | loader 已实现；结果以 CI run 为准 |
| Windows | amd64 | CI 执行 Go/fake-native 与原生 loader 测试 | loader 已实现；结果以 CI run 为准 |
| Windows | arm64 | CI 仅交叉编译包与测试二进制 | loader 已实现；未执行单测或真实 Core |

上述 CI 条目描述[工作流配置](../../.github/workflows/maa-go.yml)；具体
执行结果以对应 CI run 为准。交叉编译只验证编译兼容，不计为实际单测
执行或真实 MaaCore 运行支持。原生 loader 与夹具测试也不替代真实
MaaCore 运行验证。

## ABI 与依赖

Go 工具链与 purego 版本见 [go.mod](../../go.mod)。当前 ABI profile
锁定 MaaCore `v6.20.1`；源码 commit、头文件摘要与许可证见
[锁定 C 输入](../../tools/check-maa-api/testdata/locked/README.md)。
该 profile 的 `AsstGetNullSize()` 为 `math.MaxUint64`。

可选的真实库测试使用 `MAA-v6.20.1-linux-x86_64.tar.gz` 及其中的
`resource/`，在运行时校验整包与成员摘要；缓存路径及运行命令见
[测试夹具说明](testdata/README.md)。测试夹具与 race 层另需 C 编译器：
前者按系统生成 `.so`、`.dylib` 或 `.dll`，后者使用 `-race` 所需的
cgo 运行时；生产 `CGO_ENABLED=0` 构建不使用它。

仓库内可重复执行的分层检查包括：静态 checker 双目标、Go 故障注入、
loader fixture、真实 ABI fixture、原生线程 callback fixture，以及真实库
无设备链路。各层由 `go run ./tools/maa-go-check --layers …` 独立报告；
没有运行的层保持未验证。

## 公开面

| 对象 | 方法 / 类型 |
|---|---|
| Runtime | `Open(RuntimeConfig)`、`Version()`、`Capabilities()`、`NullSize()`、`Log(level, message)`、`NewInstance(InstanceConfig)`、`Close()` |
| Instance | `Close()`、`Events()`、`Done()`、`Err()`、`Fault()`、`Diagnostics()`、`AppendTask(taskType, params)`、`SetTaskParams(id, params)`、`Start()`、`Stop()`、`BackToHome()`、`Running()`、`Connected()`、`Connect(ConnectConfig)`、`Click(x, y)`、`Screencap()`、`GetImagePNG()`、`UUID()`、`Tasks()` |
| Operation | `ID()`、`Wait(ctx)` |
| 数据与配置 | `RuntimeConfig`、`InstanceConfig`、`ConnectConfig`、`Limits`、`BufferLimits`、`StaticOption`、`InstanceOption`、`ConnectionExtra`、`Event`、`AsyncResult`、`InstanceDiagnostics`、`TaskID`/`AsyncID`/`MessageID`、状态哨兵错误与加载/符号/注册错误类型 |

C 侧的 `AsstConnect` 与 `AsstAttachWindow` 是 deprecated 对照项，不提供
公开方法；`AsstCallerExtra.h` 的实验接口不在本包公开范围。`AsstGetImageBgr`
只报告 `Capabilities().ImageBgr()`，不提供读取方法，也不分配生产 BGR
缓冲区。

`Events()` 返回同一条单消费者通道，只有一个读取者推进消费；未知合法
消息按原文交付，`Event.JSON` 是可安全保留和修改的独立副本。任务完成
由 Core 的事件表达，`Done()` 只表示实例关闭完成。

分发器不等待读者，`Close()` 也不依赖读者存活。事件溢出或协议错误会
使实例故障，关闭独立的 `Fault()` 通知；`Err()` 立即可见首因，未完成的
Operation 结为该故障，已经确认的结果保持不变。正常关闭以及仅关闭
Stop 被拒绝时不会触发 `Fault()`，观察者应同时等待 `Done()`。

`Diagnostics()` 返回 `InstanceDiagnostics`：饱和的 `LostEvents`、只读
`FirstFault`、至多 4096 字节的独立 `RawPrefix` 和只读 `StopDiagnostic`。
关闭 Stop 被拒绝时，诊断在 Destroy 返回前即可见。`RawPrefix` 可能是
不完整 JSON 或无效 UTF-8；修改它不影响后续快照。

观察到 `Done()` 或任意 `Close()` 返回后，Events 已关闭、分发器已退出、
全部 callback 已退出、Operation 已结算，`Err()` 与诊断保持稳定。
`Err()` 返回与 `Close()` 相同的最终错误；已缓冲事件在关闭后仍可读取。

例如，由一个读取者消费事件，同时由实例所有者决定何时关闭：

```go
func observe(in *maa.Instance, handleEvent func(maa.Event)) error {
    for {
        select {
        case event, ok := <-in.Events():
            if !ok {
                <-in.Done()
                return in.Err()
            }
            handleEvent(event)
        case <-in.Fault():
            return in.Close()
        case <-in.Done():
            return in.Err()
        }
    }
}
```

此例在故障或关闭时退出；若需处理全部缓冲事件，关闭后继续遍历
`Events()` 至通道排空。`handleEvent` 应及时返回，避免阻塞读取者自身。

## 原生依赖与部署

`CGO_ENABLED=0` 只描述 Go 构建方式，**不**表示 MaaCore 没有原生依赖：

- 运行时仍需显式配置的 MaaCore 库绝对路径、至少一个包含 `resource/` 的
  资源根、可写的用户目录与 ADB 可执行文件；
- Linux 锁定发行包根目录的 9 个 `.so` 必须放在同一目录并一起部署：
  `libMaaCore.so` 与 8 个捆绑依赖 `libMaaAdbControlUnit.so`、
  `libMaaUtils.so`、`libfastdeploy_ppocr.so`、`libonnxruntime.so.1`、
  `libunwind.so.1`、`libopencv_world4.so.412`、`libc++.so.1`、
  `libc++abi.so.1`。`libMaaCore.so` 的 `DT_RUNPATH=$ORIGIN` 依赖同目录
  解析；只复制 `libMaaCore.so` 会在 dlopen 时失败；
- MaaCore 自己依赖系统库，Linux 运行环境需与发行包要求的 glibc
  兼容；不保证 musl、静态单文件或无系统动态库部署；
- 原生库与资源不随本仓库分发，也不在 `go test` 中打包；真实库测试从
  锁定发行包缓存读取并校验摘要，缺失时明确报告受阻，不冒充通过；
- 连接参数（ADB 路径、地址、profile）由调用方提供；本包不做设备发现、
  不做 shell 展开、不记录设备地址。

Linux/macOS 使用 `dlopen(RTLD_NOW|RTLD_LOCAL)` 与 `dlsym`，依赖由
系统 loader 根据库内的 `$ORIGIN`、`@loader_path`/`@rpath` 等信息解析。
Windows 使用 `LoadLibraryExW` 与 `GetProcAddress`，依赖搜索范围为
目标 DLL 所在目录与 System32；将发行包的依赖 DLL 一起部署到库目录，
不依赖当前工作目录或 PATH。本包不修改进程级搜索路径、不预加载依赖。
Windows 路径以 UTF-16 传给系统 API，调用方仍提供 UTF-8 Go 字符串。

每进程只支持一个 Runtime 和一个活动实例。`Open` 预占生命周期后，库
加载/符号解析/初始化失败都会终止本进程的 binding 使用；没有 Dlclose、
热重载或同进程重试，恢复只能退出进程并由 supervisor 启动新 worker。

## 生命周期、并发与所有权

本节的完整代码级契约见 [doc.go](doc.go)；这里给出调用方必须遵守的边界。

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
# CI 与离线默认集合：静态 checker、Go 故障注入、loader/ABI/
# callback fixture、CGO_ENABLED=0 build/vet 与 gofmt
go run ./tools/maa-go-check --layers ci

# Windows/macOS CI 单测集合（不选择 Linux 原生 fixture）
go run ./tools/maa-go-check --layers static,go,build,fmt

# 真实 MaaCore 无设备链路（可选，需要锁定发行包，仅本地运行）
go run ./tools/maa-go-check --layers core

# 全部层；任何缺少资源/工具链的层都会使整体非零退出，而不是被算作通过
go run ./tools/maa-go-check --layers all

# race 使用 -race 与 CGO_ENABLED=1，只跑 Go 并发/单元测试
# （-skip TestRealCore；真实核心由 core 层单独覆盖）
go run ./tools/maa-go-check --layers race
```

以上 `go run` 示例在 shell 中把任何非零退出统一显示为 1；需要精确区分
`UNVERIFIED`（二进制退出 2）与 `FAIL`（1）时，先
`go build -o maa-go-check ./tools/maa-go-check` 再运行二进制。CI 使用
构建后的二进制，因此保留 2/1 的区分。

CI [工作流](../../.github/workflows/maa-go.yml)在 Linux amd64 运行 `ci` 与
独立 `race`；Windows amd64、macOS 两种架构运行 `static,go,build,fmt`。
Go 层包含真实系统库与小型原生依赖夹具的加载、函数调用和回调测试。
Linux、Windows、macOS × amd64、arm64 的全部 6 个组合另做交叉编译，
构建全部包与测试二进制；Linux/Windows arm64 不执行测试。
工作流不运行 `core`，也不把任何跳过算作通过。`race` 层以 `-skip` 排除
`TestRealCore`（真实核心），被排除的测试不会出现在 `go test -json` 的
skip 事件中、因此不计入 `Skipped`；详情与 JSON 的 `skip_filter` 会写明
排除模式，真实核心由 `core` 层单独覆盖。

状态含义：`PASS` 表示该层所有检查实际执行并通过；`PASS (expected skips)`
表示该层只有明确声明的预期跳过，跳过的测试没有验证任何行为，详情会
列出；`UNVERIFIED` 表示跳过、匹配不到测试或缺少外部资源，**不是通过**，
二进制退出 2（`go run` 统一显示为 1）；`FAIL` 退出 1；`NOT SELECTED`
未运行、不做声明。空的 `-run` 选择也是 `UNVERIFIED`。

分层不能互相借结论：静态 checker 只证明声明层；loader fixture 只证明
装载/解析/注册，空导出绝不可调用；ABI/callback fixture 只证明手写 C
签名与调用约定；真实库测试证明锁定 MaaCore 的初始化/生命周期；`race`
层只跑 Go 并发/单元测试（`-skip TestRealCore`），不证明真实核心行为。
任何一层没有运行，都不能由另一层替代。

## 已知限制

- BGR 读取未公开；只有能力探测。
- 真实 MaaCore 运行支持仅限 Linux amd64/glibc；macOS、Windows 的
  测试覆盖原生 loader 与夹具，真实 MaaCore 运行未验证；musl 与静态
  单文件部署未验证。
- 每进程单 Runtime、单活动实例；无 Dlclose、热重载、同进程重建。
- `Close` 在原生卡住时可能阻塞；没有自设回收超时，依赖外部 supervisor。
- 仓库内测试不连接真机；真机连接、截图与任务执行不在自动验收范围内。
- 任务参数与消息按原文透传；binding 不提供任务类型封闭枚举或培养业务
  模型，未知字段/大整数不被丢弃或改写。

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
4. 跑 `--layers build`、`--layers fmt`、`--layers race` 与
   `--layers all`；全绿后才更新本页支持矩阵。
5. 任何跳过的层保持“未验证”；升级不能把旧平台的旧证据直接继承。

本包不以版本字符串推断兼容性；符号集合、checker profile、fixture 与
真实库结果必须对同一锁定组合成立。

## 相关文档

- [包文档](doc.go)：公开生命周期、并发、所有权、取消、错误与部署边界。
- [check-maa-api](../../tools/check-maa-api/README.md)：静态声明 checker 的输入、输出、
  失败规则与反例覆盖。
- [锁定 C 输入](../../tools/check-maa-api/testdata/locked/README.md)：仓库内锁定头的
  来源、摘要与许可证。
- [maa-go-check](../../tools/maa-go-check/README.md)：分层入口、状态与退出码。
- [测试夹具](testdata/README.md)：C fixture 与真实库链路的
  使用方式。
