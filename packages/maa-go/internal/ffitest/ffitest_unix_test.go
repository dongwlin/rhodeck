//go:build unix

// 信号死亡用例:SIGKILL 自杀仅在 unix 信号模型下可移植(syscall.Kill
// 向自身发信号),故独立成文件并以 unix 构建约束限定;非 unix 平台的
// 原生崩溃以非零退出码呈现,不属于本文件的验收范围。

package ffitest_test

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/ffitest"
)

// modeSignal 与 signalMarker 是信号死亡模式的注册键与 stdout 标记。
const (
	modeSignal   = "signal"
	signalMarker = "FFITEST_SIGNAL"
)

func init() {
	ffitest.Register(modeSignal, runSignalChild)
}

// runSignalChild 打印标记后以 SIGKILL 杀死自身:模拟原生崩溃不可
// recover 的进程死亡,由父进程验收信号死亡的报告语义。SIGKILL 不可
// 捕获,Kill 成功后不会返回;失败则打印诊断并以 1 退出(可见失败)。
func runSignalChild() int {
	fmt.Println(signalMarker + " killing self with SIGKILL")
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		fmt.Fprintf(os.Stderr, "signal child: Kill failed: %v\n", err)
		return 1
	}
	time.Sleep(time.Minute) // 不可达:防御 Kill 异常失效导致自然退出
	return 0
}

// TestRunSignalDeath 验收信号死亡:Err 非 nil、Signaled 置位、
// ExitCode 为 -1,且已产出的标记保留——原生崩溃必须成为可见失败。
func TestRunSignalDeath(t *testing.T) {
	out := ffitest.Run(t, ffitest.Options{Mode: modeSignal, Timeout: childTimeout})
	if out.Err == nil {
		t.Fatalf("SIGKILL death must be reported as a failure:\n%s", out.Diagnostics())
	}
	if !out.Signaled {
		t.Fatalf("signal death must be reported via Signaled (Signaled=false, err=%v):\n%s", out.Err, out.Diagnostics())
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d when killed by signal, want -1:\n%s", out.ExitCode, out.Diagnostics())
	}
	if out.TimedOut {
		t.Fatalf("signal death must not be misreported as a timeout:\n%s", out.Diagnostics())
	}
	if !strings.Contains(out.Stdout, signalMarker) {
		t.Fatalf("signal death output must preserve the marker already produced:\n%s", out.Diagnostics())
	}
}
