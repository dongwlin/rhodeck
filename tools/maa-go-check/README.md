# maa-go-check

`maa-go-check` 是 maa-go 的分层验收入口。它把静态声明检查、Go 故障
注入、loader/ABI/callback fixture、真实 MaaCore 无设备链路与显式设备
场景分成独立的层运行，并逐层报告 `PASS`、`UNVERIFIED`、`FAIL` 或
`NOT SELECTED`。它存在的目的只有一个：让"跳过不是通过"成为可执行
的退出码，而不是报告里的一句话。

## 用法

```sh
# CI 与离线默认集合（需要 Go 与 C 编译器；不接触设备）
go run ./tools/maa-go-check --layers ci

# 单层
go run ./tools/maa-go-check --layers static
go run ./tools/maa-go-check --layers core
go run ./tools/maa-go-check --layers race

# 全部层（真实库与设备层需要外部资源/授权）
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
| `device-gate` | 设备入口/配置拒绝/许可门/超时与失败分类测试；不执行设备动作 | 无 | 是 |
| `loader` | 独立 C loader fixture 的装载/解析/注册 | `cc`/`gcc` | 是 |
| `abi` | 真实 C ABI fixture 的数值、参数、void 与缓冲区调用 | `cc`/`gcc` | 是 |
| `callback` | 原生线程 callback fixture | `cc`/`gcc` | 是 |
| `core` | 锁定 MaaCore v6.20.1 的真实库无设备链路（M1/M3） | 锁定发行包 | 否 |
| `device` | 显式启用的连接、截图与 Depot 场景（M4） | `MAA_M4_DEVICE_CONFIG` | 否 |
| `build` | `CGO_ENABLED=0 go build ./...` 与 `go vet ./...` | 无 | 是 |
| `fmt` | `gofmt -l .`，有输出即失败 | 无 | 是 |
| `race` | `go test ./packages/maa-go/... -race` | `cc`/`gcc`（race 需要 cgo） | 独立任务 |

`race` 层由 `raceGoTestStep` 固定携带 `Race: true`，`goTestArgs` 因此
追加 `-race`；`TestGoTestArgsIncludesRaceOnlyWhenRequested` 钉住该参数，
缺少 `-race` 时测试失败，避免把普通测试二进制报告成竞态检查通过。

`--layers ci` 是 `static,go,device-gate,loader,abi,callback,build,fmt`。
`--layers all` 包含 `core`、`device` 与 `race`；缺少发行包、设备配置或
编译器时对应层为 `UNVERIFIED`，二进制整体退出码 2（`go run` 统一显示
为 1）。CI 工作流 `.github/workflows/maa-go.yml` 只调用 `ci` 与独立的
`race` 层，**不运行也不声称** `core` 与 `device`。

## 状态与退出码

| 状态 | 含义 | 二进制退出码 |
|---|---|---|
| `PASS` | 所有选中的检查实际执行并通过 | 0 |
| `PASS (expected skips)` | 层内只有明确声明的预期跳过（例如设备门默认关闭）；跳过的测试没有验证任何行为，详情会列出 | 0 |
| `UNVERIFIED` | 选中的层被跳过、匹配不到测试、或缺少外部资源/授权；**不是通过** | 2 |
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
  `MAA_M1_TARBALL` 覆盖；测试会校验整包与成员 SHA-256。缺失时该层
  `UNVERIFIED`。
- **设备**：只有设置 `MAA_M4_DEVICE_CONFIG`（仓库外绝对路径）才会运行
  `device` 层。配置的许可范围由 MAA-28 的入口门强制；没有配置时
  `device` 层 `UNVERIFIED`，`device-gate` 只证明默认关闭与拒绝矩阵。
  设备地址、截图与原始日志不写入仓库，也不写入本工具的报告。

## 报告

默认输出逐层表格、环境摘要与结论。环境摘要只包含 Go/OS/架构、purego
版本、默认 cgo 设置、C 编译器与 ADB 版本、锁定发行包 SHA-256 前缀、
设备配置是否设置与 git 提交；不包含仓库根、用户目录或设备地址。
`--json` 输出同样的信息，便于 CI 归档。

`race` 层显式使用 `CGO_ENABLED=1`，与生产 `CGO_ENABLED=0` 构建分开
记录。race 通过只辅助发现 Go 并发问题，不证明原生 ABI；fixture 通过
也不证明真实 MaaCore/设备行为。分层报告的意义就是不让某一层的结论
借给另一层。
