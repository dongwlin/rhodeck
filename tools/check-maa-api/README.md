# check-maa-api

maa-go 的最小静态 API checker:对锁定的 MaaCore C 头与
`packages/maa-go/internal/native` 的 Go 声明做独立提取与一对一关联,
任何不一致都以非零退出码失败。工具只使用 Go 标准库,是主仓库根
module 下的开发工具,不成为 maa-go 的运行依赖。

静态自检证明的是**声明层**的一致性:符号覆盖、R/O 与排除清单、与锁
定 profile 冻结期望表的独立策略比对、宽度级 C↔Go 逐参数语义比对
(参数个数/顺序/具名、返回值形态、类型宽度/符号位/指针用途与 char
pointee 的 const 语义,含 void* 的三种上下文许可与 callback 平台适
配)。它**不能**证明寄存器
传递、内存有效性、原生行为或 Windows 运行支持；实际 purego callback
装配在此基础上继续验收。

## 使用方式

```sh
# 仓库内已校验的锁定输入（CI 与离线默认路径）
check-maa-api --profile maa-v6.20.1 --goos linux   --goarch amd64 \
  --c-input tools/check-maa-api/testdata/locked --go-root .
check-maa-api --profile maa-v6.20.1 --goos windows --goarch amd64 \
  --c-input tools/check-maa-api/testdata/locked --go-root .
```

在仓库根可直接 `go run ./tools/check-maa-api …` 或
`go build -o check-maa-api ./tools/check-maa-api`。`--c-input` 也可以
指向任意已校验的独立输入目录（见下文"开发输入"）。

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
  (精确排除覆盖项);
- `expect`:锁定 profile 冻结期望表的逐符号策略对照——30 项(23 项
  required+allPlatforms、2 项 optional+allPlatforms、1 项
  optional+windowsOnly、4 项 excluded),每项给出 `expected` 与
  `actual` 的 policy/platform,按符号名排序,与目标无关。期望表是
  checker 内部常量,不从被检查的 Go 文件推导:同时改写 Go 符号表与
  其派生清单使二者一致,只要偏离冻结期望仍失败。
- `signatures`:宽度级 C↔Go 逐参数语义比对的核验记录——每条绑定一
  个符号条目(目标适用绑定与非目标平台的 windowsOnly 绑定,两目标均
  为全部 26 项,按符号名排序,与目标无关),含双方声明位置
  (`c_header`/`c_line` 与 `go_file`/`go_line`)、逐参数位置的
  C 名/批准映射后的 Go 名与 C 规范化类型/Go raw 类型
  (`position`/`c_name`/`go_name`/`c_type`/`go_type`)、返回映射
  (`return`,`c_type` 与 `go_type`,C void 与 Go 无结果均为空串);
  `callbacks` 给出两个 callback 类型的同样核验(unixCallback 原样
  void,windowsCallback 许可适配为 uintptr)。映射是 checker 内部的
  冻结常量表(含 char pointee 的 const 冻结分布:非 const
  仅许可 AsstGetUUID 的 buff),不从被检查输入推导。

## 失败行为与退出码

失败关闭、不静默跳过:任何输入、文件选择、C 语法、Go 声明形式或关联
失败都以退出码 1 结束;stderr 先输出 profile 与目标头信息,再逐行输出
诊断(`类别: 文件:行号: 符号: expected …, actual …`),stdout 不输出
任何内容,失败路径绝不出现 PASS。诊断类别:

| 类别 | 覆盖 |
|---|---|
| `input` | 命令参数、清单/文件/摘要/profile/目标、Go 文件选择(目录缺失、共用声明文件缺失或被约束隐藏) |
| `c-syntax` | C 输入的受限语法、宏、条件与 typedef(第一段冻结) |
| `go-syntax` | Go 解析、声明形式、raw 类型词表、名字遮蔽、unsafe 导入形态、重复声明、空清单、共用文件自带 build 约束 |
| `relation` | C↔Go 一对一关联:未知导出、漏绑、重复绑定、错误/重复 Target、多余 raw 字段、陈旧或重复排除、实验发现未排除或被绑定、平台矛盾 |
| `policy` | 锁定 profile 独立期望比对:绑定未冻结符号、D/实验项改绑、required↔optional 互换、allPlatforms↔windowsOnly 互换、期望绑定符号被排除或缺失绑定;诊断含条目文件/行号/符号与 expected/actual 策略 |
| `signature` | 宽度级 C↔Go 逐参数语义比对与 callback 平台适配:参数个数增删、匿名/未批准重命名/同类型调序(定位到参数位置)、uint8→bool、int32→int/uint32、uint64→uintptr/uint32、char 标量与 const 标量、pointee 语义改变(const char*→uint8_t*)、char pointee 的 const 改变(C 侧丢 const 或 AsstGetUUID buff 加 const,均无许可映射)、二级指针、uint8* 指针、char*→uintptr、任务 *int32→unsafe.Pointer、图像 unsafe.Pointer→uintptr、void* 上下文许可外的任何用途、void 返回虚构成功码、Unix callback 增加返回、Windows callback 少返回或返回非 uintptr;Go 侧错误定位到字段/callback 声明行,C 类型无许可映射定位到 C 声明行(C 侧类型以规范化拼写给出) |

