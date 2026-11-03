// Package ffitest 提供「每个 FFI 用例一个新进程」的通用子进程执行器,
// 为 maa-go 的 FFI 测试与同类用例服务。
//
// # 背景与边界
//
// 本包是测试辅助包:只应被各包的 _test.go 引用,生产代码不得 import;
// API 面向测试场景,不属于生产 API,没有兼容性承诺。纯 Go 实现,
// CGO_ENABLED=0 下必须可构建,不引入 cgo。FFI 用例的原生崩溃(SIGSEGV、
// abort)与死锁在同一进程内既无法 recover 也无法超时打断,因此每个用例
// 都在全新子进程中执行,使崩溃与死锁成为父进程可见的失败(信号死亡 /
// 超时杀死),而不是挂起或静默通过。
//
// # 失败语义
//
//   - 超时强制是核心契约:Run 的 Options.Timeout 必须 >0,超时由父进程
//     外部强制,结果报告为失败(TimedOut/Signaled 置位),不是通过。
//   - 不重试、不 recover:cmd.Run 恰好执行一次,错误原样返回,失败判定
//     由调用方完成;注册的 run 内部 panic 同样不被捕获。
//   - Dispatch 对已设置但未注册的模式向 stderr 打印诊断并以
//     ExitUnknownMode 退出:配置错误 fail-fast,不伪装成通过。
//
// # 使用方式
//
// 使用方在自己包的 TestMain 中、m.Run() 之前调用 Dispatch:
//
//	func TestMain(m *testing.M) {
//	    if code, handled := ffitest.Dispatch(); handled {
//	        os.Exit(code)
//	    }
//	    os.Exit(m.Run())
//	}
//
// 各子进程模式必须经 init() 调用 Register 注册:子进程只执行包初始化与
// TestMain 的分派,不执行任何测试函数,运行期注册的模式对子进程不可见。
// 父进程用例经 Run 执行并断言 Outcome,失败时以 Diagnostics 附上子进程
// 完整输出。
//
// # 环境策略
//
// 子进程继承父进程全量环境,ExtraEnv 同名键覆盖继承值,模式变量由执行器
// 独占。平台特定的动态链接器变量策略不属于通用执行器:需要同类策略的
// 用例应经 ExtraEnv 显式处理,本包不隐式改写继承环境。
//
// # 子进程约束
//
// 子进程不得派生会持有 stdout/stderr 管道的后代进程:否则 cmd.Wait 会
// 阻塞到 WaitDelay(waitDelay 常量)耗尽,之后返回包装 ErrWaitDelay 的
// 错误——仍是可见失败(Err 非 nil),但类别上不是 TimedOut;该延迟只是
// 兜底,不是功能。
package ffitest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ChildModeEnv = "FFITEST_CHILD_MODE"

	ExitUnknownMode = 70
)

const waitDelay = 30 * time.Second

var registry = struct {
	mu    sync.Mutex
	modes map[string]func() int
}{modes: make(map[string]func() int)}

func Register(mode string, run func() int) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if mode == "" {
		panic("ffitest: Register rejects an empty mode name")
	}
	if run == nil {
		panic(fmt.Sprintf("ffitest: Register(%q) has a nil run", mode))
	}
	if _, dup := registry.modes[mode]; dup {
		panic(fmt.Sprintf("ffitest: mode %q is registered twice", mode))
	}
	registry.modes[mode] = run
}

func Dispatch() (code int, handled bool) {
	mode, ok := os.LookupEnv(ChildModeEnv)
	if !ok {
		return 0, false
	}
	registry.mu.Lock()
	run := registry.modes[mode]
	registry.mu.Unlock()
	if run == nil {
		fmt.Fprintf(os.Stderr,
			"ffitest: child-process mode %q is not registered (%s); check the case's Options.Mode against its init() registration\n",
			mode, ChildModeEnv)
		return ExitUnknownMode, true
	}
	return run(), true
}

type Options struct {
	Mode string

	Timeout time.Duration

	ExtraEnv map[string]string
}

type Outcome struct {
	Stdout string
	Stderr string

	Err error

	TimedOut bool

	Signaled bool

	ExitCode int

	mode string
}

func (o Outcome) Diagnostics() string {
	var b strings.Builder
	prefix := ""
	if o.mode != "" {
		prefix = "mode=" + o.mode + " "
	}
	switch {
	case o.Err == nil:
		fmt.Fprintf(&b, "%sexit code 0\n", prefix)
	case o.TimedOut:
		fmt.Fprintf(&b, "%skilled on timeout (context deadline exceeded): %v\n", prefix, o.Err)
	case o.Signaled:
		fmt.Fprintf(&b, "%skilled by signal: %v\n", prefix, o.Err)
	default:
		fmt.Fprintf(&b, "%snon-zero exit (exit code %d): %v\n", prefix, o.ExitCode, o.Err)
	}
	fmt.Fprintf(&b, "--- stdout ---\n%s\n--- stderr ---\n%s", o.Stdout, o.Stderr)
	return b.String()
}

func Run(t *testing.T, opts Options) Outcome {
	t.Helper()
	if opts.Timeout <= 0 {
		panic(fmt.Sprintf("ffitest: Run(%q) requires Timeout >0, got %s (timeout enforcement is the core contract; execution without a timeout is not allowed)",
			opts.Mode, opts.Timeout))
	}
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("failed to locate the test binary %q: %v", os.Args[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
	cmd.Env = childEnviron(opts.ExtraEnv, opts.Mode)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()
	out := Outcome{Stdout: stdout.String(), Stderr: stderr.String(), Err: runErr, mode: opts.Mode}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.TimedOut = true
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		out.ExitCode = ee.ExitCode()
		out.Signaled = exitSignaled(ee)
	}
	return out
}

func childEnviron(extra map[string]string, mode string) []string {
	override := make(map[string]struct{}, len(extra)+1)
	for k := range extra {
		if k == ChildModeEnv {
			continue
		}
		override[k] = struct{}{}
	}
	env := make([]string, 0, len(os.Environ())+len(extra)+1)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if _, dup := override[key]; dup || key == ChildModeEnv {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		if k == ChildModeEnv {
			continue
		}
		env = append(env, k+"="+v)
	}
	return append(env, ChildModeEnv+"="+mode)
}
