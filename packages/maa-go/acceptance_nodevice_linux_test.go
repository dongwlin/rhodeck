//go:build linux

// 本文件验证真实 MaaCore 的无设备链路:从锁定发行包解包真实 Core 与资源,
// 在全新子进程中经公开 API 跑通 Version/NullSize/资源加载 → NewInstance
// → 未连接 Screencap 失败关联 → 任务入队与 Tasks 读取 → Instance.Close
// → Runtime.Close,并验证执行器超时与原生依赖缺失的可见失败。
package maa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// 无设备链路子进程分派与资源传递的环境变量:与真实库链路的 MAA_CORE_* 互相独立,
// 互不触发对方 TestMain/测试守卫。
const (
	noDeviceChildModeEnv    = "MAA_CORE_NODEVICE_CHILD_MODE" // 子进程模式标记;缺省即父进程
	noDeviceModeChain       = "chain"                        // 真实库无设备完整链路
	noDeviceModeHang        = "hang"                         // 挂起模式:供父进程验收超时终止语义
	noDeviceModeMissingDeps = "missing-deps"                 // 原生依赖缺失模式:隔离目录加载可见失败
	noDeviceLibPathEnv      = "MAA_CORE_NODEVICE_LIB_PATH"   // 解包出的 libMaaCore.so 绝对路径
	noDeviceResourceEnv     = "MAA_CORE_NODEVICE_RESOURCE_ROOT"
	noDeviceUserDirEnv      = "MAA_CORE_NODEVICE_USER_DIR"
)

// 子进程 stdout 标记:META 在 Open 成功后立即打印(链路中途死亡的
// 诊断);OK 仅在子进程全部断言通过后打印(父进程复验其字段)。
const (
	noDeviceMetaMarker = "MAA_CORE_NODEVICE_META"
	noDeviceOKMarker   = "MAA_CORE_NODEVICE_OK"
	noDeviceHangMarker = "MAA_CORE_NODEVICE_HANG_STARTED"
	noDeviceDepsMarker = "MAA_CORE_NODEVICE_MISSING_DEPS_OK"
)

// 子进程超时与等待参数:链路与依赖缺失路径给足 10 分钟(Core 加载
// 资源在慢环境可能需要时间);挂起路径用 15 秒短超时专门验收超时
// 终止语义。WaitDelay 防止孙进程持有输出管道导致 Wait 挂起。
const (
	noDeviceChainChildTimeout    = 10 * time.Minute
	noDeviceDepsChildTimeout     = 10 * time.Minute
	noDeviceHangChildTimeout     = 15 * time.Second
	noDeviceHangSleepDuration    = 30 * time.Minute
	noDeviceChildWaitDelay       = 30 * time.Second
	noDeviceScreencapWaitTimeout = 5 * time.Minute
	noDeviceReplayWaitTimeout    = 2 * time.Minute
	noDeviceDrainGuardTimeout    = 30 * time.Second
)

// noDeviceDestroyedMessageID 是 AsstMsg::Destroyed 的消息编号,冻结自
// MaaAssistantArknights src/MaaCore/Common/AsstMsg.h(InternalError=0、
// InitFailed=1、ConnectionInfo=2、AllTasksCompleted=3、AsyncCallInfo=4、
// Destroyed=5)。锁定 Core 在 Destroy 调用线程 join 全部线程后同步
// 触发该消息(Assistant.cpp ~Assistant)。
const noDeviceDestroyedMessageID = MessageID(5)

// noDeviceTaskType 是仅追加不执行的任务类型:Award 的 set_params
// 对未知键取默认值恒返回 true(AwardTask.cpp),未连接、未 Start 的
// 实例只把它留在任务列表中,不产生任何设备动作。
const noDeviceTaskType = "Award"

