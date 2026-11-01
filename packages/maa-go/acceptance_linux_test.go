//go:build linux

// 本文件验证真实 MaaCore 的加载与元数据读取:显式安装输入或发行包流式解包,
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 实际安装输入不固化版本;摘要期望仅由调用方显式给出。
const (
	coreSOName            = "libMaaCore.so"
	versionRelPath        = "resource/version.json"
	childModeEnv          = "MAA_CORE_CHILD_MODE"
	childModeSuccess      = "success"
	childModeMissing      = "missing-lib"
	childModeHang         = "hang"
	libPathEnv            = "MAA_CORE_LIB_PATH"
	resourceRootEnv       = "MAA_CORE_RESOURCE_ROOT"
	userDirEnv            = "MAA_CORE_USER_DIR"
	tarballOverrideEv     = "MAA_CORE_TARBALL"
	expectedVersionEnv    = "MAA_CORE_EXPECTED_VERSION"
	expectedLibrarySHAEnv = "MAA_CORE_EXPECTED_LIBRARY_SHA256"
	expectedTarballSHAEnv = "MAA_CORE_EXPECTED_TARBALL_SHA256"
	expectedVersionSHAEnv = "MAA_CORE_EXPECTED_VERSION_SHA256"
	successMarker         = "MAA_CORE_SUCCESS"
	missingOKMarker       = "MAA_CORE_MISSING_LIB_OK"
	hangMarker            = "MAA_CORE_HANG"
)

// 输入不完整只说明真实库未验证;已给出的无效输入则是失败。
var errCoreInputMissing = errors.New("explicit real-library inputs are incomplete")

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

// 每个父进程惰性准备一次输入,只清理由本进程解包的临时目录。
var unpackState struct {
	once      sync.Once
	resources coreResources
	err       error
}

type coreResources struct {
	root, libPath, cleanupRoot         string
	tarballSHA, librarySHA, versionSHA string
}

func cleanupUnpackedResources() {
	if unpackState.resources.cleanupRoot != "" {
		os.RemoveAll(unpackState.resources.cleanupRoot)
	}
}

func requireUnpackedResources(t *testing.T) (libPath, resourceRoot string) {
	t.Helper()
	unpackState.once.Do(func() { unpackState.resources, unpackState.err = prepareCoreResources(os.Getenv) })
	if errors.Is(unpackState.err, errCoreInputMissing) {
		t.Skipf("real-library admission UNVERIFIED: %v; provide %s and %s, or %s; SKIP is not a pass", unpackState.err, libPathEnv, resourceRootEnv, tarballOverrideEv)
	}
	if unpackState.err != nil {
		require.NoError(t, unpackState.err, "real-library input preparation failed: %v", unpackState.err)
	}
	r := unpackState.resources
	t.Logf("actual installation: library SHA256=%s resource/version.json SHA256=%s tarball SHA256=%s (empty when not supplied); resource provenance is maintained by the host", r.librarySHA, r.versionSHA, r.tarballSHA)
	return r.libPath, r.root
}

