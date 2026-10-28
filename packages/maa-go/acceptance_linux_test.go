//go:build linux

// 本文件验证真实 MaaCore 的加载与元数据读取:从锁定发行包缓存流式解包,
// 在子进程中经公开 Open 完成 libMaaCore.so 加载、Version/NullSize 读取,
// 并验证缺库失败的生命周期语义与执行器超时语义。
package maa

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 锁定基线(证据冻结):发行包、libMaaCore.so 与
// resource/version.json 的 SHA256,以及 Core 预期版本字符串。任何
// 一项不符都是测试失败(锁定基线被破坏),不是 skip。
const (
	lockedTarballSHA256  = "814a128d24c8183b38003284facd56789fe8346b8a66932f9ff885df48d48bc1"
	lockedCoreSHA256     = "1e44dbf35a9ad84a62faa90af34412f8ed038f6dcb6de6db5a8eb016902bbb85"
	lockedVersionSHA256  = "3178eda0c620b87a0c91b02f92da40fbc603230fb19135ad0adb37258873ecd1"
	lockedCoreVersion    = "v6.20.1"
	lockedCoreSOName     = "libMaaCore.so"
	lockedVersionRelPath = "resource/version.json"
)

// 子进程分派与资源传递的环境变量。
const (
	childModeEnv      = "MAA_CORE_CHILD_MODE"     // 子进程模式标记;缺省即父进程,照常执行测试框架
	childModeSuccess  = "success"                 // 真实库成功路径:公开 Open → 元数据断言 → 打印 capabilities
	childModeMissing  = "missing-lib"             // 缺库失败路径:errors.As 命中 LibraryLoadError → 同进程再 Open 得 terminated
	childModeHang     = "hang"                    // 挂起模式:长时间 sleep,供父进程验收超时终止语义
	libPathEnv        = "MAA_CORE_LIB_PATH"       // 解包出的 libMaaCore.so 绝对路径
	resourceRootEnv   = "MAA_CORE_RESOURCE_ROOT"  // 解包根目录绝对路径(其下含 resource/)
	userDirEnv        = "MAA_CORE_USER_DIR"       // 子进程专用的可写用户目录绝对路径
	tarballOverrideEv = "MAA_CORE_TARBALL"        // 测试专用:覆盖锁定发行包缓存路径
	successMarker     = "MAA_CORE_SUCCESS"        // success 子进程的 stdout 成功标记
	missingOKMarker   = "MAA_CORE_MISSING_LIB_OK" // missing-lib 子进程的 stdout 成功标记
	hangMarker        = "MAA_CORE_HANG"           // hang 子进程的 stdout 启动标记
)

// errTarballMissing 表示锁定发行包缓存缺失:真实库验收受阻,计为
// SKIP——跳过不是通过,不得冒充真实环境验收。
var errTarballMissing = errors.New("locked release tarball missing")

// 子进程超时参数:Core 加载约 280MB 资源需要时间,成功路径给足
// 10 分钟;缺库路径 dlopen 立即失败,同样给 10 分钟以免慢环境误杀;
// 挂起路径用 15 秒短超时,专门验收「超时杀死被报告为失败」的机制。
const (
	successChildTimeout = 10 * time.Minute
	missingChildTimeout = 10 * time.Minute
	hangChildTimeout    = 15 * time.Second
	childWaitDelay      = 30 * time.Second
	hangSleepDuration   = 30 * time.Minute
)

// TestMain 是包内首个 TestMain:带 childModeEnv 标记时按模式分派并
// os.Exit,不执行测试框架;无标记时照常 m.Run(),退出前清理父进程
// 解包的临时目录。分派不改变既有用例的可见行为。
func TestMain(m *testing.M) {
	switch os.Getenv(childModeEnv) {
	case childModeSuccess:
		os.Exit(runSuccessChild())
	case childModeMissing:
		os.Exit(runMissingLibChild())
	case childModeHang:
		os.Exit(runHangChild())
	}
	code := m.Run()
	cleanupUnpackedResources()
	os.Exit(code)
}

// unpackState 汇集父进程的锁定资源解包结果:once 保证每父进程至多
// 解包一次(惰性共享);err 为 errTarballMissing 时计为 skip,其余
// 错误(哈希不符、成员缺失、IO 失败)都是测试失败。
var unpackState struct {
	once    sync.Once
	root    string // 解包根目录(其下平铺 *.so* 并含 resource/);"" 表示未解包
	libPath string // 解包出的 libMaaCore.so 绝对路径
	err     error
}