// TestRealCoreNoDeviceChain 验收真实库无设备完整链路(见文件头映
// 射):父进程准备锁定资源与临时用户目录后启动子进程;子进程内断言
// 全部通过才退出 0。退出码非 0、被信号杀死、超时都是本测试的明确
// 失败,不重试;父进程并对 stdout 的 OK 标记行做独立元数据复验。
func TestRealCoreNoDeviceChain(t *testing.T) {
	if os.Getenv(noDeviceChildModeEnv) == noDeviceModeChain {
		runCoreNoDeviceChainChild(t)
		return
	}
	libPath, resourceRoot := requireUnpackedResources(t)
	userDir := t.TempDir() // 子进程专用的可写用户目录

	out := runCoreNoDeviceChild(t, noDeviceChainChildTimeout, noDeviceModeChain, map[string]string{
		noDeviceLibPathEnv:  libPath,
		noDeviceResourceEnv: resourceRoot,
		noDeviceUserDirEnv:  userDir,
	})
	if out.err != nil {
		t.Fatalf("chain child failed (non-zero exit/signal/timeout are all treated as failure, no retry):\n%s",
			childDiagnostics(noDeviceModeChain, out))
	}
	if err := verifyCoreNoDeviceChildMetadata(out.stdout); err != nil {
		t.Fatalf("chain child stdout metadata re-verification failed (independent of the assertions inside the child):\n%v\n%s",
			err, childDiagnostics(noDeviceModeChain, out))
	}
	t.Logf("chain child internal assertions all passed:\n%s", out.stdout)
	if out.stderr != "" {
		t.Logf("chain child stderr:\n%s", out.stderr)
	}
	t.Logf("locked baseline: tarball SHA256=%s, core SHA256=%s, resource/version.json SHA256=%s, core version=%s",
		lockedTarballSHA256, lockedCoreSHA256, lockedVersionSHA256, lockedCoreVersion)
}

// TestRealCoreNoDeviceExecutorTimeoutKillReportedAsFailure 验收本文件执行
// 器的超时语义:挂起子进程在短超时后被杀死,必须被报告为失败并保留
// 可辨识诊断——超时不是通过,不得 recover 后继续或自动重试。同形态
// 覆盖信号死亡(原生崩溃)的可见性:任何信号终止都映射为
// signaled=true 的失败。
func TestRealCoreNoDeviceExecutorTimeoutKillReportedAsFailure(t *testing.T) {
	if os.Getenv(noDeviceChildModeEnv) == noDeviceModeHang {
		// 子进程:打印启动标记后长时间 sleep,由父进程的外部超时杀死。
		fmt.Println(noDeviceHangMarker + " started, sleeping " + noDeviceHangSleepDuration.String())
		time.Sleep(noDeviceHangSleepDuration)
		return
	}
	out := runCoreNoDeviceChild(t, noDeviceHangChildTimeout, noDeviceModeHang, nil)
	if out.err == nil {
		t.Fatalf("hang child must be reported as failed after the %s timeout, actual exit code 0:\n%s",
			noDeviceHangChildTimeout, childDiagnostics(noDeviceModeHang, out))
	}
	if !out.timedOut {
		t.Fatalf("the failure must be identifiable as timeout termination (timedOut=false, err=%v):\n%s",
			out.err, childDiagnostics(noDeviceModeHang, out))
	}
	if !out.signaled {
		t.Fatalf("timeout termination must kill the child via a signal (signaled=false, err=%v):\n%s",
			out.err, childDiagnostics(noDeviceModeHang, out))
	}
	if !strings.Contains(out.stdout, noDeviceHangMarker) {
		t.Fatalf("timeout diagnostics must preserve the output the child already produced:\n%s", childDiagnostics(noDeviceModeHang, out))
	}
	t.Logf("hang child was SIGKILLed on the %s timeout as expected; the executor reported failure and preserved diagnostics:\n%s",
		noDeviceHangChildTimeout, childDiagnostics(noDeviceModeHang, out))
}

