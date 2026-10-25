# check-maa-api

maa-go 的最小静态 API checker:对锁定的 MaaCore C 头与
`packages/maa-go/internal/native` 的 Go 声明做独立提取与一对一关联,
任何不一致都以非零退出码失败。工具只使用 Go 标准库,是主仓库根
module 下的开发工具,不成为 maa-go 的运行依赖。

静态自检证明的是**声明层**的一致性:符号覆盖、R/O 与排除清单、参数
个数/顺序/具名、返回值形态、类型宽度/符号位/指针用途。它**不能**
证明寄存器传递、内存有效性、原生行为或 Windows 运行支持(绑定设计
§9.1);R/O 策略与锁定 profile 独立期望的比对、宽度级 C↔Go 逐参数
语义比对由 MAA-10 在此基础上继续实现。

## 使用方式

```sh
check-maa-api --profile maa-v6.20.1 --goos linux   --goarch amd64 \
  --c-input /path/to/maa-c-input --go-root /path/to/rhodeck
check-maa-api --profile maa-v6.20.1 --goos windows --goarch amd64 \
  --c-input /path/to/maa-c-input --go-root /path/to/rhodeck
```

在仓库根可直接 `go run ./tools/check-maa-api …` 或
`go build -o check-maa-api ./tools/check-maa-api`。

| flag | 必需 | 说明 |
|---|---|---|
| `--profile` | 是 | 锁定 profile id;生产注册表只含 `maa-v6.20.1`,未注册 id 失败 |
| `--goos` | 是 | 静态目标 GOOS,只接受 `linux` / `windows` |
| `--goarch` | 是 | 静态目标 GOARCH,只接受 `amd64` |
| `--c-input` | 是 | 含 `inputs.json` 及其相对路径文件的目录(见下文"开发输入") |
| `--go-root` | 是 | 仓库根;Go 输入固定为其下 `packages/maa-go/internal/native` |

未知 flag、多余位置参数、缺失 flag、未注册 profile、未知目标组合均失败。
工具不接受 `-D`、自定义 build tags、skip-hash 或任何其他逃逸参数。

## 成功输出

成功退出码 0,stdout 输出键序稳定、列表按名称排序的 JSON,包含:

- `profile` / `target`;
- `c`:完整导出集合(28 项 caller 导出 + 2 项实验发现)、目标适用集合
  与实验发现,以及每个函数的来源头/行号与规范化参数/返回类型;
- `go`:入选源文件、api 字段(含来源与规范化签名)、符号条目、排除
  条目与两个 callback 签名;
- `link`:关联分类表——`bound`(目标适用 R/O 绑定)、
  `nontarget_bindings`(非目标平台的 windowsOnly 条目)、`excluded`
  (精确排除覆盖项)。

## 失败行为与退出码

失败关闭、不静默跳过:任何输入、文件选择、C 语法、Go 声明形式或关联
失败都以退出码 1 结束;stderr 先输出 profile 与目标头信息,再逐行输出
诊断(`类别: 文件:行号: 符号: expected …, actual …`),stdout 不输出
任何内容,失败路径绝不出现 PASS。诊断类别:

| 类别 | 覆盖 |
|---|---|
| `input` | 命令参数、清单/文件/摘要/profile/目标、Go 文件选择(目录缺失、共用声明文件缺失或被约束隐藏) |
| `c-syntax` | C 输入的受限语法、宏、条件与 typedef(第一段冻结) |
| `go-syntax` | Go 解析、§3.1 声明形式、raw 类型词表、名字遮蔽、unsafe 导入形态、重复声明、空清单、共用文件自带 build 约束 |
| `relation` | C↔Go 一对一关联:未知导出、漏绑、重复绑定、错误/重复 Target、多余 raw 字段、陈旧或重复排除、实验发现未排除或被绑定、平台矛盾 |

## 支持的声明语法边界

按绑定设计 §9.1.1 如实列出。**支持**:

- C 侧(第一段冻结):`#pragma once`、注释、冻结 include 集合、
  AsstPort.h 的锁定宏表、§9.1.1 冻结的条件拼写(非活跃分支同样检查)、
  标量 typedef 链、`AsstHandle`、`AsstApiCallback` 函数指针、普通
  `返回类型 ASSTAPI 名称(参数列表);`、唯一的
  `ASSTAPI_PORT const char* ASST_CALL AsstGetVersion();` 拼写、Caller
  两个 Windows enum 的命名值、Extra 四指针结构体、三个锁定零参数符号
  的空 `()`。
