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
| ABI fixture | `testdata/abifixture/fixture.c` | 与锁定 `AsstCaller.h` 对应的真实函数 | uint8/负 int32/uint64 往返、4/5/6 参数位置、void 副作用、借用字符串复制、图像/UUID/任务列表缓冲区协议 | 真实 MaaCore 的资源、连接、任务或线程行为 |
| callback fixture | `testdata/callbackfixture/fixture.c` | 从原生线程回调 Go trampoline | token 往返、线程回调、调用返回后的 JSON 改写、panic 边界 | C++ 异常、原生崩溃隔离或真实 Core 的回调序列 |
| 真实库无设备 | `acceptance_linux_test.go`、`acceptance_nodevice_linux_test.go` | 锁定发行包 + 公开 API | 真实 `libMaaCore.so` 的 Version/NullSize、初始化、实例创建/关闭、未连接截图失败关联 | 设备连接、截图成功或任务执行 |
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

# 真实库无设备链路（需要锁定发行包）
CGO_ENABLED=0 go test ./packages/maa-go -run 'TestRealCore' -count=1 -v -timeout 30m
```

真实库测试默认读取
`~/.cache/maa/MAA-v6.20.1-linux-x86_64.tar.gz`，可用 `MAA_CORE_TARBALL`
覆盖；整包与成员 SHA-256 不匹配是失败，不是跳过。锁定发行包缺失时
测试明确 SKIP 并打印受阻原因；SKIP 不计为本平台验收通过。

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
[check-maa-api](../../../tools/check-maa-api/README.md)，仓库内锁定头见
[锁定 C 输入](../../../tools/check-maa-api/testdata/locked/README.md)。