// tarballPath 返回锁定发行包缓存路径:默认用户缓存,可经
// tarballOverrideEv 覆盖(测试专用,不改任何全局状态)。
func tarballPath() string {
	if p := os.Getenv(tarballOverrideEv); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "maa", "MAA-v6.20.1-linux-x86_64.tar.gz")
}

// cleanupUnpackedResources 在 TestMain 末尾移除父进程解包的临时目录;
// 未解包过(或仅子进程模式)时不做任何事。
func cleanupUnpackedResources() {
	if unpackState.root != "" {
		os.RemoveAll(unpackState.root)
	}
}

// requireUnpackedResources 为父进程用例取得锁定资源:非 linux/amd64
// 运行环境跳过(构建约束已限定 linux);缓存缺失跳过并写明受阻原因;
// 其余解包/校验失败一律 Fatal——锁定基线被破坏不是环境受限。
// 返回 libMaaCore.so 绝对路径与资源根(解包根目录,其下含 resource/)。
func requireUnpackedResources(t *testing.T) (libPath, resourceRoot string) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skipf("real-library admission blocked: architecture %s is not in the locked baseline (linux amd64)", runtime.GOARCH)
	}
	unpackState.once.Do(unpackLockedRelease)
	if errors.Is(unpackState.err, errTarballMissing) {
		t.Skipf("real-library admission blocked: locked release tarball missing (%s)", tarballPath())
	}
	if unpackState.err != nil {
		t.Fatalf("locked release tarball unpack/verification failed: %v", unpackState.err)
	}
	return unpackState.libPath, unpackState.root
}

// unpackLockedRelease 执行一次性的锁定资源准备:
//  1. 核验整包 SHA256;
//  2. 流式解包(top-level *.so* 全部 + resource/ 整目录)到
//     os.MkdirTemp 临时目录,成员逐项过滤并拒绝路径穿越;
//  3. 核验解出的 libMaaCore.so 与 resource/version.json 的 SHA256。
//
// 任何失败都记录进 unpackState.err,由调用方决定 skip 或 Fatal。
func unpackLockedRelease() {
	tarball := tarballPath()
	if tarball == "" {
		unpackState.err = fmt.Errorf("%w: cannot locate the user home directory", errTarballMissing)
		return
	}
	if _, err := os.Stat(tarball); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			unpackState.err = fmt.Errorf("%w: %s", errTarballMissing, tarball)
			return
		}
		unpackState.err = fmt.Errorf("stat %s: %w", tarball, err)
		return
	}

	// 整包哈希:先全量读一遍核对,再打开解包。
	got, err := sha256File(tarball)
	if err != nil {
		unpackState.err = fmt.Errorf("hash %s: %w", tarball, err)
		return
	}
	if got != lockedTarballSHA256 {
		unpackState.err = fmt.Errorf("release tarball SHA256 = %s, want %s (locked baseline broken)", got, lockedTarballSHA256)
		return
	}

	root, err := os.MkdirTemp("", "maa-m1-accept-")
	if err != nil {
		unpackState.err = fmt.Errorf("MkdirTemp: %w", err)
		return
	}
	unpackState.root = root

	if err := extractLockedMembers(root, tarball); err != nil {
		unpackState.err = err
		return
	}
	if unpackState.libPath == "" {
		unpackState.err = fmt.Errorf("%s not found in the release tarball", lockedCoreSOName)
		return
	}
	if !sawLockedVersionJSON {
		unpackState.err = fmt.Errorf("%s not found in the release tarball", lockedVersionRelPath)
		return
	}
}

// sawLockedVersionJSON 记录解包过程中是否流过 resource/version.json
// (extractLockedMembers 成员核验通过后置位)。
var sawLockedVersionJSON bool

