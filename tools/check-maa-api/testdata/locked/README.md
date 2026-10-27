# 锁定 C 输入（MaaCore v6.20.1）

本目录是 `check-maa-api` 的完整、可离线使用的 `checker_input` 目录，
用于在不依赖用户缓存发行包的情况下执行真实的锁定头比对。它只包含
声明层输入，不是 `maa-go` 的运行依赖：这里没有 `libMaaCore.so`，也没
有 `resource/` 资源。

## 来源

- 仓库：`https://github.com/MaaAssistantArknights/MaaAssistantArknights`
- tag：`v6.20.1`
- commit：`d94dd007aca3b519395fb1f0aee26c7bd5ba5d5f`
- 三个头文件：发行包 `MAA-v6.20.1-linux-x86_64.tar.gz` 归档根的
  `AsstCaller.h`、`AsstPort.h`、`AsstCallerExtra.h`
- 许可证：上游源码树同一 commit 的 `LICENSE`（AGPL-3.0）

## 文件与摘要

| 归档路径 | 用途 | size | SHA-256 |
|---|---|---|---|
| `upstream/AsstCaller.h` | checker_input | 5413 | `8f1245d55198364bc15beaa2ff91456c747e258beb7a7547f80751563f8c94b7` |
| `upstream/AsstPort.h` | checker_input | 1087 | `1c99ea5ceb121e323fefbd4116e344efa73640b719474b738aad1b15b6de4a64` |
| `upstream/AsstCallerExtra.h` | checker_input | 1312 | `9229054cf8cd6a36eb4ae59e4927fb3e16de89800bf922f96b67d0c672450737` |
| `upstream/LICENSE` | upstream_license | 34523 | `8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef` |

`inputs.json` 中的 `profile_id`、`source`、路径、大小与摘要必须与
`check-maa-api` 内部锁定 profile 常量一致；不一致时 checker 以
`input` 类诊断失败。语义参考头 `AsstTypes.h`/`AsstMsg.h` 不随本目录
分发；checker 只把它们当作可选的来源摘要条目，不解析 C++。

## 使用

```sh
go run ./tools/check-maa-api --profile maa-v6.20.1 --goos linux --goarch amd64 \
  --c-input tools/check-maa-api/testdata/locked --go-root .
go run ./tools/check-maa-api --profile maa-v6.20.1 --goos windows --goarch amd64 \
  --c-input tools/check-maa-api/testdata/locked --go-root .
```

`TestVendoredLockedInputRun` 对两个目标执行上述完整命令入口并核对
`bound`/`nontarget_bindings`/`excluded` 与签名记录数量。更新锁定
MaaCore 版本时，必须同时更新本目录、`inputs.json`、checker 内部
profile 常量与期望表，再重跑两个目标；只改其中一侧会失败。
