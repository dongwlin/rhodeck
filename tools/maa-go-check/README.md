# maa-go-check

`maa-go-check` 是 maa-go 的分层验收入口。它把静态声明检查、Go 故障
注入、loader/ABI/callback fixture 与真实 MaaCore 无设备链路分成独立的
层运行，并逐层报告 `PASS`、`UNVERIFIED`、`FAIL` 或 `NOT SELECTED`。
它存在的目的只有一个：让"跳过不是通过"成为可执行的退出码，而不是
报告里的一句话。

## 用法

```sh
# CI 与离线默认集合（需要 Go 与 C 编译器）
go run ./tools/maa-go-check --layers ci

# Windows/macOS 的单测集合（不选择尚未实现的原生 fixture）
go run ./tools/maa-go-check --layers static,go,build,fmt

# 单层
go run ./tools/maa-go-check --layers static
go run ./tools/maa-go-check --layers core
go run ./tools/maa-go-check --layers race

# 全部层（真实库层需要外部资源）
go run ./tools/maa-go-check --layers all

# 机器可读报告
go run ./tools/maa-go-check --layers ci --json

go run ./tools/maa-go-check --list
```

`--root` 默认为当前目录向上最近的 `go.mod` 所在目录。

需要精确区分退出码时先构建二进制：

```sh
go build -o maa-go-check ./tools/maa-go-check
./maa-go-check --layers ci
```

`go run` 会把任何非零退出统一显示为 shell 状态 1，无法区分
`UNVERIFIED`（2）与 `FAIL`（1）；CI 使用构建后的二进制。

## 层与选择

| 层 | 运行内容 | 外部要求 | CI |
|---|---|---|---|
| `static` | `tools/check-maa-api` 单测/反例 + 仓库内锁定 C 输入的 linux/windows 完整 checker | 无（头文件已随仓库提供） | 是 |
| `go` | `packages/maa-go` 单测与故障注入、`internal/ffitest`、`internal/native` 非 fixture 单测、本工具单测 | 无 | 是 |
| `loader` | 独立 C loader fixture 的装载/解析/注册 | Linux、`cc`/`gcc` | Linux |
| `abi` | 真实 C ABI fixture 的数值、参数、void 与缓冲区调用 | Linux、`cc`/`gcc` | Linux |
| `callback` | 原生线程 callback fixture | Linux、`cc`/`gcc` | Linux |
| `core` | 锁定 MaaCore v6.20.1 的真实库无设备链路 | 锁定发行包 | 否 |
| `build` | `CGO_ENABLED=0 go build ./...` 与 `go vet ./...` | 无 | 是 |
| `fmt` | `gofmt -l .`，有输出即失败 | 无 | 是 |
| `race` | `go test ./packages/maa-go/... -race`，另以 `-skip` 排除 TestRealCore（Go 并发/单元测试；真实核心由 core 覆盖） | `cc`/`gcc`（race 需要 cgo） | 独立任务 |

`race` 层由 `raceGoTestStep` 固定携带 `Race: true`，`goTestArgs` 因此
追加 `-race`；同时固定 `-skip 'TestRealCore'`，只跑 Go 并发/单元测试。
被 `-skip` 排除的 TestRealCore 是真实库子进程验收，不属于 Go 并发
检查，且 CI 没有发行包；它不会出现在 `go test -json` 的 skip 事件里，
所以该层 `Skipped=0`，报告的 `skip_filter` 字段与详情会写明排除模式。
真实核心生命周期由 `core` 层单独覆盖。
`TestGoTestArgsIncludesRaceOnlyWhenRequested` 同时钉住 `-race` 与该
`-skip` 值。

`--layers ci` 是 `static,go,loader,abi,callback,build,fmt`。
`--layers all` 包含 `core` 与 `race`；缺少发行包或编译器时对应层为
`UNVERIFIED`，二进制整体退出码 2（`go run` 统一显示为 1）。CI 工作流
`.github/workflows/maa-go.yml` 使用以下矩阵，**不运行也不声称** `core`：

| Runner | 实际运行平台 | 层 |
|---|---|---|
| `ubuntu-24.04` | Linux amd64 | `ci`、独立 `race` |
| `windows-2025` | Windows amd64 | `static,go,build,fmt` |
| `macos-15` | macOS arm64 | `static,go,build,fmt` |

其余 `linux/arm64`、`windows/arm64`、`darwin/amd64` 组合由独立任务
交叉编译全部包与测试二进制，不执行测试。测试编译通过只证明编译兼容，
不计为对应架构的单测通过。

矩阵在各平台实际执行 Go 单测、故障注入与构建检查；Linux 额外执行
loader/ABI/callback fixture。`static` 在所有 runner 上仍检查锁定的
`linux/amd64`、`windows/amd64` 两个声明目标，不把宿主架构当成新的
ABI 基线。Windows/macOS 的生产 loader 仍返回 `ErrUnsupported`，
这些平台的单测通过不代表已能加载 MaaCore。真实 Core、设备与 Docker
运行不属于 CI 单测验收，后续运行问题按用户反馈处理。

## 状态与退出码

| 状态 | 含义 | 二进制退出码 |
|---|---|---|
| `PASS` | 所有选中的检查实际执行并通过 | 0 |
| `PASS (expected skips)` | 层内只有明确声明的预期跳过；跳过的测试没有验证任何行为，详情会列出 | 0 |
| `UNVERIFIED` | 选中的层被跳过、匹配不到测试、或缺少外部资源；**不是通过** | 2 |
| `FAIL` | 有测试或命令失败；`FAIL` 优先于 `UNVERIFIED` | 1 |
| `NOT SELECTED` | 本次未选择，未运行，不做任何声明 | — |

`go run` 对任何非零退出统一返回 1；上表是构建后二进制的退出码。CI
使用二进制，因此 UNVERIFIED 会让 job 以 2 失败，而不是与 FAIL 混为 1。

空测试选择也算 `UNVERIFIED`：把 `-run` 写错导致一个测试都没跑，不会
显示成绿色。`go test -json` 的顶层与子测试跳过都被记录；子测试跳过不
会躲在通过的父测试后面。

## 外部资源

- **C 编译器**：loader/ABI/callback fixture 与 race 层需要 `cc` 或
  `gcc`。缺失时这些层 `UNVERIFIED`，二进制退出码 2。
- **锁定 MaaCore**：`core` 层使用
  `~/.cache/maa/MAA-v6.20.1-linux-x86_64.tar.gz`，可用
  `MAA_CORE_TARBALL` 覆盖；测试会校验整包与成员 SHA-256。缺失时该层
  `UNVERIFIED`。

## 报告

默认输出逐层表格、环境摘要与结论。环境摘要只包含 Go/OS/架构、purego
版本、默认 cgo 设置、C 编译器、锁定发行包 SHA-256 前缀与 git 提交；
不包含仓库根、用户目录或外部资源路径。`--json` 输出同样的信息，便于
CI 归档。

`race` 层显式使用 `CGO_ENABLED=1`，与生产 `CGO_ENABLED=0` 构建分开
记录。race 通过只辅助发现 Go 并发问题，不证明原生 ABI；fixture 通过
也不证明真实 MaaCore 行为。分层报告的意义就是不让某一层的结论借给
另一层。