// prepareCoreResources 不访问默认缓存、不下载资源,也不修改已有安装。
func prepareCoreResources(getenv func(string) string) (r coreResources, err error) {
	lib, root, archive := getenv(libPathEnv), getenv(resourceRootEnv), getenv(tarballOverrideEv)
	if archive != "" && (lib != "" || root != "") {
		return r, errors.New("provide either an installed library/resource root or a tarball, not both")
	}
	if archive == "" && getenv(expectedTarballSHAEnv) != "" {
		return r, fmt.Errorf("%s requires %s", expectedTarballSHAEnv, tarballOverrideEv)
	}
	for _, key := range []string{expectedLibrarySHAEnv, expectedTarballSHAEnv, expectedVersionSHAEnv} {
		if value := getenv(key); value != "" {
			decoded, decodeErr := hex.DecodeString(value)
			if decodeErr != nil || len(decoded) != sha256.Size {
				return r, fmt.Errorf("%s must contain a SHA256 hexadecimal digest", key)
			}
		}
	}
	if archive == "" && (lib == "" || root == "") {
		return r, errCoreInputMissing
	}
	if archive != "" {
		if !filepath.IsAbs(archive) {
			return r, fmt.Errorf("%s must be an absolute path", tarballOverrideEv)
		}
		if r.tarballSHA, err = checkFileSHA(archive, getenv(expectedTarballSHAEnv)); err != nil {
			return r, err
		}
		r.root, err = os.MkdirTemp("", "maa-core-accept-")
		if err != nil {
			return r, err
		}
		r.cleanupRoot = r.root
		defer func() {
			if err != nil {
				os.RemoveAll(r.cleanupRoot)
			}
		}()
		if err = extractCoreMembers(r.root, archive); err != nil {
			return r, err
		}
		r.libPath = filepath.Join(r.root, coreSOName)
	} else {
		if !filepath.IsAbs(lib) || !filepath.IsAbs(root) {
			return r, fmt.Errorf("%s and %s must be absolute paths", libPathEnv, resourceRootEnv)
		}
		r.libPath, r.root = filepath.Clean(lib), filepath.Clean(root)
	}
	if r.librarySHA, err = checkFileSHA(r.libPath, getenv(expectedLibrarySHAEnv)); err != nil {
		return r, err
	}
	if r.versionSHA, err = checkFileSHA(filepath.Join(r.root, versionRelPath), getenv(expectedVersionSHAEnv)); err != nil {
		return r, err
	}
	return r, nil
}

func checkFileSHA(path, want string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	got, err := sha256File(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	if want != "" && !strings.EqualFold(got, want) {
		return "", fmt.Errorf("%s SHA256 = %s, want %s", path, got, want)
	}
	return got, nil
}

// 只提取顶层动态库及 resource/;链接仅允许指向解包内的顶层动态库。
func extractCoreMembers(root, archive string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip %s: %w", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || name == "." {
			if hdr.Typeflag != tar.TypeDir {
				return fmt.Errorf("archive root member %q is not a directory", hdr.Name)
			}
			continue
		}
		dest, err := safeJoin(root, name)
		if err != nil {
			return err
		}
		if !coreMemberWanted(name) {
			continue
		}
		if seen[dest] {
			return fmt.Errorf("duplicate archive member %q", name)
		}
		seen[dest] = true
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := extractFile(tr, dest, hdr.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("extract %s: %w", name, err)
			}
		case tar.TypeSymlink:
			if strings.Contains(name, "/") || strings.Contains(hdr.Linkname, "/") || !coreMemberWanted(hdr.Linkname) {
				return fmt.Errorf("member %s has an unsafe symlink target %q", name, hdr.Linkname)
			}
			if _, err := safeJoin(root, hdr.Linkname); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, dest); err != nil {
				return err
			}
		default:
			return fmt.Errorf("member %s has unsupported type %q", name, hdr.Typeflag)
		}
	}
}

func coreMemberWanted(name string) bool {
	if !strings.Contains(name, "/") {
		return strings.HasPrefix(name, "lib") && (strings.HasSuffix(name, ".so") || strings.Contains(name, ".so."))
	}
	return strings.HasPrefix(name, "resource/")
}

func safeJoin(root, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("refusing unsafe member path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("refusing unsafe member path %q", name)
		}
	}
	return filepath.Join(root, filepath.FromSlash(name)), nil
}

func extractFile(r io.Reader, dest string, mode fs.FileMode) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, r)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}