// TestRealCoreNoDeviceMissingNativeDepsReportedAsFailure 验收原生依赖缺失
// 的可见失败:libMaaCore.so 的捆绑依赖(OpenCV/ONNX Runtime 等 8 个
// .so,DT_RUNPATH=$ORIGIN)不在系统默认路径,仅复制 libMaaCore.so 的
// 隔离目录必然 dlopen 失败。子进程断言该失败以 *LibraryLoadError 可
// 见地返回(原始错误直接返回,不包 ErrRuntimeTerminated),且同进程
// 二次 Open 返回 errors.Is(…, ErrRuntimeTerminated)(预占即不可回
// 头);两条断言都通过才退出 0。
func TestRealCoreNoDeviceMissingNativeDepsReportedAsFailure(t *testing.T) {
	if os.Getenv(noDeviceChildModeEnv) == noDeviceModeMissingDeps {
		runCoreNoDeviceMissingDepsChild(t)
		return
	}
	libPath, _ := requireUnpackedResources(t)

	// 隔离目录:只放 libMaaCore.so,不含任何捆绑依赖。
	isolated := t.TempDir()
	isolatedLib := filepath.Join(isolated, lockedCoreSOName)
	if err := copyFile(libPath, isolatedLib); err != nil {
		t.Fatalf("failed to copy libMaaCore.so into the isolated directory: %v", err)
	}

	out := runCoreNoDeviceChild(t, noDeviceDepsChildTimeout, noDeviceModeMissingDeps, map[string]string{
		noDeviceLibPathEnv: isolatedLib,
		noDeviceUserDirEnv: t.TempDir(),
	})
	if out.err != nil {
		t.Fatalf("missing-deps child failed (its internal assertions did not all pass):\n%s",
			childDiagnostics(noDeviceModeMissingDeps, out))
	}
	if !strings.Contains(out.stdout, noDeviceDepsMarker) {
		t.Fatalf("missing-deps child exited with exit code 0, but stdout lacks the success marker:\n%s",
			childDiagnostics(noDeviceModeMissingDeps, out))
	}
	t.Logf("missing-deps child internal assertions all passed:\n%s", out.stdout)
}

