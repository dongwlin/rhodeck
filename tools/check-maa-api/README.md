# check-maa-api

maa-go 的最小静态 API checker:对显式提供的 MaaCore C 头与
`packages/maa-go/internal/native` 的 Go 声明做独立提取与一对一关联,
任何不一致都以非零退出码失败。工具只使用 Go 标准库,是主仓库根
module 下的开发工具,不成为 maa-go 的运行依赖。

静态自检证明的是**声明层**的一致性:符号覆盖、R/O 与排除清单、与独立策略期望表的独立策略比对、宽度级 C↔Go 逐参数语义比对
(参数个数/顺序/具名、返回值形态、类型宽度/符号位/指针用途与 char
pointee 的 const 语义,含 void* 的三种上下文许可与 callback 平台适
配)。它**不能**证明寄存器
传递、内存有效性、原生行为或 Windows 运行支持；实际 purego callback
装配在此基础上继续验收。

## 使用方式

```sh
# 仓库内有来源的未版本化头快照（CI 与离线输入）
check-maa-api --goos linux   --goarch amd64 \
  --c-input tools/check-maa-api/testdata/snapshot --go-root .
check-maa-api --goos windows --goarch amd64 \
  --c-input tools/check-maa-api/testdata/snapshot --go-root .
```

在仓库根可直接 `go run ./tools/check-maa-api …` 或
`go build -o check-maa-api ./tools/check-maa-api`。`--c-input` 也可以
指向任意已校验的独立输入目录（见下文"开发输入"）。

| flag | 必需 | 说明 |
|---|---|---|
| `--goos` | 是 | 静态目标 GOOS,只接受 `linux` / `windows` |
| `--goarch` | 是 | 静态目标 GOARCH,只接受 `amd64` |
| `--c-input` | 是 | 含 `inputs.json` 及其相对路径文件的目录(见下文"开发输入") |
| `--go-root` | 是 | 仓库根;Go 输入固定为其下 `packages/maa-go/internal/native` |

未知 flag、多余位置参数、缺失 flag、未知目标组合均失败。
工具不接受 `-D`、自定义 build tags、skip-hash 或任何其他逃逸参数。

## 成功输出

成功退出码 0,stdout 输出键序稳定、列表按名称排序的 JSON,包含:

- `input` / `target`：`input` 包含核对后的 `snapshot_id`、来源与逐文件大小/SHA256，按路径排序；
- `c`:完整导出集合(28 项 caller 导出 + 2 项实验发现)、目标适用集合
  与实验发现,以及每个函数的来源头/行号与规范化参数/返回类型;
- `go`:入选源文件、api 字段(含来源与规范化签名)、符号条目、排除
  条目与两个 callback 签名;
- `link`:关联分类表——`bound`(目标适用 R/O 绑定)、
  `nontarget_bindings`(非目标平台的 windowsOnly 条目)、`excluded`
  (精确排除覆盖项);
- `expect`:独立绑定策略期望表的逐符号策略对照——30 项（25 项 required+allPlatforms、2 项 optional+allPlatforms、1 项 optional+windowsOnly、2 项 excluded）,每项给出 `expected` 与
  `actual` 的 policy/platform,按符号名排序,与目标无关。期望表是
  checker 内部常量,不从被检查的 Go 文件推导:同时改写 Go 符号表与
  其派生清单使二者一致,只要偏离冻结期望仍失败。
- `signatures`:宽度级 C↔Go 逐参数语义比对的核验记录——每条绑定一
  个符号条目(目标适用绑定与非目标平台的 windowsOnly 绑定,两目标均
  为全部 28 项,按符号名排序,与目标无关),含双方声明位置
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
失败都以退出码 1 结束;stderr 输出目标；输入校验成功后还输出快照身份,再逐行输出
诊断(`类别: 文件:行号: 符号: expected …, actual …`),stdout 不输出
任何内容,失败路径绝不出现 PASS。诊断类别:

