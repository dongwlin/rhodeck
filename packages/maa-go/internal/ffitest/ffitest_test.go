// ffitest 包的自验证测试:执行器必须先用自身用例验证(步骤 1
// 的验收),覆盖成功路径(含每用例新进程)、异常退出、超时终止、
// 未知模式与 ExtraEnv 传递,以及 Register/Timeout 的契约类断言。信号
// 死亡用例在 ffitest_unix_test.go(SIGKILL 自杀仅 unix 信号模型可
// 移植)。
//
// 子进程模式经 init() 注册;本包 TestMain 先 Dispatch 再 m.Run(),
// 子进程不进入测试框架。所有超时参数取小值,整包测试秒级完成。
package ffitest_test

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/ffitest"
)

// 子进程模式名:本包自验证用例的注册表键,经 ffitest.ChildModeEnv
// 传递给子进程。
const (
	modeOK      = "ok"
	modeExit    = "exit-code"
	modeHang    = "hang"
	modeEchoEnv = "echo-env"
)

// 各模式的 stdout 标记:父进程断言标记在输出中保留,证明子进程确实
// 运行到对应阶段(而不是以某种方式静默通过)。
const (
	okMarker      = "FFITEST_OK"
	exitMarker    = "FFITEST_EXIT_CODE"
	hangMarker    = "FFITEST_HANG"
	echoEnvMarker = "FFITEST_ECHO_ENV"
)

// echoEnvName 与 echoEnvValue 是 ExtraEnv 传递用例的变量名与值:值含
// 空格,顺带验证环境值经进程边界传递不被截断或改写。
const (
	echoEnvName  = "FFITEST_ECHO_ENV_VAR"
	echoEnvValue = "value with spaces 42"
)

// hangSleepDuration 是 hang 子进程的睡眠时长:远超父进程用例的超时
// (3s),保证超时路径命中的是被父进程外部杀死,而不是子进程自然
// 退出。
const hangSleepDuration = 30 * time.Minute

// childTimeout 是成功/异常类子进程的父进程超时上限:正常执行在毫秒
// 级完成,30s 只防御环境异常导致的挂起。
const childTimeout = 30 * time.Second

func init() {
	ffitest.Register(modeOK, runOKChild)
	ffitest.Register(modeExit, runExitCodeChild)
	ffitest.Register(modeHang, runHangChild)
	ffitest.Register(modeEchoEnv, runEchoEnvChild)
}

