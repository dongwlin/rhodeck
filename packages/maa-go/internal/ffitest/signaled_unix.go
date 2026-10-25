//go:build unix

// 子进程信号死亡的检测在 unix 上经 syscall.WaitStatus 判定。与
// ffitest.go 分文件的原因:WaitStatus.Signaled 只在 unix 平台存在,
// 通用执行器须保持 GOOS=windows 等交叉构建可编译。

package ffitest

import (
	"os/exec"
	"syscall"
)

// exitSignaled 报告子进程是否被信号杀死(含父进程超时 SIGKILL 与
// 原生崩溃 SIGSEGV/SIGABRT)。
func exitSignaled(ee *exec.ExitError) bool {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}
