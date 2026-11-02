# Rhodeck 开发指南

本文件是仓库唯一阅读入口。先遵守全局规则，再按修改范围从“延伸阅读”选择子目录指南；无需预读所有模块文档。

## 项目与边界

Rhodeck 是一个 Go module，提供 MaaCore 动态绑定、独立 Core worker、父进程监管客户端、control 应用运行时与独立 CLI。原生绑定通过 purego 调用 C ABI，生产代码可用 `CGO_ENABLED=0` 构建；MaaCore、资源和 ADB 由调用方另行提供。

| 路径 | 职责 |
| --- | --- |
| `packages/maa-go/` | 包名 `maa`；公开 Go API、输入校验、生命周期、事件与异步操作；`internal/native/` 封装 loader、ABI 和 callback |
| `apps/worker/` | 可执行程序；入口只有 `main.go`，实现位于 `internal/app`，在子进程内持有绑定，把命令和观察转换为 JSONL |
| `apps/cli/` | 独立 `rhodeck-cli`；`internal/output` 拥有机器输出契约与退出码，`internal/plan` 拥有计划服务与适配器注册表，`internal/cmd` 拥有 cobra 命令面，`internal/app` 拥有进程外壳 |
| `packages/control/` | 应用运行时；运行身份、任务结果、停止、目录互斥与最小执行记录 |
| `packages/control/adapters/` | 按任务分包的显式类型适配器；严格输入编译、目标选择与回调结果解释 |
| `packages/control/worker/` | 父进程客户端；启动、请求关联、期限、诊断和进程回收 |
| `packages/control/worker/protocol/` | 纯协议 DTO、编解码和有界队列 |
| `tools/check-maa-api/` | 显式来源化 C 头与 Go raw 声明的静态一致性检查 |
| `tools/maa-go-check/` | 分层验证入口，分别报告各层执行结果 |
| `tools/api-coverage/` | 完整 worker 的 API 覆盖清单与漂移门禁：扫描 maa-go 公开面并与冻结清单、五段映射对账 |

依赖方向为 CLI → control 与显式 adapters、adapters → control 契约、control → 监管客户端 → protocol、worker → protocol 与 maa、maa → internal/native。CLI 内部按 app → cmd → plan → output 单向依赖，每层只依赖下一层与 control；该分层由 `apps/cli/internal/app` 的层依赖测试守门。control 不导入具体 adapter；control 和监管客户端不加载原生库；protocol 不依赖绑定、进程管理或应用服务。不要将规划中的产品模块当作已实现能力。

## 全局开发约定

- raw 任务类型与参数、连接 extras 和原生消息保持开放；显式适配器按各自契约严格校验。使用 `json.RawMessage` 保留未知字段和大整数，禁止经 `map[string]any` 的 float64 往返破坏精度。
- 请求被接受、获得 task/async ID、`Running=false`、IPC 可响应都不能推导为任务成功；业务完成依据 Core 事件解释。故障后不自动重连、重试或重放设备动作，未确认结果保留为未知。
- Events 流只设一个消费者，并同时观察相应 Done/Err；关闭完成后仍可读取缓冲事件，清理不能依赖消费者继续排空。
- 错误通过 `errors.Is` / `errors.As`、协议稳定 Code 判别；诊断文本不作为控制逻辑。新增错误和测试诊断沿用英文，注释沿用所在文件的语言。
- Go 导出标识符使用 CamelCase，协议字段沿用 `snake_case` JSON tag；测试放在对应包的 `*_test.go`，平台差异沿用文件后缀和 build constraints。
- 测试断言统一使用 testify：失败即止的前置条件用 `require`，可继续的检查用 `assert`；testify 消息参数为急切求值，并发写入的缓冲区只能在失败路径或进程 Wait 之后读取。
- 新增行为或修复 Bug 时，在负责该行为的层补接受、拒绝或回归用例；并发修改需覆盖相关竞态与关闭路径，优先用通道/受控阻塞确认时序。
- 可复用接口契约留在包文档和 README；任务编号、实施过程与个人验收证据不写入生产源码。
- 指南只补充所在层的维护规则，不重复展开已有契约。文档链接集中于各指南的“延伸阅读”，每个目标只列一次并注明阅读时机；指南之间仅向子目录引用，普通文档不反向要求读取指南。

## 必要命令

以下命令均在仓库根执行。Go 版本与依赖以模块配置为准，CI 使用其指定工具链；C 编译器用于原生测试夹具和 race 检查。

