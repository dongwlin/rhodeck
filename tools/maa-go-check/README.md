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

# Windows/macOS 的单测集合（包含生产 loader 测试）
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
`--c-input DIR` 选择含来源与逐文件摘要的头文件目录；默认使用仓库的
`tools/check-maa-api/testdata/snapshot`。该目录是可更新的静态验证输入，
不限制宿主安装的 MaaCore 版本。显式输入的结构与摘要校验见
[checker 输入合同](../check-maa-api/README.md)。例如：

```sh
go run ./tools/maa-go-check --layers static --c-input /opt/maa-headers
```

缺少头文件 manifest 时 `static` 为 `UNVERIFIED`；头文件或声明不匹配
时为 `FAIL`。静态输入不可用不会因为工具单测通过而被算为通过。

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
| `static` | `tools/check-maa-api` 单测/反例 + 选定 C 输入的 linux/windows 完整 checker | `--c-input` 目录及 manifest（默认随仓库提供） | 是 |
| `go` | `packages/maa-go` 单测与故障注入、`internal/ffitest`、`internal/native` 单测（含生产 loader 测试，排除独立 fixture 层）、本工具单测 | loader 夹具需要 `cc`/`gcc` | 是 |
| `loader` | 独立 C loader fixture 的装载/解析/注册 | Linux、`cc`/`gcc` | Linux |
| `abi` | 真实 C ABI fixture 的数值、参数、void 与缓冲区调用 | Linux、`cc`/`gcc` | Linux |
| `callback` | 原生线程 callback fixture | Linux、`cc`/`gcc` | Linux |
| `core` | 显式实际安装的真实库无设备链路 | 库与资源根，或显式 tarball | 否 |
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
`--layers all` 包含 `core` 与 `race`；缺少实际安装输入或编译器时对应层为
`UNVERIFIED`，二进制整体退出码 2（`go run` 统一显示为 1）。
[CI 工作流](../../.github/workflows/maa-go.yml)使用以下矩阵，不运行 `core`：

| Runner | 实际运行平台 | 层 |
|---|---|---|
| `ubuntu-latest` | Linux amd64 | `ci`、独立 `race` |
| `windows-latest` | Windows amd64 | `static,go,build,fmt` |
| `macos-latest` | macOS arm64 | `static,go,build,fmt` |
| `macos-15-intel` | macOS amd64 | `static,go,build,fmt` |

独立交叉编译任务在 `ubuntu-latest` 上构建 `linux/windows/darwin` ×
`amd64/arm64` 的全部 6 个组合，编译全部包与测试二进制，不执行测试。
测试编译通过只证明编译兼容，不计为对应架构的单测通过。

矩阵在各平台实际执行 Go 单测、故障注入、生产 loader 测试与构建检查；
Linux 额外执行 loader/ABI/callback 独立 fixture 层。Windows 使用 runner
预装的 MinGW gcc，macOS 使用系统 C 编译器。`static` 在所有 runner 上检查选定头文件的
`linux/amd64`、`windows/amd64` 两个声明目标，不把宿主架构当成新的
声明目标。Windows/macOS 生产 loader 由本机原生库与夹具测试验证；
真实 MaaCore、设备与 Docker 运行不在上述 CI 测试覆盖范围内。

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

- **C 编译器**：生产 loader 夹具、loader/ABI/callback fixture 与 race 层需要 `cc` 或
  `gcc`。缺失时这些层 `UNVERIFIED`，二进制退出码 2。
- **实际 MaaCore**：`core` 层要求显式 `MAA_CORE_LIB_PATH` 与
  `MAA_CORE_RESOURCE_ROOT`，或互斥的 `MAA_CORE_TARBALL`。不搜索缓存，
  不规定版本。缺少或不完整输入为 `UNVERIFIED`；显式路径不存在、
  输入冲突或不满足调用方期望时为 `FAIL`。原生依赖由宿主安装提供。
  可选期望与 tarball 布局见[真实库测试说明](../../packages/maa-go/testdata/README.md)。

```sh
MAA_CORE_LIB_PATH=/opt/maa/libMaaCore.so \
MAA_CORE_RESOURCE_ROOT=/opt/maa \
go run ./tools/maa-go-check --layers core

MAA_CORE_TARBALL=/opt/downloads/maa-linux.tar.gz \
go run ./tools/maa-go-check --layers core
```

## 报告

默认输出逐层表格、环境摘要与结论。环境摘要只包含 Go/OS/架构、purego
版本、默认 cgo 设置、C 编译器、显式输入类型、实际库或 tarball 的 SHA-256 前缀与 git 提交；
不包含仓库根、用户目录或外部资源路径。`--json` 输出同样的信息，便于
CI 归档。静态层的 JSON `header_input` 保留选定头文件的 `snapshot_id`、
来源与逐文件摘要；CI 显式选择仓库快照并上传该报告。头文件来源与真实
安装分开记账；`core` 没有执行时不声明真实库版本或能力通过。

`race` 层显式使用 `CGO_ENABLED=1`，与生产 `CGO_ENABLED=0` 构建分开
记录。race 通过只辅助发现 Go 并发问题，不证明原生 ABI；fixture 通过
也不证明真实 MaaCore 行为。分层报告的意义就是不让某一层的结论借给
另一层。