// runCoreNoDeviceChainChild 是 noDeviceModeChain 子进程入口:真实锁定库上的
// 无设备公开链路,步骤与断言见文件头映射。红线:全程不调用 Start、
// Connect、Click,不触达任何设备动作。断言失败以非 0 退出。
func runCoreNoDeviceChainChild(t *testing.T) {
	libPath := os.Getenv(noDeviceLibPathEnv)
	resourceRoot := os.Getenv(noDeviceResourceEnv)
	userDir := os.Getenv(noDeviceUserDirEnv)
	for _, e := range []struct{ name, value string }{
		{noDeviceLibPathEnv, libPath}, {noDeviceResourceEnv, resourceRoot}, {noDeviceUserDirEnv, userDir},
	} {
		if !filepath.IsAbs(e.value) {
			t.Fatalf("%s = %q is not an absolute path", e.name, e.value)
		}
	}

	// 1. 公开 Open:真实库加载 + Version/NullSize 核验 + 用户目录 +
	//    资源初始化(内部任一步失败 Open 即失败)。
	rt, err := Open(RuntimeConfig{
		LibraryPath:   libPath,
		UserDir:       userDir,
		ResourceRoots: []string{resourceRoot},
	})
	if err != nil {
		t.Fatalf("Open(%s) failed: %v", libPath, err)
	}
	if got := rt.Version(); got != lockedCoreVersion {
		t.Fatalf("Version() = %q, want %q (locked baseline)", got, lockedCoreVersion)
	}
	if got := rt.NullSize(); got != math.MaxUint64 {
		t.Fatalf("NullSize() = %#x, want %#x (locked profile sentinel)", got, uint64(math.MaxUint64))
	}
	if entries, err := os.ReadDir(userDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Logf("user directory entries after Open (names only): %v", names)
	}
	caps := rt.Capabilities()
	t.Logf("Open ok: version=%s nullSize=%#x capabilities Create=%v ImageBgr=%v AsyncAttachWindow=%v",
		rt.Version(), rt.NullSize(), caps.Create(), caps.ImageBgr(), caps.AsyncAttachWindow())
	fmt.Printf("%s version=%s nullSize=%d\n", noDeviceMetaMarker, rt.Version(), rt.NullSize())

	// 2. NewInstance:CreateEx(进程唯一 callback trampoline + token)。
	inst, err := rt.NewInstance(InstanceConfig{})
	if err != nil || inst == nil {
		t.Fatalf("NewInstance failed: err=%v, inst=%v", err, inst)
	}
	t.Logf("NewInstance ok: CreateEx returned a valid handle and published a ready instance")

	// 3. 未连接 Screencap:提交被接受(非零 ID)不等于截图成功;最终
	//    结果由真实 AsyncCallInfo 按 async_call_id 关联结算为原生失败。
	op, err := inst.Screencap()
	if err != nil || op == nil {
		t.Fatalf("Screencap submit failed: err=%v, op=%v", err, op)
	}
	if op.ID() == 0 {
		t.Fatalf("Screencap submission returned ID 0")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), noDeviceScreencapWaitTimeout)
	res, waitErr := op.Wait(waitCtx)
	cancel()
	if waitErr == nil {
		t.Fatalf("unconnected Screencap reported success %+v: submission success must not be interpreted as a successful screenshot", res)
	}
	if !errors.Is(waitErr, ErrNativeRejected) {
		t.Fatalf("unconnected Screencap Wait error = %v, want errors.Is ErrNativeRejected", waitErr)
	}
	if res.OK {
		t.Fatalf("AsyncResult.OK = true, unconnected screencap must fail")
	}
	if res.What != asyncWhatScreencap {
		t.Fatalf("AsyncResult.What = %q, want %q (frozen value from the locked Core call_proc)", res.What, asyncWhatScreencap)
	}
	if res.ID != op.ID() {
		t.Fatalf("AsyncResult.ID = %d, want the submitted ID %d (async_call_id correlation broken)", res.ID, op.ID())
	}
	var det struct {
		Ret  *bool  `json:"ret"`
		Cost *int64 `json:"cost"`
	}
	if err := json.Unmarshal(res.Details, &det); err != nil {
		t.Fatalf("failed to decode AsyncResult.Details: %v (raw: %s)", err, res.Details)
	}
	if det.Ret == nil || *det.Ret {
		t.Fatalf("details.ret = %s, want false", res.Details)
	}
	cost := int64(-1)
	if det.Cost != nil {
		cost = *det.Cost
	}
	t.Logf("real AsyncCallInfo of the unconnected Screencap correlated: id=%d what=%s ok=false ret=false cost=%dms details=%s",
		res.ID, res.What, cost, res.Details)

	// 4. 仅追加不执行的任务并读取列表:不 Start、不 Connect、不执行
	//    游戏操作;Connected()==false 佐证全程无连接。
	id1, err := inst.AppendTask(noDeviceTaskType, json.RawMessage(`{"enable":true}`))
	if err != nil || id1 == 0 {
		t.Fatalf("AppendTask#1 = (%d, %v), want (non-zero, nil)", id1, err)
	}
	id2, err := inst.AppendTask(noDeviceTaskType, json.RawMessage(`{"enable":true}`))
	if err != nil || id2 == 0 {
		t.Fatalf("AppendTask#2 = (%d, %v), want (non-zero, nil)", id2, err)
	}
	if id2 == id1 {
		t.Fatalf("the two AppendTask calls returned the same TaskID %d", id1)
	}
	connected, err := inst.Connected()
	if err != nil {
		t.Fatalf("Connected() error: %v", err)
	}
	if connected {
		t.Fatalf("Connected() = true, must not report connected when Connect was never called")
	}
	tasks, err := inst.Tasks()
	if err != nil {
		t.Fatalf("Tasks() error: %v", err)
	}
	if len(tasks) != 2 || !slices.Contains(tasks, id1) || !slices.Contains(tasks, id2) {
		t.Fatalf("Tasks() = %v, want a 2-element list containing exactly the enqueued {%d, %d} (order decided by Core, order not checked)",
			tasks, id1, id2)
	}
	t.Logf("appended but not executed: tasks=%v (Award×2, no Start/Connect), Connected=false", tasks)

	// 5. Instance.Close:真实一次 Stop(未启动实例返回 true)与一次
	//    Destroy(C void);Destroyed 在 Destroy 调用线程同步触发,经
	//    仍开放的准入门入队,由关闭流程排空后投递公开缓冲。
	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (normal close of a never-started instance)", err)
	}

	// 5a. Destroyed 的诊断顺序:排空后的公开缓冲恰为 [AsyncCallInfo,
	//     Destroyed],本地序号单调递增;公开 Events 通道已随关闭关闭。
	evs := readDrainedEvents(t, inst)
	if len(evs) != 2 {
		t.Fatalf("public event buffer after Close = %d events (%s), want exactly the two [AsyncCallInfo, Destroyed]",
			len(evs), renderEvents(evs))
	}
	if evs[0].MessageID != asyncCallInfoMessageID {
		t.Fatalf("public event[0] messageID = %d, want AsyncCallInfo(%d): %s",
			evs[0].MessageID, asyncCallInfoMessageID, renderEvents(evs))
	}
	if evs[1].MessageID != noDeviceDestroyedMessageID {
		t.Fatalf("public event[1] messageID = %d, want Destroyed(%d): %s",
			evs[1].MessageID, noDeviceDestroyedMessageID, renderEvents(evs))
	}
	if evs[0].Sequence != 1 || evs[1].Sequence != 2 || evs[0].Sequence >= evs[1].Sequence {
		t.Fatalf("public event local sequences = (%d, %d), want (1, 2) monotonically increasing", evs[0].Sequence, evs[1].Sequence)
	}
	t.Logf("Destroyed diagnostic order confirmed: AsyncCallInfo(seq=%d, json=%s) -> Destroyed(seq=%d, json=%s), channel closed afterwards",
		evs[0].Sequence, evs[0].JSON, evs[1].Sequence, evs[1].JSON)

	// 5b. Destroy 返回后 callback 契约:关门、退空、移除 token 之后,
	//     真实注册表不再把迟到的原生回调路由到本实例。
	if native.LookupCallbackDelivery(inst.inst.token) != nil {
		t.Fatalf("callback token %d still resolves to a delivery function after Close, the late-callback contract is broken", inst.inst.token)
	}

	// 5c. 实例关闭后 Operation 仍可等待,取得同一保存终态的独立副本。
	replayCtx, cancel := context.WithTimeout(context.Background(), noDeviceReplayWaitTimeout)
	res2, replayErr := op.Wait(replayCtx)
	cancel()
	if !errors.Is(replayErr, ErrNativeRejected) {
		t.Fatalf("repeated Wait after close error = %v, want errors.Is ErrNativeRejected", replayErr)
	}
	if res2.ID != res.ID || res2.What != res.What || res2.OK != res.OK || !bytes.Equal(res2.Details, res.Details) {
		t.Fatalf("repeated Wait after close drifted: first=%+v replay=%+v", res, res2)
	}

	// 6. Runtime.Close:实例已终止使用,重复 Close 直接返回已存储结果
	//    (nil,不再进入 native);此后 NewInstance 一律被拒。
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("NewInstance error after Runtime termination = %v, want errors.Is ErrRuntimeTerminated", err)
	}

	// 7. 全部断言通过后才打印 OK 标记(父进程复验其字段)。
	fmt.Printf("%s version=%s nullSize=%d screencap_id=%d screencap_what=%s screencap_ok=false details_ret=false tasks=2 destroyed_events=1 token_unregistered=true replay=true\n",
		noDeviceOKMarker, rt.Version(), rt.NullSize(), res.ID, res.What)
}