// extractLockedMembers 流式解包锁定成员:tar+gzip 逐成员过滤,顶层
// *.so* 全部保留、resource/ 整目录保留,其余成员(头文件、Python、
// maa 目录等)跳过。libMaaCore.so 与 resource/version.json 边复制边
// 算哈希并当场核对。本发行包只含普通文件与目录(无符号链接/硬链接),
// 其余成员类型按锁定基线破坏处理,不猜测语义。
func extractLockedMembers(root, tarball string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return fmt.Errorf("open %s: %w", tarball, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip %s: %w", tarball, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || !lockedMemberWanted(name) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(name)), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", name, err)
			}
		case tar.TypeReg:
			dest, err := safeJoin(root, name)
			if err != nil {
				return fmt.Errorf("member %s has an illegal path: %w", name, err)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("mkdir for %s: %w", name, err)
			}
			pinned := ""
			switch name {
			case lockedCoreSOName:
				pinned = lockedCoreSHA256
			case lockedVersionRelPath:
				pinned = lockedVersionSHA256
			}
			if err := extractFile(tr, dest, hdr.FileInfo().Mode().Perm(), pinned); err != nil {
				return fmt.Errorf("extract %s: %w", name, err)
			}
			switch name {
			case lockedCoreSOName:
				unpackState.libPath = dest
			case lockedVersionRelPath:
				sawLockedVersionJSON = true
			}
		default:
			return fmt.Errorf("member %s has type %q outside the locked baseline (only regular files and directories)", name, hdr.Typeflag)
		}
	}
}

// lockedMemberWanted 报告成员是否属于解包集合:顶层 *.so* 全部
// (libMaaCore.so 与 8 个捆绑依赖,DT_RUNPATH=$ORIGIN/ 依赖同目录
// 解析)或 resource/ 整目录。拒绝绝对路径与路径穿越分量。
func lockedMemberWanted(name string) bool {
	if strings.HasPrefix(name, "/") || name == ".." || strings.Contains(name, "/../") || strings.HasSuffix(name, "/..") {
		return false
	}
	if !strings.Contains(name, "/") {
		return strings.HasPrefix(name, "lib") && (strings.HasSuffix(name, ".so") || strings.Contains(name, ".so."))
	}
	return strings.HasPrefix(name, "resource/")
}

// safeJoin 把 tar 成员名安全地拼接进 root:拒绝绝对路径与任何..
// 分量,防止路径穿越逃出解包目录。
func safeJoin(root, name string) (string, error) {
	if !filepath.IsAbs(name) && !strings.HasPrefix(name, "..") && !strings.Contains(name, "/../") {
		return filepath.Join(root, filepath.FromSlash(name)), nil
	}
	return "", fmt.Errorf("refusing unsafe member path %q", name)
}

// extractFile 把当前 tar 成员复制到 dest:mode 取 tar 头权限;
// want 为非空时边复制边计算 SHA256 并当场核对(锁定成员哈希不符
// 即失败)。复制完成并核验后才保留文件。
func extractFile(r io.Reader, dest string, mode fs.FileMode, want string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if want == "" {
		_, err := io.Copy(out, r)
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), r); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA256 = %s, want %s (locked baseline broken)", got, want)
	}
	return nil
}

// sha256File 计算文件的 SHA256 十六进制串。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// childOutcome 是一次子进程执行的完整结果:exec 错误原样保留,
// timedOut/signaled 区分「被超时杀死」「被信号杀死」与普通非零
// 退出,stdout/stderr 供父测试附上诊断。
type childOutcome struct {
	stdout   string
	stderr   string
	err      error // cmd.Run 的原始错误;nil 表示退出码 0
	timedOut bool  // 因父进程 context 截止而被杀死
	signaled bool  // 进程被信号杀死(含超时 SIGKILL)
	exitCode int   // 正常退出时的退出码;被信号杀死时无意义
}

// runCoreOpenChild 以 go test 自身二进制重执行一个子进程:子进程经
// childModeEnv 在 TestMain 分派,不进入测试框架。超时经
// exec.CommandContext 外部强制(超时杀死是明确的失败,不是通过);
// WaitDelay 防止孙进程持有输出管道导致 Wait 挂起。不重试、不
// recover:执行结果原样交回调用方判定。
func runCoreOpenChild(t *testing.T, timeout time.Duration, mode string, extraEnv map[string]string) childOutcome {
	t.Helper()
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("failed to locate the test binary %q: %v", os.Args[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
	cmd.Env = childEnviron(mode)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = childWaitDelay

	runErr := cmd.Run()
	out := childOutcome{stdout: stdout.String(), stderr: stderr.String(), err: runErr}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		out.timedOut = true
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		out.exitCode = ee.ExitCode() // 被信号杀死时为 -1
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			out.signaled = true
		}
	}
	return out
}

