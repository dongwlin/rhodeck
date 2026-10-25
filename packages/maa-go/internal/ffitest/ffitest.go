// Package ffitest 提供「每个 FFI 用例一个新进程」的通用子进程执行器,
// 为 maa-go 后续 FFI 测试(MAA-12/13)与 M2+ 阶段的同类用例服务。
//
// # 背景与边界
//
// 本包是测试辅助包:只应被各包的 _test.go 引用,生产代码不得
// import;API 面向测试场景,不属于生产 API,没有兼容性承诺。纯 Go
// 实现,CGO_ENABLED=0 下必须可构建,不引入 cgo。
//
// FFI 用例的原生崩溃(SIGSEGV、abort)与死锁在同一进程内既无法
// recover 也无法超时打断,只会拖死整个测试二进制。因此每个用例都在
// 全新子进程中执行:父进程以 go test 自身二进制重执行
// (-test.run=^$),子进程在 TestMain 中先经 Dispatch 按环境变量
// ChildModeEnv 分派到注册的入口,不进入测试框架。崩溃与死锁由此
// 成为父进程可见的失败(信号死亡 / 超时杀死),而不是挂起或静默
// 通过。
//
// # 失败语义
//
//   - 超时强制是核心契约:Run 的 Options.Timeout 必须 >0,超时经
//     exec.CommandContext 由父进程外部强制(不依赖子进程自觉),
//     结果报告为失败(TimedOut/Signaled 置位),不是通过。
//   - 不重试、不 recover:cmd.Run 恰好执行一次,错误原样返回,失败
//     判定由调用方完成;注册的 run 内部 panic 同样不被捕获,子进程
//     以 Go 默认方式崩溃退出,崩溃本身即可见失败。
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
// 各子进程模式必须经 init() 调用 Register 注册:子进程与父进程是同
// 一测试二进制,但只执行包初始化与 TestMain 的分派,不执行任何测试
// 函数,运行期注册的模式对子进程不可见。父进程用例经 Run 执行并
// 断言 Outcome,失败时以 Diagnostics 附上子进程完整输出。分派发生在
// m.Run() 之前,go test 自身的超时计时器在子进程内不会启动,唯一的
// 超时执行来自 Run 的 Timeout。
//
// # 环境策略
//
// 子进程继承父进程全量环境,ExtraEnv 同名键覆盖继承值,模式变量由
// 执行器独占。MAA-08 验收(acceptance_linux_test.go)对 LD_* 动态
// 链接器变量的剥离是真实库用例的锁定策略,不属于通用执行器:需要
// 同类策略的用例应经 ExtraEnv 显式处理,或等待后续字段扩展,本包
// 不隐式改写继承环境。
//
// # 子进程约束
//
// 子进程不得派生会持有 stdout/stderr 管道的后代进程:子进程退出后,
// 持管道的孙进程会让 cmd.Wait 一直阻塞到 WaitDelay(waitDelay 常量)
// 耗尽,把快用例拖成超时失败;该延迟只是兜底,不是功能。
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

// 子进程分派机制与失败退出码的契约常量。
const (
	// ChildModeEnv 是子进程模式环境变量:Dispatch 读它分派,Run 经它
	// 向子进程传递模式名。该变量由执行器独占,ExtraEnv 中的同名条目
	// 被忽略。
	ChildModeEnv = "FFITEST_CHILD_MODE"

	// ExitUnknownMode 是子进程收到「已设置但未注册」模式时的退出码:
	// 取 sysexits(3) 的 EX_SOFTWARE(70),语义为内部软件状态非法。
	// 未注册的模式即编程错误,fail-fast 暴露,不伪装成通过。
	ExitUnknownMode = 70
)

// waitDelay 是 cmd.WaitDelay 的取值:子进程退出后若仍有孙进程持有
// stdout/stderr 管道,Wait 至多再等这么久,防止执行器被别人的后代
// 挂死(契约上子进程不应有此类后代,见包注释「子进程约束」)。
const waitDelay = 30 * time.Second

// registry 保存子进程模式注册表。注册发生在各测试文件的 init() 中,
// 必然先于任何子进程分派(子进程与父进程是同一测试二进制,init 同
// 样先于 TestMain 的 Dispatch)。
var registry = struct {
	mu    sync.Mutex
	modes map[string]func() int
}{modes: make(map[string]func() int)}

// Register 注册一个子进程模式:run 在子进程经 Dispatch 分派后执行,
// 返回值即子进程退出码。重复 mode、nil run 与空 mode 都是编程错误
// ——注册期静默吞掉只会把错误推迟到用例运行时,故直接 panic。并发
// 安全;必须在 init() 中完成注册:分派先于一切,且子进程只执行包
// 初始化与 TestMain,运行期注册对子进程不可见。
func Register(mode string, run func() int) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if mode == "" {
		panic("ffitest: Register 拒绝空模式名")
	}
	if run == nil {
		panic(fmt.Sprintf("ffitest: Register(%q) 的 run 为 nil", mode))
	}
	if _, dup := registry.modes[mode]; dup {
		panic(fmt.Sprintf("ffitest: 模式 %q 重复注册", mode))
	}
	registry.modes[mode] = run
}

// Dispatch 是子进程分派入口,由使用方的 TestMain 在 m.Run() 之前调用:
//
//   - ChildModeEnv 未设置:返回 (0, false),调用方照常执行测试框架
//     (父进程路径);
//   - 已设置且已注册:执行注册的 run 并返回 (退出码, true),调用方
//     应立即 os.Exit;
//   - 已设置但未注册:向 stderr 打印含模式名的诊断,返回
//     (ExitUnknownMode, true)——配置错误快速可见,不进入测试框架。
//
// 分派发生在 m.Run() 之前,go test 自身的超时计时器在子进程内不会
// 启动;子进程的超时执行完全来自父进程 Run 的外部强制。
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
			"ffitest: 子进程模式 %q 未注册(%s);检查用例的 Options.Mode 与 init() 注册是否一致\n",
			mode, ChildModeEnv)
		return ExitUnknownMode, true
	}
	return run(), true
}