| 类别 | 覆盖 |
|---|---|
| `input` | 命令参数、清单/文件/摘要/来源/目标、Go 文件选择(目录缺失、共用声明文件缺失或被约束隐藏) |
| `c-syntax` | C 输入的受限语法、宏、条件与 typedef |
| `go-syntax` | Go 解析、声明形式、raw 类型词表、名字遮蔽、unsafe 导入形态、重复声明、空清单、共用文件自带 build 约束 |
| `relation` | C↔Go 一对一关联:未知导出、漏绑、重复绑定、错误/重复 Target、多余 raw 字段、陈旧或重复排除、实验发现未排除或被绑定、平台矛盾 |
| `policy` | 独立绑定策略期望比对:绑定未冻结符号、D/实验项改绑、required↔optional 互换、allPlatforms↔windowsOnly 互换、期望绑定符号被排除或缺失绑定;诊断含条目文件/行号/符号与 expected/actual 策略 |
| `signature` | 宽度级 C↔Go 逐参数语义比对与 callback 平台适配:参数个数增删、匿名/未批准重命名/同类型调序(定位到参数位置)、uint8→bool、int32→int/uint32、uint64→uintptr/uint32、char 标量与 const 标量、pointee 语义改变(const char*→uint8_t*)、char pointee 的 const 改变(C 侧丢 const 或 AsstGetUUID buff 加 const,均无许可映射)、二级指针、uint8* 指针、char*→uintptr、任务 *int32→unsafe.Pointer、图像 unsafe.Pointer→uintptr、void* 上下文许可外的任何用途、void 返回虚构成功码、Unix callback 增加返回、Windows callback 少返回或返回非 uintptr;Go 侧错误定位到字段/callback 声明行,C 类型无许可映射定位到 C 声明行(C 侧类型以规范化拼写给出) |

## 支持的声明语法边界

支持的声明语法边界如下。**支持**:

- C 侧：`#pragma once`、注释、冻结 include 集合、
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

## 显式 C 输入与溯源

`--c-input` 指向包含 `inputs.json` 的目录。仓库不注册发行版本，不维护版本化 profile，也不从用户缓存寻找固定发行包。调用方可直接准备另一份输入：

```json
{
  "schema_version": 1,
  "snapshot_id": "host-selected-headers",
  "source": {
    "repository": "https://github.com/MaaAssistantArknights/MaaAssistantArknights",
    "commit": "actual-upstream-commit"
  },
  "files": [
    {
      "archive_path": "upstream/AsstCaller.h",
      "source_path": "include/AsstCaller.h",
      "size_bytes": 1234,
      "sha256": "replace-with-the-actual-64-lowercase-hex-digest",
      "purpose": "checker_input"
    }
  ]
}
```

示例省略另两个必需文件；不能直接用作输入。三个 `checker_input` 条目及对应文件必须齐全：`upstream/AsstCaller.h`、`upstream/AsstPort.h`、`upstream/AsstCallerExtra.h`。可选条目仅有 `upstream/AsstTypes.h`、`upstream/AsstMsg.h`（`semantic_reference`）和 `upstream/LICENSE`（`upstream_license`），存在即核对。清单身份和来源必填；`source.repository` 填仓库或下载 URL，`tag`/`commit` 至少提供一个。每个文件大小必须大于零、SHA256 必须是 64 位小写十六进制，实测字节须与清单一致。路径必须相对、不含点分量、不能通过符号链接逃出输入目录。

摘要核对证明本次静态检查读取了清单列出的字节；清单由调用方提供，不能单凭其字段证明来源可信。调用方负责从可信来源取得头文件并记录实际提交、tag 或下载来源。Go raw 的策略和类型比对仍使用 checker 内独立维护的期望表，不能只凭输入清单或 Go 自洽结果通过。

[仓库头快照](testdata/snapshot/README.md)保留来源和许可，目录与 `snapshot_id` 不带发行版本。升级时替换头文件并更新清单实测摘要；来源 tag 只说明本次输入，不是可接受版本白名单。新的声明、宏或 ABI 形态不在支持范围内时明确失败，需审查和更新绑定及独立期望。静态通过不证明该发行库或设备能力可用。

## 测试范围

测试覆盖输入完整性、受限 C 提取、Go 文件选择与声明提取、C↔Go 关联、
独立策略和签名比对，以及符号、宽度、指针、参数次序、callback 和平台
策略的破坏变体。命令级用例要求失败时 stdout 为空，并核对诊断类别与
源码定位。测试输入通过各自的来源清单校验，不依赖网络或默认缓存。

## 范围边界

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

命令不提供 skip-hash、`-D` 或自定义 build tags；显式输入仍须通过完整性、声明、关系、独立策略和签名检查。生产代码没有 profile 注册表。
宏环境固定，不继承外部 `-D` 或宿主预定义宏。外部 `ASST_CALL` 与
Wine 配置没有输入通道；对应调用约定只按头文件中的受支持拼写解析。

## 测试

```sh
CGO_ENABLED=0 go test ./tools/check-maa-api/... -count=1
```

`CHECK_MAA_API_INPUT` 可显式覆盖集成测试的输入目录，错误输入失败；
默认使用仓库快照，不查找发行包缓存。