## 支持的声明语法边界

支持的声明语法边界如下。**支持**:

- C 侧(第一段冻结):`#pragma once`、注释、冻结 include 集合、
  AsstPort.h 的锁定宏表、冻结的条件拼写(非活跃分支同样检查)、
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
- `Policy`/`Platform` 常量的序数值比较;实际 purego callback 装配。

## 文件选择规则

Go 输入固定为 `--go-root/packages/maa-go/internal/native`,按目标
GOOS/GOARCH、`CGO_ENABLED=0` 与工具链 release tags 用标准库 go/build
选择真实参与构建的源文件:处理文件名后缀(如 `loader_windows.go`)、
`//go:build`(如 `loader_unix.go`)、旧式 build 行,排除 `_test.go` 与
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
及其相对路径文件。开发时可用三种来源:

1. 仓库内已校验的锁定输入 `tools/check-maa-api/testdata/locked`
   (推荐,CI 与离线运行使用;来源、摘要与许可见该目录 README);
2. 任意已校验的独立输入目录经显式路径传入(目录须满足同一清单与
   摘要约定);
3. 从锁定发行包自建:MAA v6.20.1 linux 发行包归档根的
   `AsstCaller.h`/`AsstPort.h`/`AsstCallerExtra.h` 三个成员解包到
   `<dir>/upstream/` 下,按上表核对 size/SHA-256 后写出 `inputs.json`
   (集成测试 `realinput_linux_test.go` 即按此流程从
   `os.UserCacheDir()/maa/MAA-v6.20.1-linux-x86_64.tar.gz` 构建输入,
   可用 `CHECK_MAA_API_TARBALL` 覆盖归档路径)。

## 反例覆盖

反例按类别以**完整命令入口**验收:每个反例在
进程内走 Run 的完整 CLI 路径(参数解析 → 摘要校验 → 文件读取 → C
提取 → Go 选择/提取 → 关联 → 期望比对 → 签名比对 → 退出),断言非
零退出、stdout 无内容且无 PASS、stderr 含 profile/目标头、诊断类别
与文件/行号/符号定位。C 侧变体经仅 `_test.go` 可达的测试 profile 注
册表为变体文件计算独立摘要注册合成 profile(生产注册表不动),从而
越过摘要校验走到解析与比对;Go 侧变体用复制后的真实 native 目录改
写。拒绝层如实归属:被更早层冻结规则拒绝的向量钉死在该层,不强行移
到比对层。

| 类别 | Run() 级反例测试 | 拒绝层 |
|---|---|---|
| 输入/目标 | `TestRunArgumentFailures`(缺 flag、未知 profile、未知目标)、`TestRunInputFailures`(缺头、缺清单、缺 native 目录)、`TestCounterexampleCInputThroughCLI`(空文件、盘上摘要不符、清单路径越界)、`TestRealNativeVariantsFailThroughCLI`(空 API/符号/排除表) | input |
| C 提取 | `TestCounterexampleCExtractionThroughCLI`(未知 typedef、循环别名、非活跃分支未知条件、GetVersion 特殊拼写、ASST_CALL 右值与外部预定义、零参数函数加参) | c-syntax |
| 覆盖/关系 | `TestRealNativeVariantsFailThroughCLI`(漏绑、重复绑定、Name 拼错、不存在字段、字段无条目)、`TestCounterexampleCoverageRelationThroughCLI`(Target 指向另一存在字段、helper/计算构造、拼接符号名、别名 unsafe 导入) | relation / go-syntax |
| 策略/平台 | `TestRealNativeVariantsFailThroughCLI`(R↔O 互换、D 项改绑、适用导出被排除、平台互换) | policy / relation(平台互换被 link 先拒绝,期望比对独立性由 expect_test.go 直接钉死) |
| 宽度/符号 | `TestRealNativeSignatureVariantsFailThroughCLI`(uint64→uintptr 等)、`TestCounterexampleSignatureThroughCLI`(uint8→bool、int32→int/uint32、void 虚构 uint8 返回、增删参数) | signature / go-syntax(词表外拼写) |
| 指针/角色 | `TestRealNativeSignatureVariantsFailThroughCLI`(图像 unsafe.Pointer→uintptr、const char*→uint8_t*、丢 const)、`TestCounterexampleSignatureThroughCLI`(char* 返回→uintptr、*int32→unsafe.Pointer;char** 以 c-syntax 拒绝) | signature / c-syntax(多级指针) |
| 参数次序 | `TestRealNativeSignatureVariantsFailThroughCLI`(x/y)、`TestCounterexampleSignatureThroughCLI`(level/message、callback/customArg、窗口 method) | signature |
| 精确排除 | `TestRealNativeVariantsFailThroughCLI`(改名失效排除)、`TestCounterexampleExclusionThroughCLI`(重复、空原因、通配符样式、排除已绑定项、实验导出删除/新增/脱离守卫) | relation / go-syntax(空原因)/ c-syntax(发现集) |
| callback | `TestCounterexampleCallbackThroughCLI`(Unix 增返回、Windows 少返回/返回非 uintptr、两平台参数调序/错类型、C 侧 void 改非 void/丢 ASST_CALL;比对层等价向量见 compare_test.go) | go-syntax(返回形态)/ signature(参数)/ c-syntax(C 侧拼写) |
| 文件选择 | `TestRealNativeVariantsFailThroughCLI`(共用文件缺失/被约束隐藏)、`TestCounterexampleFileSelectionThroughCLI`(平台 tag、ignore tag、_test.go 中的正确副本均不掩盖生产错误) | input / signature / go-syntax |
| 独立性 | `TestRealNativeVariantsFailThroughCLI`(只改 Go 派生表的 policy 破坏)、`TestCounterexampleIndependenceThroughCLI`(字段+Target 同步改名、Go 自洽参数调序) | relation / signature / policy |