// runCoreNoDeviceMissingDepsChild 是 noDeviceModeMissingDeps 子进程入口:以仅含
// libMaaCore.so 的隔离路径 Open,断言失败以 *LibraryLoadError 可见
// 返回且不包 ErrRuntimeTerminated,同进程二次 Open 得
// ErrRuntimeTerminated;两条断言都通过才退出 0。
func runCoreNoDeviceMissingDepsChild(t *testing.T) {
	libPath := os.Getenv(noDeviceLibPathEnv)
	userDir := os.Getenv(noDeviceUserDirEnv)
	for _, e := range []struct{ name, value string }{
		{noDeviceLibPathEnv, libPath}, {noDeviceUserDirEnv, userDir},
	} {
		if !filepath.IsAbs(e.value) {
			t.Fatalf("%s = %q is not an absolute path", e.name, e.value)
		}
	}

	cfg := RuntimeConfig{
		LibraryPath:   libPath,
		UserDir:       userDir,
		ResourceRoots: []string{userDir},
	}
	rt, err := Open(cfg)
	if rt != nil || err == nil {
		t.Fatalf("Open on the isolated directory (missing bundled deps) = (%v, %v), want (nil, error)", rt, err)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As did not match *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Path != libPath {
		t.Fatalf("LibraryLoadError.Path = %q, want %q", lle.Path, libPath)
	}
	if errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("original error must not be wrapped into ErrRuntimeTerminated: %v", err)
	}
	rt2, err2 := Open(cfg)
	if rt2 != nil {
		t.Fatalf("second Open must not return a Runtime: %v", rt2)
	}
	if !errors.Is(err2, ErrRuntimeTerminated) {
		t.Fatalf("second Open error = %v, want errors.Is ErrRuntimeTerminated", err2)
	}
	t.Logf("missing native deps visibly failed: %v; second Open: errors.Is(ErrRuntimeTerminated)=true", err)
	fmt.Printf("%s errors_as_library_load_error=true second_open_runtime_terminated=true\n", noDeviceDepsMarker)
}

