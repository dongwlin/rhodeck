# maa-go

`maa-go` 是 MaaCore C ABI 的 Go 薄绑定，Go 包名为 `maa`。它在运行时
动态加载 `libMaaCore.so`（Linux）、`libMaaCore.dylib`（macOS）或
`MaaCore.dll`（Windows），不把 MaaCore 链接进 Go 二进制，
生产 Go 包可以 `CGO_ENABLED=0` 构建。

## ABI 与依赖

Go 工具链与 purego 版本见 [go.mod](../../go.mod)。绑定跟随调用方提供的
MaaCore 头文件与安装组合，不固定发行版本，也不保证旧版本兼容。
静态 checker 独立比较 C 头与 Go raw 声明；仓库内未版本化的
[头文件快照](../../tools/check-maa-api/testdata/snapshot/README.md)记录来源、
摘要与许可证，作为静态声明输入。
`AsstGetNullSize()` 的 `math.MaxUint64` 哨兵仍是输出缓冲区 ABI 约束。

安装的实际版本、库和资源版本文件摘要可通过
[CLI 安装元数据](../../apps/cli/installation-provenance.md)读取；资源来源与
摘要覆盖范围由安装宿主管理。真实库测试的输入与可选期望值见
[测试夹具说明](testdata/README.md)。

测试夹具与 race 层另需 C 编译器：前者按系统生成 `.so`、`.dylib` 或
`.dll`，后者使用 `-race` 所需的 cgo 运行时；生产 `CGO_ENABLED=0`
构建不使用它。

仓库内可重复执行的分层检查包括：静态 checker 双目标、Go 故障注入、
loader fixture、真实 ABI fixture、原生线程 callback fixture，以及真实库
无设备链路。各层由 `go run ./tools/maa-go-check --layers …` 独立运行和报告。

## 公开面

| 对象 | 方法 / 类型 |
|---|---|
| Runtime | `Open(RuntimeConfig)`、`Version()`、`Capabilities()`、`NullSize()`、`Log(level, message)`、`MapLevelKey(key)`、`ItemName(id)`、`NewInstance(InstanceConfig)`、`Close()` |
| Instance | `Close()`、`Events()`、`Done()`、`Err()`、`Fault()`、`Diagnostics()`、`AppendTask(taskType, params)`、`SetTaskParams(id, params)`、`Start()`、`Stop()`、`BackToHome()`、`Running()`、`Connected()`、`Connect(ConnectConfig)`、`Click(x, y)`、`Screencap()`、`AttachWindow(hwnd, screencap, mouse, keyboard)`（仅 Windows 生效）、`GetImagePNG()`、`GetImageBGR()`、`UUID()`、`Tasks()` |
| Operation | `ID()`、`Wait(ctx)` |
| 数据与配置 | `RuntimeConfig`、`InstanceConfig`、`ConnectConfig`、`Limits`、`BufferLimits`、`StaticOption`、`InstanceOption`、`ConnectionExtra`、`Event`、`AsyncResult`、`InstanceDiagnostics`、`MapLevelKey`、`Win32ScreencapMethod`、`Win32InputMethod`、`TaskID`/`AsyncID`/`MessageID`、状态哨兵错误与加载/符号/注册错误类型 |

C 侧的 `AsstConnect` 与 `AsstAttachWindow` 是 deprecated 对照项，不提供
公开方法。`AsstAsyncAttachWindow`（`AttachWindow`）只在 Windows 平台
生效：底层符号仅 Windows 参与查找，非 Windows 平台与能力缺席时返回
`ErrUnsupported`，不在其他系统上模拟成功。`AsstCallerExtra.h` 的两个
实验接口（`MapLevelKey`、`ItemName`）随 `ASST_WITH_EXTRA_CALLERS` 构建
选项导出（上游发行包默认仅 Apple 构建打开），能力缺席时同样返回
`ErrUnsupported`；实验接口无稳定兼容保证。`GetImageBGR` 读取可选的
`AsstGetImageBgr`：能力缺席时返回 `ErrUnsupported`，在场时按
`Limits.ImageBGR` 走与 `GetImagePNG` 相同的有界探测契约，返回原始 BGR
字节（通道序与步长由 Core 决定）。

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
- 将实际发行包要求的捆绑依赖与主库一起部署，保留其目录布局与库内
  RUNPATH；依赖集合随安装变化，不固定文件名或数量。只复制主库而
  遗漏依赖可能在 dlopen 时失败；
- MaaCore 自己依赖系统库，Linux 运行环境需与发行包要求的 glibc
  兼容；不保证 musl、静态单文件或无系统动态库部署；
- 原生库与资源不随本仓库分发，由安装宿主提供；
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

# 真实 MaaCore 无设备链路（可选，需显式安装或发行包，仅本地运行）
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

完整的选层、外部输入、报告状态和退出码见
[maa-go-check](../../tools/maa-go-check/README.md)；平台与构建矩阵见
[CI 工作流](../../.github/workflows/maa-go.yml)。

## 已知限制

- `GetImageBGR` 返回原始 BGR 字节，不公开宽、高与步长元数据，通道序与
  步长由 Core 决定；符号缺席时返回 `ErrUnsupported`，只有能力探测结果。
- 每进程单 Runtime、单活动实例；无 Dlclose、热重载、同进程重建。
- `Close` 在原生卡住时可能阻塞；没有自设回收超时，依赖外部 supervisor。
- 任务参数与消息按原文透传；binding 不提供任务类型封闭枚举或培养业务
  模型，未知字段/大整数不被丢弃或改写。

## 更换 MaaCore 安装

本包不以版本字符串推断兼容性。调用方提供库、完整原生依赖和资源；
必需符号缺失时加载失败，可选符号缺失时对应能力返回 `ErrUnsupported`。
版本字符串也不能证明某个任务或回调可用。

静态核对可通过 `maa-go-check --c-input DIR --layers static` 选择对应头文件。
头文件、宏或 ABI 形态改变时，需要同步绑定、独立期望表和夹具。
安装切换应在旧 worker 退出后进行；运行期间保持库、依赖和资源不变。

## 相关文档

- [包文档](doc.go)：公开生命周期、并发、所有权、取消、错误与部署边界。
- [check-maa-api](../../tools/check-maa-api/README.md)：静态声明 checker 的输入、输出、
  失败规则与反例覆盖。
- [C 头文件快照](../../tools/check-maa-api/testdata/snapshot/README.md)：仓库内头文件的
  来源、摘要与许可证。
- [maa-go-check](../../tools/maa-go-check/README.md)：分层入口、状态与退出码。
- [测试夹具](testdata/README.md)：C fixture 与真实库链路的
  使用方式。