「以默认 feature=0 漏做 Extra 发现」的路径被结构性关闭:实验发现
pass 恒以 `ASST_WITH_EXTRA_CALLERS=1` 运行,其结果集被钉死为两个锁
定实验符号(cextract.go),任何扰动(删除、新增、脱离守卫)都以
c-syntax 拒绝;正例基线同时断言成功报告的 `discovery_exports` 恰为
两项(main_test.go)。

### 范围边界

静态 checker 只证明**声明层**的一致性,它不能证明:

- 寄存器传参与调用约定的实际执行效果——由独立 C ABI fixture 的真
  实调用验收;
- 内存有效性、缓冲区容量与指针生命周期——宽度/角色比对只核验声明
  形态;
- 真实 callback 行为——`unixCallback`/`windowsCallback` 的平台适
  配只是声明层形式核验(无返回/uintptr),实际 purego
  `NewCallback` 装配、trampoline 常量 0 返回与线程行为需另行验收;
- Windows 运行支持——`(windows, amd64)` 目标只按同一套冻结规则
  解析声明并核验 Windows 分支拼写,不声称 Windows 库执行通过。

生产路径没有逃逸通道:命令不提供 skip-hash、`-D`、自定义 build
tags 或未锁定 profile 参数;`testProfileFromFiles`/
`registerTestProfile` 只存在于 `_test.go`,生产注册表恒只含锁定
profile(profile_env_test.go 钉死)。C 解析器破坏样例因此经测试
profile 注册表固定独立摘要后仍走完整命令路径,而不是停在摘要校验。
宏环境固定(无外部 `-D`/host 继承),外部预定义 `ASST_CALL` 在
进入 AsstPort.h 前即被拒绝,Wine 调用约定分支的拼写冻结在 Port 宏
表内——外部 ASST_CALL/Wine 配置没有输入通道
(TestPassZeroAndHostileEnv、TestNewMacroEnv)。

## 测试

```sh
CGO_ENABLED=0 go test ./tools/check-maa-api/... -count=1
```

覆盖:C 输入校验与受限 C 提取(含失败向量)、Go 文件选择、Go 提取
形式向量、C↔Go 关联向量、冻结期望表逐项钉死与期望比对失败向量、
宽度级签名比对的冻结映射表钉死与接受/拒绝向量(真实生产树正例、
Go/C 两侧过错方定位、callback 平台适配、C callback 变体的 c-syntax
拒绝归属)、真实目录破坏变体与按目录类别归组的完整命令入
口反例集(counterexample_cli_test.go,含 C 提取、输入摘要、覆盖/
关系、精确排除与实验发现、宽度/指针/次序、callback、文件选择正确
副本不掩盖、独立性)、真实发行包头的两目标端到端正例与 signatures
段确定性(归档缺失时 SKIP 并写明受阻原因,受阻不是通过)。仓库内
`testdata/locked` 的 `TestVendoredLockedInputRun` 不依赖发行包缓存,
对两个目标执行完整命令入口并核对策略计数,使离线与 CI 环境也能跑
真实锁定头比对。全部测试 `CGO_ENABLED=0` 可跑,无网络与绝对路径依赖。