// TestMain 先分派子进程模式(分派命中即 os.Exit,不进入测试框架),
// 父进程路径照常 m.Run()。
func TestMain(m *testing.M) {
	if code, handled := ffitest.Dispatch(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// runOKChild 是 modeOK 子进程入口:打印含 PID 的标记行后返回 0。PID
// 供父进程断言「每用例新进程」。
func runOKChild() int {
	fmt.Printf("%s pid=%d\n", okMarker, os.Getpid())
	return 0
}

// runExitCodeChild 是 modeExit 子进程入口:stdout/stderr 各写一行后以
// 42 退出,验收非零退出码与双路输出捕获。
func runExitCodeChild() int {
	fmt.Println(exitMarker + " on stdout")
	fmt.Fprintln(os.Stderr, exitMarker+" on stderr")
	os.Exit(42)
	return 0 // 不可达:os.Exit 不返回
}

// runHangChild 是 modeHang 子进程入口:打印标记后长时间 sleep,由父
// 进程的外部超时杀死,专门验收「超时终止被报告为失败并保留诊断」。
func runHangChild() int {
	fmt.Println(hangMarker + " started, sleeping " + hangSleepDuration.String())
	time.Sleep(hangSleepDuration)
	return 0
}

// runEchoEnvChild 是 modeEchoEnv 子进程入口:打印指定 ExtraEnv 变量的
// 值(%q 渲染,空值与空白可辨识),验收环境传递。
func runEchoEnvChild() int {
	fmt.Printf("%s %s=%q\n", echoEnvMarker, echoEnvName, os.Getenv(echoEnvName))
	return 0
}

// TestRunFreshProcessPerCase 验收成功路径与「每用例新进程」契约:连续
// 两次 Run 皆退出 0、Err 为 nil、非超时/信号死亡、标记保留,且两次
// 子进程 PID 不同——同进程复用会让进程级状态(已加载的原生库、
// 生命周期门)跨用例泄漏,这正是本执行器存在的理由。
func TestRunFreshProcessPerCase(t *testing.T) {
	pids := make([]int, 0, 2)
	for i := 1; i <= 2; i++ {
		out := ffitest.Run(t, ffitest.Options{Mode: modeOK, Timeout: childTimeout})
		if out.Err != nil {
			t.Fatalf("Run #%d failed (the success path must not fail):\n%s", i, out.Diagnostics())
		}
		if out.TimedOut || out.Signaled {
			t.Fatalf("Run #%d must not report timeout/signal death:\n%s", i, out.Diagnostics())
		}
		if !strings.Contains(out.Stdout, okMarker) {
			t.Fatalf("Run #%d exited 0 but stdout is missing marker %s:\n%s", i, okMarker, out.Diagnostics())
		}
		pids = append(pids, parseChildPID(t, out.Stdout))
	}
	if pids[0] == pids[1] {
		t.Fatalf("two consecutive Runs got the same child PID (%d), violating the fresh-process-per-case contract", pids[0])
	}
}

// TestRunAbnormalExitCode 验收异常退出:os.Exit(42) 被原样报告为失败
// (Err 非 nil、ExitCode 42),不误报信号死亡/超时,双路输出保留
// 可辨识证据。
func TestRunAbnormalExitCode(t *testing.T) {
	out := ffitest.Run(t, ffitest.Options{Mode: modeExit, Timeout: childTimeout})
	if out.Err == nil {
		t.Fatalf("the exit-code child ended via os.Exit(42), must be reported as a failure:\n%s", out.Diagnostics())
	}
	if out.ExitCode != 42 {
		t.Fatalf("ExitCode = %d, want 42:\n%s", out.ExitCode, out.Diagnostics())
	}
	if out.Signaled || out.TimedOut {
		t.Fatalf("a plain non-zero exit must not be misreported as signal death/timeout:\n%s", out.Diagnostics())
	}
	if !strings.Contains(out.Stdout, exitMarker) || !strings.Contains(out.Stderr, exitMarker) {
		t.Fatalf("the abnormal exit's stdout/stderr must preserve recognizable output:\n%s", out.Diagnostics())
	}
}

// TestRunTimeoutKillReportedAsFailure 验收超时语义:挂起子进程在 3s
// 短超时后被外部杀死,结果报告为失败(TimedOut 置位,Unix 上 Signaled
// 置位;Windows 的 TerminateProcess 不产生信号),
// 已产出的标记保留——超时不是通过,也不得 recover 后继续或自动重试。
// 本测试断言该机制工作,测试本身通过。
func TestRunTimeoutKillReportedAsFailure(t *testing.T) {
	const timeout = 3 * time.Second
	out := ffitest.Run(t, ffitest.Options{Mode: modeHang, Timeout: timeout})
	if out.Err == nil {
		t.Fatalf("the hanging child must be reported as a failure after the %s timeout, got exit code 0:\n%s", timeout, out.Diagnostics())
	}
	if !out.TimedOut {
		t.Fatalf("the failure must be identifiable as a timeout kill (TimedOut=false, err=%v):\n%s", out.Err, out.Diagnostics())
	}
	if out.ExitCode == 0 {
		t.Fatalf("an externally killed child must not report a successful exit:\n%s", out.Diagnostics())
	}
	if runtime.GOOS == "windows" && out.Signaled {
		t.Fatalf("a Windows timeout kill must not report Unix signal death:\n%s", out.Diagnostics())
	}
	if runtime.GOOS != "windows" && !out.Signaled {
		t.Fatalf("the timeout kill must kill the child via a signal (Signaled=false, err=%v):\n%s", out.Err, out.Diagnostics())
	}
	if !strings.Contains(out.Stdout, hangMarker) {
		t.Fatalf("timeout diagnostics must preserve the output the child already produced:\n%s", out.Diagnostics())
	}
	t.Logf("the hanging child was externally killed on the %s timeout as expected; the executor reported a failure and kept diagnostics:\n%s",
		timeout, out.Diagnostics())
}

// TestRunUnknownModeFailsFast 验收未知模式:子进程以 ExitUnknownMode
// 退出并在 stderr 打印含模式名的诊断——配置错误 fail-fast,不伪装
// 成通过。
func TestRunUnknownModeFailsFast(t *testing.T) {
	const missing = "no-such-mode-in-ffitest"
	out := ffitest.Run(t, ffitest.Options{Mode: missing, Timeout: childTimeout})
	if out.Err == nil {
		t.Fatalf("an unknown mode must be reported as a failure:\n%s", out.Diagnostics())
	}
	if out.ExitCode != ffitest.ExitUnknownMode {
		t.Fatalf("unknown mode exit code = %d, want %d:\n%s",
			out.ExitCode, ffitest.ExitUnknownMode, out.Diagnostics())
	}
	if !strings.Contains(out.Stderr, missing) {
		t.Fatalf("the unknown mode's stderr diagnostics must contain the mode name %q:\n%s", missing, out.Diagnostics())
	}
	if out.Signaled || out.TimedOut {
		t.Fatalf("an unknown mode is a controlled fail-fast exit and must not be misreported as signal death/timeout:\n%s", out.Diagnostics())
	}
}

// TestRunExtraEnvPassedThrough 验收 ExtraEnv 传递:子进程读到的变量值
// 与父进程传入一致(值含空格,验证不被截断或改写)。
func TestRunExtraEnvPassedThrough(t *testing.T) {
	out := ffitest.Run(t, ffitest.Options{
		Mode:     modeEchoEnv,
		Timeout:  childTimeout,
		ExtraEnv: map[string]string{echoEnvName: echoEnvValue},
	})
	if out.Err != nil {
		t.Fatalf("echo-env child failed:\n%s", out.Diagnostics())
	}
	want := fmt.Sprintf("%s %s=%q", echoEnvMarker, echoEnvName, echoEnvValue)
	if !strings.Contains(out.Stdout, want) {
		t.Fatalf("stdout does not contain %q (ExtraEnv not applied or rewritten):\n%s", want, out.Diagnostics())
	}
}

// inheritedEnvValue 与 overrideEnvValue 是 ExtraEnv 覆盖语义用例的
// 父进程继承值与覆盖值:经 t.Setenv 供子进程继承,再以 ExtraEnv 同名
// 键覆盖。
const (
	inheritedEnvValue = "inherited-from-parent-env"
	overrideEnvValue  = "override-from-extra-env"
)

// TestRunExtraEnvOverridesInheritedValue 验收 ExtraEnv 的覆盖语义:父
// 进程经 t.Setenv 提供同名继承值,子进程必须读到 ExtraEnv 的覆盖值而
// 非继承值——覆盖在构造环境时显式去重保证,不依赖 environ 重复条目
// 的解析顺序。
func TestRunExtraEnvOverridesInheritedValue(t *testing.T) {
	t.Setenv(echoEnvName, inheritedEnvValue)
	out := ffitest.Run(t, ffitest.Options{
		Mode:     modeEchoEnv,
		Timeout:  childTimeout,
		ExtraEnv: map[string]string{echoEnvName: overrideEnvValue},
	})
	if out.Err != nil {
		t.Fatalf("echo-env child failed:\n%s", out.Diagnostics())
	}
	want := fmt.Sprintf("%s %s=%q", echoEnvMarker, echoEnvName, overrideEnvValue)
	if !strings.Contains(out.Stdout, want) {
		t.Fatalf("the child should read the ExtraEnv override value %q:\n%s", overrideEnvValue, out.Diagnostics())
	}
	if strings.Contains(out.Stdout, inheritedEnvValue) {
		t.Fatalf("the child read the parent-inherited value %q; the override did not take effect:\n%s", inheritedEnvValue, out.Diagnostics())
	}
}

// TestRunChildModeEnvExclusiveOverExtraEnv 验收模式变量独占:ExtraEnv
// 中试图篡改 ChildModeEnv 的条目必须被忽略,分派仍按 Options.Mode 走
// echo-env(退出 0、打印标记);若条目生效,子进程将分派 modeExit 并
// 以 42 退出。
func TestRunChildModeEnvExclusiveOverExtraEnv(t *testing.T) {
	out := ffitest.Run(t, ffitest.Options{
		Mode:    modeEchoEnv,
		Timeout: childTimeout,
		ExtraEnv: map[string]string{
			ffitest.ChildModeEnv: modeExit,
			echoEnvName:          echoEnvValue,
		},
	})
	if out.Err != nil {
		t.Fatalf("the ChildModeEnv entry in ExtraEnv must be ignored; dispatch follows Options.Mode:\n%s", out.Diagnostics())
	}
	want := fmt.Sprintf("%s %s=%q", echoEnvMarker, echoEnvName, echoEnvValue)
	if !strings.Contains(out.Stdout, want) {
		t.Fatalf("echo-env child exited 0 but stdout is missing the marker (dispatch may have been tampered with):\n%s", out.Diagnostics())
	}
}

// TestRegisterContractPanics 验收 Register 的编程错误契约:重复 mode、
// nil run 与空 mode 一律 panic——注册期炸掉比把错误推迟到用例运行时
// 安全。
func TestRegisterContractPanics(t *testing.T) {
	assertPanics(t, "duplicate mode", func() {
		ffitest.Register(modeOK, func() int { return 0 })
	})
	assertPanics(t, "nil run", func() {
		ffitest.Register("contract-nil-run", nil)
	})
	assertPanics(t, "empty mode", func() {
		ffitest.Register("", func() int { return 0 })
	})
}

// TestRunRequiresPositiveTimeout 验收 Run 的超时契约:Timeout<=0 一律
// panic,无超时的 FFI 用例不允许存在(panic 发生在任何子进程启动
// 之前)。
func TestRunRequiresPositiveTimeout(t *testing.T) {
	assertPanics(t, "Timeout=0", func() {
		ffitest.Run(t, ffitest.Options{Mode: modeOK})
	})
	assertPanics(t, "Timeout<0", func() {
		ffitest.Run(t, ffitest.Options{Mode: modeOK, Timeout: -time.Second})
	})
}

// TestRegisterConcurrentDistinctModes 验收 Register 的并发安全:并发
// 注册互不相同的模式不 panic、不丢注册,注册结果经 Dispatch 在进程
// 内可见。子进程分派可见性(子进程只执行包 init 与 TestMain)由其余
// 用例的 init 注册模式覆盖;运行期注册本就不对子进程可见,见包注释
// 「使用方式」。Dispatch 在本测试中仅用于进程内验证,不 os.Exit。
func TestRegisterConcurrentDistinctModes(t *testing.T) {
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ffitest.Register(fmt.Sprintf("race-mode-%d", i), func() int { return 0 })
		}(i)
	}
	wg.Wait()
	t.Setenv(ffitest.ChildModeEnv, "race-mode-3")
	code, handled := ffitest.Dispatch()
	if !handled {
		t.Fatalf("Dispatch did not dispatch the set mode %q", ffitest.ChildModeEnv+"=race-mode-3")
	}
	if code != 0 {
		t.Fatalf("race-mode-3 registered concurrently returned exit code %d via Dispatch, want 0", code)
	}
}

// parseChildPID 从 ok 子进程的标记行提取 PID。
func parseChildPID(t *testing.T, stdout string) int {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		rest, ok := strings.CutPrefix(line, okMarker+" ")
		if !ok {
			continue
		}
		rest, ok = strings.CutPrefix(strings.TrimSpace(rest), "pid=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			t.Fatalf("the PID in the ok child's marker line is not a decimal integer %q: %v", line, err)
		}
		return pid
	}
	t.Fatalf("stdout is missing the %s pid=... marker line:\n%s", okMarker, stdout)
	return 0
}

// assertPanics 断言 fn 按契约 panic,并记录 panic 值供排查。
func assertPanics(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s did not panic as required by the contract", what)
		} else {
			t.Logf("%s panicked as required by the contract: %v", what, r)
		}
	}()
	fn()
}
