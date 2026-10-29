# api-coverage

完整 worker 的 API 覆盖清单与漂移门禁：扫描 `packages/maa-go` 的公开绑定面
（含经 `Capabilities` 别名再导出的能力方法），与冻结清单逐项对账，并核验
每项能力的五段映射

    binding 入口 → DTO/传输（或等价合并）→ worker 适配 → 客户端 → 验收用例

工具只使用主项目内容（`packages/maa-go`、`apps/core-worker`、
`packages/control/worker`），只读不写，不加载原生库，不属于 maa-go 的运行
依赖。它不改变绑定生产 API，也不解释原生行为；静态一致只证明清单与源码
同步，不证明 worker 能力已实现——「已完成」以各任务的实际验证记录为准。

## 两种模式

| 模式 | 含义 | 当前预期结果 |
| --- | --- | --- |
| `--mode drift`（默认） | 源码必须与冻结清单逐项一致（新增/删除/改签名/改字段/改常量/改错误都算漂移）；`done` 条目的四段引用必须全部可解析；`pending:WKR-xx` 缺口允许存在 | 通过（当前零已登记缺口） |
| `--mode final` | 在漂移检查之上，要求零 `pending` 项；已登记缺口不算覆盖 | 通过（零 pending）；出现新缺口即失败 |

`na` 条目（如 `Capabilities.Create`：公共层统一走 CreateEx，无公开入口）
必须附注理由与等价映射，不计缺口也不计覆盖。

## 使用方式

```sh
go run ./tools/api-coverage --root .                # 漂移检查（CI/日常门禁）
go run ./tools/api-coverage --root . --mode final   # 最终完整性检查（WKR-12 复验）
go run ./tools/api-coverage --emit                  # 打印扫描得到的符号表（对账用）
```

退出码：0 通过，1 失败（漂移/映射/缺口），2 用法或扫描错误。

## 清单维护纪律

- 新增、删除或重构任何被覆盖的 API（方法、类型、字段、常量、错误）时，
  必须在**同一次变更**中更新 `inventory.go` 的符号表与映射，否则漂移检查
  失败；测试函数的引用必须真实存在，伪造引用会被解析失败拦下。
- `done` 条目的五段缺一不可：只写能力布尔标记（如 capabilities 里的一个
  bool）而没有 worker 分支、客户端方法与验收用例，检查一律失败。
- 缺口按 `pending:WKR-xx` 登记归属，`--mode final` 要求清零。当前清单
  零 pending：新增能力必须先登记映射与用例再交付；重构缺口归属任务时
  同步改写登记，不以「以后按需补」关闭。

## 测试

`go test ./tools/api-coverage` 覆盖：清单与源码对账、扫描可重复性、32 个
公开入口逐项登记、final 模式对缺口的失败语义，以及变异反例（新增方法/
字段/常量、改名方法/错误哨兵、改签名/字段类型、漏 DTO/worker 分支/
客户端方法、伪造测试引用、能力标记冒充覆盖、非法状态）。
