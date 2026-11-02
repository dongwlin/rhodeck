# MaaCore C 头快照

此目录提供可离线核对的静态输入；路径与快照身份不绑定发行版本。checker 根据调用方清单核对字节，再与 Go raw 声明及独立策略表比较。调用方可通过 `--c-input` 选择其他来源，无需在生产代码注册版本。

## 当前来源

- 上游：[MaaAssistantArknights/MaaAssistantArknights](https://github.com/MaaAssistantArknights/MaaAssistantArknights)
- 来源 tag：`v6.21.0-beta.3`
- commit：`ece7da53787dbe9980bb075277c86edfadfde4c4`
- 原始路径：`include/AsstCaller.h`、`include/AsstPort.h`、`include/AsstCallerExtra.h`、`LICENSE`
- 文件原文未修改，大小和 SHA256 见 [inputs.json](inputs.json)。
- 许可原文：[upstream/LICENSE](upstream/LICENSE)。

tag 与提交是头文件来源记录，不限制运行时 MaaCore 版本。更新头文件时同步填写来源与摘要，并运行静态检查；ABI 差异需要同步绑定和独立期望表。

## 使用

在仓库根执行：

```sh
go run ./tools/check-maa-api --goos linux --goarch amd64 \
  --c-input tools/check-maa-api/testdata/snapshot --go-root .
go run ./tools/check-maa-api --goos windows --goarch amd64 \
  --c-input tools/check-maa-api/testdata/snapshot --go-root .
```