// verifyCoreVersion 核对实际非空版本和调用方可选期望,不推断任务能力。
func verifyCoreVersion(version, expected string) error {
	if version == "" {
		return errors.New("Core version is empty")
	}
	if expected != "" && version != expected {
		return fmt.Errorf("Core version = %q, want %q", version, expected)
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
		require.NoError(t, err, "failed to locate the test binary %q: %v", os.Args[0], err)
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
// 隔离策略是不依赖任何 LD_* 配置——libMaaCore.so 的依赖只经
// $ORIGIN RUNPATH 与系统默认路径解析;父进程环境残留这些变量(如指向
// 其他 MAA 安装的 LD_LIBRARY_PATH)会把子进程的依赖解析带离隔离策略,
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
// 值,独立核对实际安装组合。子进程内的同名断言与本复验互相独立(纵深
// 防御):父进程不信任子进程内的断言是否真实执行,只以 stdout 实际
// 打印值为准——缺行、格式异常或任一值不符都判定父测试失败。
func verifySuccessChildMetadata(stdout string) error {
	line, ok := successMetadataLine(stdout)
	if !ok {
		return fmt.Errorf("stdout missing %s version=... nullSize=... metadata line", successMarker)
	}
	fields := strings.Fields(line)
	gotVersion := strings.TrimPrefix(fields[1], "version=")
	if err := verifyCoreVersion(gotVersion, os.Getenv(expectedVersionEnv)); err != nil {
		return fmt.Errorf("stdout re-verification: %w", err)
	}
	gotNullSize, err := strconv.ParseUint(strings.TrimPrefix(fields[2], "nullSize="), 10, 64)
	if err != nil {
		return fmt.Errorf("stdout re-verification nullSize %q is not a decimal uint64: %w", fields[2], err)
	}
	if gotNullSize != math.MaxUint64 {
		return fmt.Errorf("stdout re-verification NullSize = %d (0x%x), want %d (0x%x, ABI sentinel)",
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
// 在全新子进程中经公开 Open 加载显式的 libMaaCore.so 并完成初始化,
// 核对实际版本与可选期望、NullSize ABI 哨兵,打印 Capabilities 实际
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
			require.NoError(t, out.err, "success child run %d failed (non-zero exit/signal/timeout are all treated as failure):\n%s",
				attempt, childDiagnostics(childModeSuccess, out))
		}
		if !strings.Contains(out.stdout, successMarker) || !strings.Contains(out.stdout, "capabilities ") {
			require.True(t, strings.Contains(out.stdout, successMarker) && strings.Contains(out.stdout, "capabilities "),
				"success child run %d exited with exit code 0, but stdout lacks the success marker:\n%s",
				attempt, childDiagnostics(childModeSuccess, out))
		}
		if err := verifySuccessChildMetadata(out.stdout); err != nil {
			require.NoError(t, err, "success child run %d stdout metadata re-verification failed (independent of the assertions inside the child):\n%v\n%s",
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
		require.NoError(t, out.err, "missing-lib child failed (its internal assertions did not all pass):\n%s",
			childDiagnostics(childModeMissing, out))
	}
	if !strings.Contains(out.stdout, missingOKMarker) {
		require.Contains(t, out.stdout, missingOKMarker, "missing-lib child exited with exit code 0, but stdout lacks the success marker:\n%s",
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
		require.Error(t, out.err, "hang child must be reported as failed after the %s timeout, actual exit code 0:\n%s",
			hangChildTimeout, childDiagnostics(childModeHang, out))
	}
	if !out.timedOut {
		require.True(t, out.timedOut, "the failure must be identifiable as timeout termination (timedOut=false, err=%v):\n%s",
			out.err, childDiagnostics(childModeHang, out))
	}
	if !out.signaled {
		require.True(t, out.signaled, "timeout termination must kill the child via a signal (signaled=false, err=%v):\n%s",
			out.err, childDiagnostics(childModeHang, out))
	}
	if !strings.Contains(out.stdout, hangMarker) {
		require.Contains(t, out.stdout, hangMarker, "timeout diagnostics must preserve the output the child already produced:\n%s",
			childDiagnostics(childModeHang, out))
	}
	t.Logf("hang child was SIGKILLed on the %s timeout as expected; the executor reported failure and preserved diagnostics:\n%s",
		hangChildTimeout, childDiagnostics(childModeHang, out))
}

// runSuccessChild 是 childModeSuccess 子进程入口:经公开 Open 完成
// 真实库初始化,核对实际版本与可选期望、NullSize ABI 哨兵,把
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
	if err := verifyCoreVersion(rt.Version(), os.Getenv(expectedVersionEnv)); err != nil {
		fmt.Fprintf(os.Stderr, "success child: %v\n", err)
		return 1
	}
	if got := rt.NullSize(); got != math.MaxUint64 {
		fmt.Fprintf(os.Stderr, "success child: NullSize() = %#x, want %#x (ABI sentinel)\n", got, uint64(math.MaxUint64))
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
