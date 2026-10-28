# Rhodeck 开发指南

本文件是仓库唯一阅读入口。先遵守全局规则，再按修改范围从“延伸阅读”选择子目录指南；无需预读所有模块文档。

## 项目与边界

Rhodeck 当前是一个 Go module，提供 MaaCore 动态绑定、独立 Core worker 和父进程监管客户端。生产代码通过 purego 调用原生 C ABI，可用 `CGO_ENABLED=0` 构建；MaaCore、资源和 ADB 由调用方另行提供。

| 路径 | 职责 |
| --- | --- |
| `packages/maa-go/` | 包名 `maa`；公开 Go API、输入校验、生命周期、事件与异步操作；`internal/native/` 封装 loader、ABI 和 callback |
| `apps/core-worker/` | 可执行程序；在子进程内持有绑定，把命令和观察转换为 JSONL |
| `packages/control/worker/` | 父进程客户端；启动、请求关联、期限、诊断和进程回收 |
| `packages/control/worker/protocol/` | 纯协议 DTO、编解码和有界队列 |
| `tools/check-maa-api/` | 锁定 C 头与 Go raw 声明的静态一致性检查 |
| `tools/maa-go-check/` | 分层验证入口，分别报告各层执行结果 |
| `tools/api-coverage/` | 完整 worker 的 API 覆盖清单与漂移门禁：扫描 maa-go 公开面并与冻结清单、五段映射对账 |

依赖方向为监管客户端 → protocol、core-worker → protocol 与 maa、maa → internal/native。监管客户端不加载原生库；protocol 不依赖绑定、进程管理或应用服务。不要将规划中的产品模块当作已实现能力。

## 全局开发约定

- 任务类型、任务参数、连接 extras 和原生消息保持开放；使用 `json.RawMessage` 保留未知字段和大整数，禁止经 `map[string]any` 的 float64 往返破坏精度。
- 请求被接受、获得 task/async ID、`Running=false`、IPC 可响应都不能推导为任务成功；业务完成依据 Core 事件解释。故障后不自动重连、重试或重放设备动作，未确认结果保留为未知。
- Events 流只设一个消费者，并同时观察相应 Done/Err；关闭完成后仍可读取缓冲事件，清理不能依赖消费者继续排空。
- 错误通过 `errors.Is` / `errors.As`、协议稳定 Code 判别；诊断文本不作为控制逻辑。新增错误和测试诊断沿用英文，注释沿用所在文件的语言。
- Go 导出标识符使用 CamelCase，协议字段沿用 `snake_case` JSON tag；测试放在对应包的 `*_test.go`，平台差异沿用文件后缀和 build constraints。
- 新增行为或修复 Bug 时，在负责该行为的层补接受、拒绝或回归用例；并发修改需覆盖相关竞态与关闭路径，优先用通道/受控阻塞确认时序。
- 可复用接口契约留在包文档和 README；任务编号、实施过程与个人验收证据不写入生产源码。

## 必要命令

以下命令均在仓库根执行。Go 版本与依赖以模块配置为准，CI 使用其指定工具链；C 编译器用于原生测试夹具和 race 检查。

| 用途 | 命令 |
| --- | --- |
| 下载 Go 依赖 | `go mod download` |
| 生产构建 / 编译检查 | `CGO_ENABLED=0 go build ./...` |
| 静态检查 | `CGO_ENABLED=0 go vet ./...` |
| 格式检查 | `gofmt -l .`（有输出需修正）；修改文件用 `gofmt -w <file.go>` |
| 构建并查看 worker 版本 | `CGO_ENABLED=0 go build -o /tmp/core-worker ./apps/core-worker`，再执行 `/tmp/core-worker -version` |
| Linux 绑定与工具验证 | `go run ./tools/maa-go-check --layers ci` |
| Windows/macOS 绑定与工具验证 | `go run ./tools/maa-go-check --layers static,go,build,fmt` |
| worker、监管与协议测试 | `CGO_ENABLED=0 go test ./apps/core-worker ./packages/control/worker/... -skip TestRealCoreWorker -count=1 -timeout=3m` |
| worker 与监管 race 检查 | `go test -race ./apps/core-worker ./packages/control/worker/... -skip TestRealCoreWorker -count=1 -timeout=3m` |
| API 覆盖漂移检查 | `go run ./tools/api-coverage --root .`（新增/删除/修改被覆盖 API 时同步更新 `tools/api-coverage/inventory.go`，否则失败） |

当前没有单独的 Lint、类型检查脚本或部署工作流；Go 编译与 vet 承担相应检查。worker 通过 stdin/stdout JSONL 由父进程驱动，没有独立开发服务启动命令；构建示例的 `/tmp` 路径适用于 Unix shell，Windows 使用本机绝对输出路径。

## 验证与交付

1. 先运行改动所在层的定向检查，再按上表运行相关模块验证；协议变化同时验证 worker 和客户端，绑定变化还需验证 worker 的适配行为。
2. 修改 Go 代码后检查格式、生产构建和 vet。修改并发/生命周期逻辑时，运行相应的 race 检查；纯指南改动核对事实、链接和 diff 即可。
3. 原生 ABI、平台和真实库变化按子指南选择额外验证。CI 配置含多平台运行与交叉编译；本机未执行的目标如实列明。
4. `SKIP`、`UNVERIFIED` 和未选择的层均不算验证通过；静态检查、夹具、交叉编译不能替代真实 MaaCore 运行证据。真实库测试为无设备链路，不扩展为自动真机验收。
5. 提交前用 `git diff --check` 和 `git status --short` 检查改动范围，交付说明列出已运行检查及缺少的资源或平台验证。

## Git 规范

- commit message 使用英文并遵循 **Conventional Commits**：`<type>[optional scope]: <description>`。
- 每个 commit 聚焦一个目的，不混入与当前任务无关的重构。
- branch 使用 `<type>/<kebab-case-description>`，`<type>` 与本次主要修改类型一致；不得使用 Agent、模型或工具名称作为 prefix。示例：`test/agent-server-callback-test`。
- change type vocabulary 应采用项目 Development Guide 的定义。当前仓库没有该指南；现有 Git 历史可确认的类型为 `feat`、`fix`、`docs`、`refactor`、`test`、`ci`、`chore`，沿用这些类型。若后续补入 Development Guide，以其中词表统一维护，不另建冲突规则。

## 延伸阅读

| 入口 | 用途与阅读时机 |
| --- | --- |
| [maa-go 指南](packages/maa-go/AGENTS.md) | 修改绑定、native、C 夹具或声明检查工具时读取 |
| [core-worker 指南](apps/core-worker/AGENTS.md) | 修改原生适配、worker 会话或进程入口时读取 |
| [监管与协议指南](packages/control/worker/AGENTS.md) | 修改父进程生命周期、请求路由或 wire contract 时读取 |
| [模块配置](go.mod) | 安装环境、升级 Go/purego 时核对版本；依赖校验值由 `go.sum` 维护 |
| [maa-go CI](.github/workflows/maa-go.yml) | 修改验证工具、原生支持或工具链时核对矩阵、选层和路径过滤 |
| [worker CI](.github/workflows/worker.yml) | 修改 worker/监管验证时核对测试、race 和交叉编译要求 |