- Go 侧:唯一 `type api struct`(字段名=C 符号去 `Asst` 前缀,每字段
  恰好一个具名 `func` 类型,无嵌入/匿名/变参/多返回值,无返回值=无
  结果列表,有返回值=恰一个未命名结果);唯一
  `func symbolSpecs(table *api) []symbolSpec`(单条直接 `return` 复合
  字面量,每条目恰好 Name/Target/Policy/Platform 四个具名 key,Target
  为 `&table.<字段>`,Policy/Platform 只能引用包内
  `required`/`optional`/`allPlatforms`/`windowsOnly` 常量声明);唯一
  `var exclusions []exclusionSpec`(三 key 直接字面量,Header 为裸
  文件名,Reason 非空);`type unixCallback`(无返回)与
  `type windowsCallback`(返回 `uintptr`)。raw 签名类型只允许预声明
  `uint8/int32/uint64/uintptr/byte`、`*byte`、`*int32` 与精确
  `import "unsafe"` 的 `unsafe.Pointer`;包级不得遮蔽
  `uint8/int32/uint64/uintptr/byte/any/iota/unsafe`。三个共用声明文件
  (`api.go`、`symbols.go`、`callback_types.go`)必须无条件参与构建,
  不带任何 build 约束。

**不支持**(出现即失败,不扩展为通用编译器/数据流分析):

- C:变参、其他函数指针、任意结构体传参/返回、未知 typedef/宏/条件、
  多级指针、host 的 `int/long/size_t` 代换、外部预定义 `ASST_CALL`。
- Go:helper/append/循环/条件/拼接/反射构造的清单、间接变量目标、
  自定义宽度别名、别名或伪造的 unsafe 导入、泛型声明。
- 任何 `Policy` 常量的序数值比较与 R/O 期望比对(MAA-10);实际
  purego callback 装配(MAA-13)。

## 文件选择规则

Go 输入固定为 `--go-root/packages/maa-go/internal/native`,按目标
GOOS/GOARCH、`CGO_ENABLED=0` 与工具链 release tags 用标准库 go/build
选择真实参与构建的源文件:处理文件名后缀(`loader_linux.go` /
`loader_other.go`)、`//go:build`、旧式 build 行,排除 `_test.go` 与
`_`/`.` 前缀文件,不接受自定义 tags。生产错误声明与不参与目标构建的
正确副本并存、声明隐藏于 tag、只在测试文件放正确类型,均不能借正确
副本掩盖生产错误。

## 锁定 profile 归档索引

profile `maa-v6.20.1`,来源
`https://github.com/MaaAssistantArknights/MaaAssistantArknights`,
tag `v6.20.1`,commit `d94dd007aca3b519395fb1f0aee26c7bd5ba5d5f`。
期望 size/SHA-256 以 checker 内部常量为准(输入清单自带值仅用于交叉
核对,不一致同样失败):

| 归档路径 | 用途 | size | SHA-256 |
|---|---|---|---|
| `upstream/AsstCaller.h` | checker_input | 5413 | `8f1245d55198364bc15beaa2ff91456c747e258beb7a7547f80751563f8c94b7` |
| `upstream/AsstPort.h` | checker_input | 1087 | `1c99ea5ceb121e323fefbd4116e344efa73640b719474b738aad1b15b6de4a64` |
| `upstream/AsstCallerExtra.h` | checker_input | 1312 | `9229054cf8cd6a36eb4ae59e4927fb3e16de89800bf922f96b67d0c672450737` |
| `upstream/AsstTypes.h` | semantic_reference | 29622 | `884f71e8804808aa79a1c01249532c4ada462ea8fdbc83c66b6ed9a751b3200b` |
| `upstream/AsstMsg.h` | semantic_reference | 2636 | `f6a8b551effa5a3a8cca42805b5a09c9cfa7f53a3f3e01f4bad5b858ee6df7c6` |
| `upstream/LICENSE` | upstream_license | 34523 | `8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef` |

三个 `checker_input` 文件全部必需;`semantic_reference` 与
`upstream_license` 条目缺省可容忍,存在即校验。

## 开发输入

工具不内置 C 头,也不依赖任何本机绝对路径;`--c-input` 指向的目录须
含 `inputs.json`(schema_version=1,profile/source 标识与锁定常量一致)
及其相对路径文件。开发时可用两种来源:

1. 已校验的独立输入归档(如 ops 的 maa02-abi 目录)经显式路径传入;
2. 从锁定发行包自建:MAA v6.20.1 linux 发行包归档根的
   `AsstCaller.h`/`AsstPort.h`/`AsstCallerExtra.h` 三个成员解包到
   `<dir>/upstream/` 下,按上表核对 size/SHA-256 后写出 `inputs.json`
   (集成测试 `realinput_linux_test.go` 即按此流程从
   `os.UserCacheDir()/maa/MAA-v6.20.1-linux-x86_64.tar.gz` 构建输入,
   可用 `CHECK_MAA_API_TARBALL` 覆盖归档路径)。

## 测试

```sh
CGO_ENABLED=0 go test ./tools/check-maa-api/... -count=1
```

覆盖:C 输入校验与受限 C 提取(含失败向量)、Go 文件选择、Go 提取
形式向量、C↔Go 关联向量、真实目录破坏变体的完整命令入口端到端失败、
真实发行包头的两目标端到端正例(归档缺失时 SKIP 并写明受阻原因,
受阻不是通过)。全部测试 `CGO_ENABLED=0` 可跑,无网络与绝对路径依赖。