// childEnviron 构造子进程环境:父进程环境剥离动态链接器注入变量
// (LD_LIBRARY_PATH/LD_PRELOAD/LD_AUDIT)后原样继承,再附上模式标记。
// 锁定策略是不依赖任何 LD_* 配置——libMaaCore.so 的依赖只经
// $ORIGIN RUNPATH 与系统默认路径解析;父进程环境残留这些变量(如指向
// 其他 MAA 安装的 LD_LIBRARY_PATH)会把子进程的依赖解析带离锁定策略,
// 故三种模式的子进程一律在无 LD_* 注入的环境下验收,其余变量不受影响。
func childEnviron(mode string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key == "LD_LIBRARY_PATH" || key == "LD_PRELOAD" || key == "LD_AUDIT" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, childModeEnv+"="+mode)
}

// childDiagnostics 渲染子进程诊断:错误类别(超时/信号/退出码)+
// 完整 stdout/stderr,保证三种失败情形都留下可辨识证据。
func childDiagnostics(mode string, out childOutcome) string {
	var b strings.Builder
	switch {
	case out.err == nil:
		fmt.Fprintf(&b, "mode=%s exit code 0\n", mode)
	case out.timedOut:
		fmt.Fprintf(&b, "mode=%s killed on timeout (context deadline exceeded): %v\n", mode, out.err)
	case out.signaled:
		fmt.Fprintf(&b, "mode=%s killed by signal: %v\n", mode, out.err)
	default:
		fmt.Fprintf(&b, "mode=%s non-zero exit (exit code %d): %v\n", mode, out.exitCode, out.err)
	}
	fmt.Fprintf(&b, "--- stdout ---\n%s\n--- stderr ---\n%s", out.stdout, out.stderr)
	return b.String()
}

// verifySuccessChildMetadata 对 success 子进程的 stdout 做父进程侧
// 元数据复验:解析 successMarker 行打印的 version= 与 nullSize= 实际
// 值,独立核对锁定基线。子进程内的同名断言与本复验互相独立(纵深
// 防御):父进程不信任子进程内的断言是否真实执行,只以 stdout 实际
// 打印值为准——缺行、格式异常或任一值不符都判定父测试失败。
func verifySuccessChildMetadata(stdout string) error {
	line, ok := successMetadataLine(stdout)
	if !ok {
		return fmt.Errorf("stdout missing %s version=... nullSize=... metadata line", successMarker)
	}
	fields := strings.Fields(line)
	gotVersion := strings.TrimPrefix(fields[1], "version=")
	if gotVersion != lockedCoreVersion {
		return fmt.Errorf("stdout re-verification Version = %q, want %q (locked baseline)", gotVersion, lockedCoreVersion)
	}
	gotNullSize, err := strconv.ParseUint(strings.TrimPrefix(fields[2], "nullSize="), 10, 64)
	if err != nil {
		return fmt.Errorf("stdout re-verification nullSize %q is not a decimal uint64: %w", fields[2], err)
	}
	if gotNullSize != math.MaxUint64 {
		return fmt.Errorf("stdout re-verification NullSize = %d (0x%x), want %d (0x%x, locked profile sentinel)",
			gotNullSize, gotNullSize, uint64(math.MaxUint64), uint64(math.MaxUint64))
	}
	return nil
}

// successMetadataLine 从子进程 stdout 中定位元数据行:以 successMarker
// 开头且第二、三字段分别为 version=、nullSize= 前缀的那一行;capabilities
// 行同以 successMarker 开头但不匹配此形态。
func successMetadataLine(stdout string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, successMarker+" ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.HasPrefix(fields[1], "version=") && strings.HasPrefix(fields[2], "nullSize=") {
			return line, true
		}
	}
	return "", false
}

