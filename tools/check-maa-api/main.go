// Command check-maa-api 是 maa-go 的最小静态 API checker。命令入口
// (参数解析、目标/profile 选择、LoadCInput→ExtractC 调用、诊断输出
// 与退出码)由后续阶段接入;本阶段提供并冻结锁定 profile、C 输入
// 校验与受限 C 提取链路。任何失败都必须以非零退出码结束,失败时
// 不得输出 PASS。
package main

import (
	"errors"
	"fmt"
	"os"
)

// errEntryNotImplemented 标记留给后续阶段的命令边界。
var errEntryNotImplemented = errors.New("command entry is provided by the follow-up stage")

func main() {
	if err := runEntry(); err != nil {
		fmt.Fprintln(os.Stderr, "check-maa-api:", err)
		os.Exit(1)
	}
}

// runEntry 是命令入口占位:后续阶段在此实现 --profile/--goos/
// --goarch/--c-input/--go-root 的解析与完整校验链。
func runEntry() error { return errEntryNotImplemented }
