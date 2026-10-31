# maa-go 测试夹具与真实库验收入口

本目录以及 `internal/native/testdata/` 保存测试专用的
手写 C 夹具与真实库验收说明。它们都不参与生产 Go 构建：生产包没有
cgo，C 夹具只在测试进程里由 C 编译器生成原生动态库，再经生产 loader 加载。
夹具通过不等于真实 MaaCore 通过，各层结果分开记录。

## 夹具一览

| 夹具 | 源码 | 用途 | 证明 | 不能证明 |
|---|---|---|---|---|
| 跨平台 loader fixture | `internal/native/testdata/platformfixture/` | Linux/macOS/Windows 主库与同目录依赖库 | Unicode/空格路径、依赖解析及缺失错误、符号查找、uint64 调用、C→Go callback 与 JSON 复制 | MaaCore API 表注册、原生线程并发或真实 Core 行为 |
| loader fixture | `internal/native/testdata/loaderfixture/fixture.c` | 26 个空导出，含缺少必需符号的变体 | 系统 loader 的打开/查找/注册事务、缺符号聚合报告、失败不发布半成品 | 任何 ABI 调用；空导出签名有意不对应真实头，绝不可调用 |
| ABI fixture | `testdata/abifixture/fixture.c` | 与 C ABI 声明对应的真实函数 | uint8/负 int32/uint64 往返、4/5/6 参数位置、void 副作用、借用字符串复制、图像/UUID/任务列表缓冲区协议 | 真实 MaaCore 的资源、连接、任务或线程行为 |
| callback fixture | `testdata/callbackfixture/fixture.c` | 从原生线程回调 Go trampoline | token 往返、线程回调、调用返回后的 JSON 改写、panic 边界 | C++ 异常、原生崩溃隔离或真实 Core 的回调序列 |
| 真实库无设备 | `acceptance_linux_test.go`、`acceptance_nodevice_linux_test.go` | 显式安装/发行包 + 公开 API | 真实 `libMaaCore.so` 的 Version/NullSize、初始化、实例创建/关闭、未连接截图失败关联 | 设备连接、截图成功或任务执行 |
| 静态 checker | `tools/check-maa-api/` | C 头 + Go raw 声明 | 声明层符号/宽度/策略一致性 | 寄存器传递、内存有效性、原生行为 |

## 运行

```sh
# C 夹具（需要 cc 或 gcc；缺失时测试 SKIP，SKIP 不是通过）
CGO_ENABLED=0 go test ./packages/maa-go/internal/native \
  -run 'TestLoaderFixture|TestABIFixture|TestCallbackFixture' -count=1 -v

# 只跑一组
CGO_ENABLED=0 go test ./packages/maa-go/internal/native -run 'TestPlatformLoader' -count=1 -v
CGO_ENABLED=0 go test ./packages/maa-go/internal/native -run 'TestABIFixture' -count=1 -v
CGO_ENABLED=0 go test ./packages/maa-go/internal/native -run 'TestCallbackFixture' -count=1 -v
CGO_ENABLED=0 go test ./packages/maa-go/internal/native -run 'TestLoaderFixture' -count=1 -v

# 真实库无设备链路（需显式指定安装，根目录下含 resource/）
MAA_CORE_LIB_PATH=/absolute/install/libMaaCore.so \
MAA_CORE_RESOURCE_ROOT=/absolute/install \
CGO_ENABLED=0 go test ./packages/maa-go -run 'TestRealCore' -count=1 -v -timeout 30m
```

真实库测试不读取默认缓存，接受以下互斥输入（路径均为绝对路径）：

| 变量 | 含义 |
| --- | --- |
| `MAA_CORE_LIB_PATH` + `MAA_CORE_RESOURCE_ROOT` | 已安装库及包含 `resource/` 的资源根；安装宿主保留完整原生依赖 |
| `MAA_CORE_TARBALL` | 显式 gzip tar；提取顶层 `lib*.so*` 与 `resource/`，其余文件忽略 |
| `MAA_CORE_EXPECTED_VERSION` | 可选实际 Core 版本期望；空值不比较 |
| `MAA_CORE_EXPECTED_LIBRARY_SHA256` | 可选主库文件 SHA256 期望 |
| `MAA_CORE_EXPECTED_TARBALL_SHA256` | 可选显式 tarball SHA256 期望 |
| `MAA_CORE_EXPECTED_VERSION_SHA256` | 可选 `resource/version.json` SHA256 期望，仅覆盖该文件 |

例如使用本批次选定的发行包（不填写固定版本或默认路径）：

```sh
MAA_CORE_TARBALL=/absolute/input/core.tar.gz \
  go run ./tools/maa-go-check --layers core
```

真实输入未提供或库/资源根只提供一项时明确 SKIP，分层工具报告
UNVERIFIED；它们都不计通过。期望摘要格式错误或无 tarball 却配置其
期望摘要时，即使输入不完整也失败。已提供的无效路径、模式冲突、解包失败、
显式期望不符均为失败。归档拒绝路径穿越、重复成员和资源符号链接；
顶层动态库符号链接只允许引用解包内同目录的动态库。

测试记录实际 tarball（如提供）、库和 `resource/version.json` 的 SHA256，
并由子进程报告实际 Core 版本与 NullSize。运行期间输入不可变由安装宿主
保证。资源来源、提交、覆盖范围由宿主记录，`version.json` 摘要不是全树
摘要；参见[资源溯源与 CLI 对账](../../../apps/cli/installation-provenance.md)。
缺依赖检查按实际 ELF 导入表识别同目录捆绑库；无可隔离依赖或系统在
隔离后仍能解析全部依赖时，该检查明确 SKIP/UNVERIFIED。
无设备链路验证不代表任务能力或设备验收通过。

## 分层入口

建议使用统一入口，而不是逐条记忆测试名：

```sh
go run ./tools/maa-go-check --layers loader
go run ./tools/maa-go-check --layers go
go run ./tools/maa-go-check --layers abi
go run ./tools/maa-go-check --layers callback
go run ./tools/maa-go-check --layers core
```

`UNVERIFIED` 表示选中层被跳过或缺少资源，不是通过；详见
[maa-go-check](../../../tools/maa-go-check/README.md)。静态 checker 的输入与使用方式见
[check-maa-api](../../../tools/check-maa-api/README.md)，仓库内头文件来源见
[C 头文件快照](../../../tools/check-maa-api/testdata/snapshot/README.md)。