// TestRealCoreOpenSucceedsInFreshProcess 验收真实库成功路径:
// 在全新子进程中经公开 Open 加载锁定的 libMaaCore.so 并完成初始化,
// 断言 Version 与 NullSize 符合锁定 profile,打印 Capabilities 实际
// 值供父进程记录(不硬断言具体能力位)。随后再启动一次新的 success
// 子进程且同样成功——与 missing-lib 模式共同证明:同进程失败后拒绝
// 再次 Open,恢复只能来自新进程。退出码非 0、被信号杀死、超时都
// 是本测试的明确失败,不重试。
func TestRealCoreOpenSucceedsInFreshProcess(t *testing.T) {
	libPath, resourceRoot := requireUnpackedResources(t)

	for attempt := 1; attempt <= 2; attempt++ {
		userDir := t.TempDir() // 每次子进程独立的可写用户目录
		out := runCoreOpenChild(t, successChildTimeout, childModeSuccess, map[string]string{
			libPathEnv:      libPath,
			resourceRootEnv: resourceRoot,
			userDirEnv:      userDir,
		})
		if out.err != nil {
			t.Fatalf("success child run %d failed (non-zero exit/signal/timeout are all treated as failure):\n%s",
				attempt, childDiagnostics(childModeSuccess, out))
		}
		if !strings.Contains(out.stdout, successMarker) || !strings.Contains(out.stdout, "capabilities ") {
			t.Fatalf("success child run %d exited with exit code 0, but stdout lacks the success marker:\n%s",
				attempt, childDiagnostics(childModeSuccess, out))
		}
		if err := verifySuccessChildMetadata(out.stdout); err != nil {
			t.Fatalf("success child run %d stdout metadata re-verification failed (independent of the assertions inside the child):\n%v\n%s",
				attempt, err, childDiagnostics(childModeSuccess, out))
		}
		t.Logf("success child run %d succeeded:\n%s", attempt, out.stdout)
		if out.stderr != "" {
			t.Logf("success child run %d stderr:\n%s", attempt, out.stderr)
		}
	}
}

// TestRealCoreMissingLibTerminatesProcessLifecycle 验收缺库失败
// 路径:子进程内以不存在的绝对路径调用公开 Open,断言
//   - 错误可 errors.As 命中 *LibraryLoadError(原始错误直接返回,
//     不包 ErrRuntimeTerminated);
//   - 同一进程随后再次 Open 返回 errors.Is(…, ErrRuntimeTerminated)
//     (生命周期已烧毁,拒绝重试)。
//
// 两条断言都通过子进程才退出 0;父进程断言其退出 0。
func TestRealCoreMissingLibTerminatesProcessLifecycle(t *testing.T) {
	out := runCoreOpenChild(t, missingChildTimeout, childModeMissing, nil)
	if out.err != nil {
		t.Fatalf("missing-lib child failed (its internal assertions did not all pass):\n%s",
			childDiagnostics(childModeMissing, out))
	}
	if !strings.Contains(out.stdout, missingOKMarker) {
		t.Fatalf("missing-lib child exited with exit code 0, but stdout lacks the success marker:\n%s",
			childDiagnostics(childModeMissing, out))
	}
	t.Logf("missing-lib child internal assertions all passed:\n%s", out.stdout)
}

// TestRealCoreTimeoutKillReportedAsFailure 验收执行器的超时
// 语义:挂起子进程(长时间 sleep)在短超时后被杀死,执行器必须把
// 「被超时杀死」报告为失败并保留可辨识诊断——超时不是通过,也不得
// recover 后继续或自动重试。本测试断言该机制工作,测试本身通过。
func TestRealCoreTimeoutKillReportedAsFailure(t *testing.T) {
	out := runCoreOpenChild(t, hangChildTimeout, childModeHang, nil)
	if out.err == nil {
		t.Fatalf("hang child must be reported as failed after the %s timeout, actual exit code 0:\n%s",
			hangChildTimeout, childDiagnostics(childModeHang, out))
	}
	if !out.timedOut {
		t.Fatalf("the failure must be identifiable as timeout termination (timedOut=false, err=%v):\n%s",
			out.err, childDiagnostics(childModeHang, out))
	}
	if !out.signaled {
		t.Fatalf("timeout termination must kill the child via a signal (signaled=false, err=%v):\n%s",
			out.err, childDiagnostics(childModeHang, out))
	}
	if !strings.Contains(out.stdout, hangMarker) {
		t.Fatalf("timeout diagnostics must preserve the output the child already produced:\n%s",
			childDiagnostics(childModeHang, out))
	}
	t.Logf("hang child was SIGKILLed on the %s timeout as expected; the executor reported failure and preserved diagnostics:\n%s",
		hangChildTimeout, childDiagnostics(childModeHang, out))
}

