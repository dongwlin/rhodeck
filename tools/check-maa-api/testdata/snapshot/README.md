# MaaCore C 头快照

此目录提供可离线核对的静态输入；路径与快照身份不绑定发行版本。checker 根据调用方清单核对字节，再与 Go raw 声明及独立策略表比较。调用方可通过 `--c-input` 选择其他来源，无需在生产代码注册版本。

## 当前来源

- 上游：[MaaAssistantArknights/MaaAssistantArknights](https://github.com/MaaAssistantArknights/MaaAssistantArknights)
- 本次选取的 tag：`v6.21.0-beta.3`
- commit：`ece7da53787dbe9980bb075277c86edfadfde4c4`
- 原始路径：`include/AsstCaller.h`、`include/AsstPort.h`、`include/AsstCallerExtra.h`、`LICENSE`
- 文件原文未修改，大小和 SHA256 见 [inputs.json](inputs.json)。
- 许可原文：[upstream/LICENSE](upstream/LICENSE)。

tag 与提交是来源记录，不是最低/最高版本限制或运行支持声明。此静态输入没有随附原生库、资源或设备验收结果。更新头文件时同步填写实测来源与摘要，并运行完整静态检查；ABI 差异必须明确失败或经过审查后更新绑定。

## 使用

在仓库根执行：

```sh
go run ./tools/check-maa-api --goos linux --goarch amd64 \
  --c-input tools/check-maa-api/testdata/snapshot --go-root .
go run ./tools/check-maa-api --goos windows --goarch amd64 \
  --c-input tools/check-maa-api/testdata/snapshot --go-root .
```

## 历史

此前 `testdata/locked` 使用 `maa-v6.20.1` 的版本化 profile，来源为上游 tag `v6.20.1`、commit `d94dd007aca3b519395fb1f0aee26c7bd5ba5d5f`。该旧输入和内部摘要常量已撤出生产检查路径；历史原文可从 Git 历史查看。旧静态结论不继承为当前安装或任务能力支持。