// Options 描述一次子进程用例的执行参数。
type Options struct {
	// Mode 是子进程模式名,必须已经 Register(应为非空:空模式名会在
	// 子进程侧落入「已设置但未注册」路径,以 ExitUnknownMode 退出并在
	// stderr 留下诊断)。
	Mode string

	// Timeout 是父进程强制的 wall-clock 上限,必须 >0,否则 Run panic:
	// 超时杀死被报告为失败(死锁必须成为可见失败),无超时执行不存在。
	Timeout time.Duration

	// ExtraEnv 是追加到子进程环境的额外变量,同名键覆盖父进程继承值
	// (覆盖语义在构造环境时显式去重保证,不依赖 environ 重复条目的
	// 解析顺序)。平台特定策略(如 MAA-08 对 LD_* 变量的剥离)不属于
	// 通用执行器,由调用方经本字段或后续扩展字段自理。ChildModeEnv 由
	// 执行器独占,本字段中的同名条目被忽略。
	ExtraEnv map[string]string
}

// Outcome 是一次子进程执行的完整结果:exec 错误原样保留,失败类别以
// TimedOut/Signaled 区分「超时杀死」「信号死亡」与普通非零退出,
// stdout/stderr 供调用方附上诊断。
type Outcome struct {
	// Stdout 与 Stderr 是子进程输出;超时/信号死亡时保留已产出部分。
	Stdout string
	Stderr string

	// Err 是 cmd.Run 的原始错误;nil 表示退出码 0。不重试、不包装,
	// 失败判定由调用方完成。
	Err error

	// TimedOut 表示父 context 截止(DeadlineExceeded),子进程被外部
	// 强制终止。
	TimedOut bool

	// Signaled 表示进程被信号杀死(含超时 SIGKILL 与原生崩溃
	// SIGSEGV/SIGABRT),此时 ExitCode 为 -1。
	Signaled bool

	// ExitCode 是正常退出时的退出码(含非零失败);被信号杀死时为
	// -1。Err 非 nil 但非 *exec.ExitError(如二进制启动失败)时保持
	// 0,此时以 Err 非 nil 为失败判据。
	ExitCode int

	// mode 记录执行时的模式名,仅供 Diagnostics 渲染;零值 Outcome 的
	// 诊断省略 mode 前缀。
	mode string
}

// Diagnostics 渲染子进程诊断:错误类别(超时/信号/退出码)+ 完整
// stdout/stderr,保证每种失败情形都留下可辨识证据,供调用方拼进
// t.Fatalf 消息。
func (o Outcome) Diagnostics() string {
	var b strings.Builder
	prefix := ""
	if o.mode != "" {
		prefix = "mode=" + o.mode + " "
	}
	switch {
	case o.Err == nil:
		fmt.Fprintf(&b, "%s退出码 0\n", prefix)
	case o.TimedOut:
		fmt.Fprintf(&b, "%s超时被杀死(context deadline exceeded): %v\n", prefix, o.Err)
	case o.Signaled:
		fmt.Fprintf(&b, "%s被信号杀死: %v\n", prefix, o.Err)
	default:
		fmt.Fprintf(&b, "%s非零退出(退出码 %d): %v\n", prefix, o.ExitCode, o.Err)
	}
	fmt.Fprintf(&b, "--- stdout ---\n%s\n--- stderr ---\n%s", o.Stdout, o.Stderr)
	return b.String()
}

// Run 在全新子进程中执行一次 opts.Mode 用例,返回原始执行结果。实现
// 与 MAA-08 的内联执行器(acceptance_linux_test.go 的 runM1Child)
// 同源:以 go test 自身二进制(os.Args[0])重执行并传 -test.run=^$,
// 子进程在 TestMain 经 Dispatch 分派,不进入测试框架。不重试、不
// recover:cmd.Run 恰好执行一次,结果原样交回调用方判定——原生崩溃
// 与死锁必须是可见失败。
//
// Timeout 必须 >0,否则 panic:超时强制是本执行器的核心契约,经
// exec.CommandContext 由父进程外部实施,不允许无超时的 FFI 用例。
// t 用于报告二进制定位失败等基础设施错误;用例结果的成败断言由调用
// 方基于返回值完成。
func Run(t *testing.T, opts Options) Outcome {
	t.Helper()
	if opts.Timeout <= 0 {
		panic(fmt.Sprintf("ffitest: Run(%q) 的 Timeout 必须 >0,得到 %s(超时强制是核心契约,不允许无超时执行)",
			opts.Mode, opts.Timeout))
	}
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("定位测试二进制 %q 失败: %v", os.Args[0], err)
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
		out.ExitCode = ee.ExitCode() // 被信号杀死时为 -1
		out.Signaled = exitSignaled(ee)
	}
	return out
}

// childEnviron 构造子进程环境:父进程全量继承;ExtraEnv 同名键先从
// 继承值中剔除再追加,使覆盖语义不依赖 environ 重复条目的解析顺序;
// 模式变量最后追加且独占(剔除继承值与本字段中的任何同名条目)。
// MAA-08 的 LD_* 剥离是真实库用例的锁定策略,不在通用执行器内隐式
// 执行,理由见 Options.ExtraEnv 的文档。
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