| 用途 | 命令 |
| --- | --- |
| 下载 Go 依赖 | `go mod download` |
| 生产构建 / 编译检查 | `CGO_ENABLED=0 go build ./...` |
| 静态检查 | `CGO_ENABLED=0 go vet ./...` |
| 格式检查 | `gofmt -l .`（有输出需修正）；修改文件用 `gofmt -w <file.go>` |
| Linux 绑定与工具验证 | `go run ./tools/maa-go-check --layers ci` |
| Windows/macOS 绑定与工具验证 | `go run ./tools/maa-go-check --layers static,go,build,fmt` |
| 绑定 race 检查 | `go run ./tools/maa-go-check --layers race` |
| control、CLI、worker 与协议测试 | `CGO_ENABLED=0 go test ./apps/cli/... ./apps/worker/... ./packages/control/... -skip TestRealCore -count=1 -timeout=3m` |
| control、CLI、worker race 检查 | `go test -race ./apps/cli/... ./apps/worker/... ./packages/control/... -skip TestRealCore -count=1 -timeout=8m` |
| API 覆盖漂移检查 | `go run ./tools/api-coverage --root .` |

当前没有单独的 Lint、类型检查脚本或部署工作流；Go 编译与 vet 承担相应检查。CLI 直接执行命令，worker 通过 stdin/stdout JSONL 由父进程驱动，不需要启动 Web 服务；二进制构建与调用示例按模块契约查阅。

## 验证与交付

1. 先运行改动所在层的定向检查，再按上表运行相关模块验证；协议变化同时验证 worker 和客户端，绑定变化还需验证 worker 的适配行为。
2. 修改 Go 代码后检查格式、生产构建和 vet。修改并发/生命周期逻辑时，运行相应的 race 检查；纯指南改动核对事实、链接和 diff 即可。
3. 修改被 API 覆盖清单登记的方法、类型、字段、常量或错误时，同次更新 `tools/api-coverage/inventory.go` 并运行漂移检查。原生 ABI、平台和真实库变化按子指南选择额外验证；CI 的运行与交叉编译目标以配置为准。
4. `SKIP`、`UNVERIFIED` 和未选择的层均不算验证通过；静态检查、夹具、交叉编译不能替代真实 MaaCore 运行证据。真实库测试为无设备链路，不扩展为自动真机验收。
5. 提交前用 `git diff --check` 和 `git status --short` 检查改动范围；新增的未跟踪指南也检查格式与链接。交付说明列出已运行检查和未执行的资源或平台验证。

## Git 规范

- commit message 使用英文并遵循 **Conventional Commits**：`<type>[optional scope]: <description>`。
- 每个 commit 聚焦一个目的，不混入与当前任务无关的重构。
- branch 使用 `<type>/<kebab-case-description>`，`<type>` 与本次主要修改类型一致；不得使用 Agent、模型或工具名称作为 prefix。示例：`test/agent-server-callback-test`。
- change type vocabulary 应采用项目 Development Guide 的定义。当前仓库没有该指南；现有 Git 历史可确认的类型为 `feat`、`fix`、`docs`、`refactor`、`test`、`ci`、`chore`，沿用这些类型。若后续补入 Development Guide，以其中词表统一维护，不另建冲突规则。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [maa-go 指南](packages/maa-go/AGENTS.md) | 修改绑定、native、C 夹具或声明检查工具时读取 |
| [worker 指南](apps/worker/AGENTS.md) | 修改原生适配、worker 会话或进程入口时读取 |
| [control 指南](packages/control/AGENTS.md) | 修改业务状态、适配器、停止、记录、恢复或目录所有权时读取；监管与协议任务继续按其中导航读取 |
| [CLI 指南](apps/cli/AGENTS.md) | 修改 CLI 命令、输入、适配器注册、机器输出、退出码或诊断时读取 |
| [模块配置](go.mod) | 安装环境、升级 Go/purego 时核对版本；依赖校验值由 `go.sum` 维护 |
| [API 覆盖清单说明](tools/api-coverage/README.md) | 修改公开绑定面或跨层映射时读取清单维护纪律与检查模式 |
| [maa-go CI](.github/workflows/maa-go.yml) | 修改验证工具、原生支持或工具链时核对矩阵、选层和路径过滤 |
| [worker CI](.github/workflows/worker.yml) | 修改 worker/监管验证时核对测试、race 和交叉编译要求 |