// runSuccessChild 是 childModeSuccess 子进程入口:经公开 Open 完成
// 真实库初始化,断言锁定 profile 的 Version 与 NullSize,把
// Capabilities 实际值打印到 stdout 供父进程记录。不创建实例、不连
// 设备。断言失败输出诊断并以非 0 退出。
func runSuccessChild() int {
	libPath := os.Getenv(libPathEnv)
	resourceRoot := os.Getenv(resourceRootEnv)
	userDir := os.Getenv(userDirEnv)
	for name, value := range map[string]string{
		libPathEnv:      libPath,
		resourceRootEnv: resourceRoot,
		userDirEnv:      userDir,
	} {
		if !filepath.IsAbs(value) {
			fmt.Fprintf(os.Stderr, "success child: %s = %q is not an absolute path\n", name, value)
			return 1
		}
	}

	rt, err := Open(RuntimeConfig{
		LibraryPath:   libPath,
		UserDir:       userDir,
		ResourceRoots: []string{resourceRoot},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "success child: Open(%s) failed: %v\n", libPath, err)
		return 1
	}
	if got := rt.Version(); got != lockedCoreVersion {
		fmt.Fprintf(os.Stderr, "success child: Version() = %q, want %q\n", got, lockedCoreVersion)
		return 1
	}
	if got := rt.NullSize(); got != math.MaxUint64 {
		fmt.Fprintf(os.Stderr, "success child: NullSize() = %#x, want %#x (locked profile sentinel)\n", got, uint64(math.MaxUint64))
		return 1
	}
	caps := rt.Capabilities()
	fmt.Printf("%s version=%s nullSize=%d (0x%x)\n", successMarker, rt.Version(), rt.NullSize(), rt.NullSize())
	fmt.Printf("%s capabilities Create=%v ImageBgr=%v AsyncAttachWindow=%v\n",
		successMarker, caps.Create(), caps.ImageBgr(), caps.AsyncAttachWindow())
	return 0
}

// runMissingLibChild 是 childModeMissing 子进程入口:先以不存在的
// 绝对路径 Open,断言错误可 errors.As 命中 *LibraryLoadError 且原始
// 错误不包 ErrRuntimeTerminated;再以同样配置二次 Open,断言返回
// errors.Is(…, ErrRuntimeTerminated)(同进程生命周期已烧毁)。
// 两条断言都通过才退出 0。
func runMissingLibChild() int {
	const missingLib = "/nonexistent/rhodeck-m1-missing/libMaaCore.so"
	userDir, err := os.MkdirTemp("", "maa-m1-missing-user-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "missing-lib child: MkdirTemp failed: %v\n", err)
		return 1
	}
	defer os.RemoveAll(userDir)

	cfg := RuntimeConfig{
		LibraryPath:   missingLib,
		UserDir:       userDir,
		ResourceRoots: []string{missingLib},
	}
	rt, err := Open(cfg)
	if rt != nil || err == nil {
		fmt.Fprintf(os.Stderr, "missing-lib child: Open(%s) = (%v, %v), want (nil, error)\n", missingLib, rt, err)
		return 1
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		fmt.Fprintf(os.Stderr, "missing-lib child: errors.As did not match *LibraryLoadError, got %T: %v\n", err, err)
		return 1
	}
	if lle.Path != missingLib {
		fmt.Fprintf(os.Stderr, "missing-lib child: LibraryLoadError.Path = %q, want %q\n", lle.Path, missingLib)
		return 1
	}
	if errors.Is(err, ErrRuntimeTerminated) {
		fmt.Fprintf(os.Stderr, "missing-lib child: original error must not be wrapped into ErrRuntimeTerminated: %v\n", err)
		return 1
	}

	rt2, err2 := Open(cfg)
	if rt2 != nil {
		fmt.Fprintf(os.Stderr, "missing-lib child: second Open must not return a Runtime: %v\n", rt2)
		return 1
	}
	if !errors.Is(err2, ErrRuntimeTerminated) {
		fmt.Fprintf(os.Stderr, "missing-lib child: second Open error = %v, want errors.Is ErrRuntimeTerminated\n", err2)
		return 1
	}
	fmt.Printf("%s first=%v; second: errors.Is(ErrRuntimeTerminated)=true\n", missingOKMarker, err)
	return 0
}

// runHangChild 是 childModeHang 子进程入口:打印启动标记后长时间
// sleep,由父进程的外部超时杀死,专门验收「超时终止被明确判为
// 失败并保留诊断」的执行器语义。
func runHangChild() int {
	fmt.Println(hangMarker + " started, sleeping " + hangSleepDuration.String())
	time.Sleep(hangSleepDuration)
	return 0
}