// runCoreNoDeviceChild 以 go test 自身二进制重执行一个子进程:-test.run 钉住
// 单个测试名,子进程在该测试函数顶部按 noDeviceChildModeEnv 守卫分岔。超
// 时经 exec.CommandContext 外部强制(超时杀死是明确的失败,不是通
// 过);WaitDelay 防止孙进程持有输出管道导致 Wait 挂起。不重试、不
// recover:执行结果原样交回调用方判定(形态与真实库链路的 runCoreOpenChild 一致,
// 独立实现以避免跨接口契约改动)。
func runCoreNoDeviceChild(t *testing.T, timeout time.Duration, mode string, extraEnv map[string]string) childOutcome {
	t.Helper()
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("failed to locate the test binary %q: %v", os.Args[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+t.Name()+"$", "-test.v=true")
	cmd.Env = noDeviceChildEnviron(mode)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = noDeviceChildWaitDelay

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

// noDeviceChildEnviron 构造子进程环境:与真实库链路的 childEnviron 同一策略——
// 剥离动态链接器注入变量(LD_LIBRARY_PATH/LD_PRELOAD/LD_AUDIT)后
// 原样继承,再附上无设备链路模式标记。锁定策略不依赖任何 LD_* 配置
// (libMaaCore.so 的依赖只经 $ORIGIN RUNPATH 与系统默认路径解析),
// 父进程环境残留会把子进程的依赖解析带离锁定策略。
func noDeviceChildEnviron(mode string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key == "LD_LIBRARY_PATH" || key == "LD_PRELOAD" || key == "LD_AUDIT" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, noDeviceChildModeEnv+"="+mode)
}

// readDrainedEvents 在 Instance.Close 返回后读尽公开 Events 缓冲:
// 关闭流程保证 Close 返回前通道已由分发器关闭,缓冲事件仍可读;防
// 挂死守卫仅在契约被破坏(通道未关闭)时兜底为明确失败,不作同步
// 手段。子进程内防挂,父进程外部超时最终兜底。
func readDrainedEvents(t *testing.T, inst *Instance) []Event {
	t.Helper()
	ch := inst.inst.disp.events()
	var evs []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-time.After(noDeviceDrainGuardTimeout):
			t.Fatalf("public Events channel still not closed within %s after Close returned (%d events read), the close contract is broken",
				noDeviceDrainGuardTimeout, len(evs))
		}
	}
}

