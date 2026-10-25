//go:build !unix

// 非 unix 平台没有可移植的「被信号杀死」判定(如 Windows 的
// TerminateProcess 终止不产生信号),统一报告为未被信号杀死;该平台
// 的原生崩溃以非零退出码呈现,超时终止以 Err + TimedOut 呈现。

package ffitest

import "os/exec"

// exitSignaled 恒为 false,理由见文件头注释。
func exitSignaled(*exec.ExitError) bool { return false }