// renderEvents 渲染事件列表用于失败诊断。
func renderEvents(evs []Event) string {
	parts := make([]string, 0, len(evs))
	for _, ev := range evs {
		parts = append(parts, fmt.Sprintf("{msg=%d seq=%d json=%s}", ev.MessageID, ev.Sequence, ev.JSON))
	}
	return strings.Join(parts, " ")
}

// verifyCoreNoDeviceChildMetadata 对 chain 子进程的 stdout 做父进程侧元数据复
// 验:解析 OK 标记行的 version=/nullSize=/screencap_what=/screencap_
// ok/tasks= 实际值,独立核对锁定基线与链路结论(与子进程内断言互相
// 独立的纵深防御):缺行、格式异常或任一值不符都判定父测试失败。
func verifyCoreNoDeviceChildMetadata(stdout string) error {
	var (
		found      bool
		version    string
		nullSize   string
		what       string
		okField    string
		tasksCount string
	)
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, noDeviceOKMarker+" ") {
			continue
		}
		found = true
		for _, field := range strings.Fields(line)[1:] {
			switch {
			case strings.HasPrefix(field, "version="):
				version = strings.TrimPrefix(field, "version=")
			case strings.HasPrefix(field, "nullSize="):
				nullSize = strings.TrimPrefix(field, "nullSize=")
			case strings.HasPrefix(field, "screencap_what="):
				what = strings.TrimPrefix(field, "screencap_what=")
			case strings.HasPrefix(field, "screencap_ok="):
				okField = strings.TrimPrefix(field, "screencap_ok=")
			case strings.HasPrefix(field, "tasks="):
				tasksCount = strings.TrimPrefix(field, "tasks=")
			}
		}
		break
	}
	if !found {
		return fmt.Errorf("stdout missing %s metadata line", noDeviceOKMarker)
	}
	if version != lockedCoreVersion {
		return fmt.Errorf("stdout re-verification Version = %q, want %q (locked baseline)", version, lockedCoreVersion)
	}
	gotNullSize, err := strconv.ParseUint(nullSize, 10, 64)
	if err != nil {
		return fmt.Errorf("stdout re-verification nullSize %q is not a decimal uint64: %w", nullSize, err)
	}
	if gotNullSize != math.MaxUint64 {
		return fmt.Errorf("stdout re-verification NullSize = %d (0x%x), want %d (0x%x, locked profile sentinel)",
			gotNullSize, gotNullSize, uint64(math.MaxUint64), uint64(math.MaxUint64))
	}
	if what != asyncWhatScreencap {
		return fmt.Errorf("stdout re-verification screencap_what = %q, want %q", what, asyncWhatScreencap)
	}
	if okField != "false" {
		return fmt.Errorf("stdout re-verification screencap_ok = %q, want false", okField)
	}
	if tasksCount != "2" {
		return fmt.Errorf("stdout re-verification tasks = %q, want 2", tasksCount)
	}
	return nil
}

// copyFile 复制一个普通文件(内容与权限),供隔离目录构造使用。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
