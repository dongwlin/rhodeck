//go:build linux

// 本文件是 MAA-27 的真实库无设备链路验收(M3 阶段退出条件;绑定设计
// §9、§9.1):从锁定的发行包缓存解包真实 MaaCore 与资源,在全新子进
// 程中经公开 API 跑通「Version/NullSize/用户目录/资源 → NewInstance
// (CreateEx)→ 未连接 Screencap(真实 AsyncCallInfo 失败关联)→
// 仅追加任务与 Tasks 读取 → Instance.Close(真实 Stop/Destroy 与
// Destroyed)→ Runtime.Close(终止使用)」的完整链路,并验收执行器
// 的超时语义与原生依赖缺失的可见失败。不连接设备、不调用 Start、
// 不执行任何游戏操作:Screencap 是任务明文要求的未连接异步失败探针,
// 任务只验证入队和列表。
//
// 与 acceptance_linux_test.go(MAA-08 交付)的关系:直接复用其中的
// 锁定基线常量(lockedTarballSHA256/lockedCoreSHA256/lockedVersion
// SHA256/lockedCoreVersion/lockedCoreSOName)、解包与哈希钉扎
// (requireUnpackedResources,父子进程共享 once)与子进程结果渲染
// (childOutcome/childDiagnostics);本文件不改动该文件,自带独立
// 的子进程执行器(runM3Child,与 runM1Child 同形)与环境剥离
// (m3ChildEnviron),也不复用 M1 的 TestMain 分派——M3 子进程经
// -test.run 钉住单个测试名,测试函数顶部按 m3ChildModeEnv 守卫分岔,
// 子进程内 TestMain 照常落回 m.Run() 执行该测试,不进入 M1 分派。
//
// 执行器纪律(§9):子进程带外部超时(exec.CommandContext),超时
// 经 SIGKILL 终止并被报告为失败(超时不是通过);非零退出与信号死
// (含原生崩溃)一律失败并保留完整诊断;不 recover、不自动重试,
// 原生失败原样可见。锁定发行包缺失由 requireUnpackedResources 记为
// SKIP 并写明受阻原因——跳过不计为通过。
//
// 白盒边界(仅有的两处非公开读取,均为同包观察,不改生产行为):
//  1. Instance.Close 返回后读取实例分发器的公开 Events 缓冲
//     (disp.events()):公开事件流 API(Done/Err/Diagnostics)尚未
//     接入(doc.go),而「Destroyed 的诊断顺序」只能在排空后的公开
//     缓冲中观察——Destroyed 必须是 Close 返回前已被准入门准入、由
//     分发器投递的最后一条事件(锁定 Core 在 Destroy 调用线程同步
//     触发,Assistant.cpp ~Assistant);通道随关闭流程关闭,读尽即止。
//  2. Close 返回后经真实 token 注册表(native.LookupCallbackDelivery)
//     确认回调契约:Destroy 返回并完成关门、退空、移除 token 后,
//     迟到的原生回调无法再找到本实例的投递路径。
//
// 验收条件 → 用例映射(MAA-27):
//   - 「公开 API 完成 Version → 资源 → CreateEx → 未连接 Screencap →
//     入队/List → Destroy」:TestM3RealCoreNoDeviceChain 的链路步骤
//     1–6(Open 内部完成 Version/NullSize/SetUserDir/LoadResource;
//     NewInstance 完成 CreateEx;Screencap/Wait;AppendTask/Tasks;
//     Instance.Close;Runtime.Close)。
//   - 「异步失败与提交 ID 正确关联,提交成功不被解释为截图成功」:
//     同上步骤 3——提交返回非零 ID 后,Wait 取得真实 AsyncCallInfo
//     结算的终态:What=Screencap、ID 与提交 ID 相同、OK=false、
//     details.ret=false、错误包装 ErrNativeRejected;任何一环缺失
//     都是子进程失败。
//   - 「未发生 Start、Connect 或任何设备操作;任务只验证入队和列
//     表」:链路全程无 Start/Connect/Click 调用(代码即红线,测试
//     另断言 Connected()==(false,nil) 佐证无连接);任务仅
//     AppendTask×2 + Tasks 读取成员核对。
//   - 「真实 Destroy/回调/退出行为符合契约;若出现不符,阻止进入
//     M4」:同上步骤 5——Close 返回 nil(真实 Stop 在未启动实例上
//     返回 true,Destroy 为 C void);排空缓冲恰为 [AsyncCallInfo,
//     Destroyed] 两条、序号单调递增;token 不再可查;关闭后重复
//     Wait 取得同一保存终态;Runtime 终止使用(NewInstance 返回
//     ErrRuntimeTerminated);子进程正常退出 0。任一不符即为失败,
//     阻断 M4 并回设计修正。
//   - 「子进程崩溃、超时及原生依赖缺失产生可见结果;跳过不计为通
//     过,原生失败不自动重试」:TestM3RealCoreExecutorTimeoutKill
//     ReportedAsFailure(挂起子进程在短超时后被 SIGKILL,执行器报
//     告为失败并保留已产出输出,同形态覆盖信号死亡的可见性);
//     TestM3RealCoreMissingNativeDepsReportedAsFailure(仅含
//     libMaaCore.so 的隔离目录——捆绑依赖不在系统路径,dlopen 以
//     *LibraryLoadError 可见失败,同进程二次 Open 得
//     ErrRuntimeTerminated);链路子进程单次运行,失败即 Fatal。
//
// 复现命令(linux amd64,需 ~/.cache/maa 的锁定发行包):
//
//	CGO_ENABLED=0 go test ./packages/maa-go/ -run 'TestM3RealCore' -count=1 -v
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

// M3 子进程分派与资源传递的环境变量:与 M1 的 MAA_M1_* 互相独立,
// 互不触发对方 TestMain/测试守卫。
const (
	m3ChildModeEnv = "MAA_M3_CHILD_MODE" // 子进程模式标记;缺省即父进程
	m3ModeChain    = "chain"             // 真实库无设备完整链路
	m3ModeHang     = "hang"              // 挂起模式:供父进程验收超时终止语义
	m3ModeMissDeps = "missing-deps"      // 原生依赖缺失模式:隔离目录加载可见失败
	m3LibPathEnv   = "MAA_M3_LIB_PATH"   // 解包出的 libMaaCore.so 绝对路径
	m3ResourceEnv  = "MAA_M3_RESOURCE_ROOT"
	m3UserDirEnv   = "MAA_M3_USER_DIR"
)

// 子进程 stdout 标记:META 在 Open 成功后立即打印(链路中途死亡的
// 诊断);OK 仅在子进程全部断言通过后打印(父进程复验其字段)。
const (
	m3MetaMarker = "MAA_M3_NODEVICE_META"
	m3OKMarker   = "MAA_M3_NODEVICE_OK"
	m3HangMarker = "MAA_M3_HANG_STARTED"
	m3DepsMarker = "MAA_M3_MISSING_DEPS_OK"
)

// 子进程超时与等待参数:链路与依赖缺失路径给足 10 分钟(Core 加载
// 资源在慢环境可能需要时间);挂起路径用 15 秒短超时专门验收超时
// 终止语义。WaitDelay 防止孙进程持有输出管道导致 Wait 挂起。
const (
	m3ChainChildTimeout    = 10 * time.Minute
	m3DepsChildTimeout     = 10 * time.Minute
	m3HangChildTimeout     = 15 * time.Second
	m3HangSleepDuration    = 30 * time.Minute
	m3ChildWaitDelay       = 30 * time.Second
	m3ScreencapWaitTimeout = 5 * time.Minute
	m3ReplayWaitTimeout    = 2 * time.Minute
	m3DrainGuardTimeout    = 30 * time.Second
)

// m3DestroyedMessageID 是 AsstMsg::Destroyed 的消息编号,冻结自
// MaaAssistantArknights src/MaaCore/Common/AsstMsg.h(InternalError=0、
// InitFailed=1、ConnectionInfo=2、AllTasksCompleted=3、AsyncCallInfo=4、
// Destroyed=5)。锁定 Core 在 Destroy 调用线程 join 全部线程后同步
// 触发该消息(Assistant.cpp ~Assistant)。
const m3DestroyedMessageID = MessageID(5)

// m3NoDeviceTaskType 是仅追加不执行的任务类型:Award 的 set_params
// 对未知键取默认值恒返回 true(AwardTask.cpp),未连接、未 Start 的
// 实例只把它留在任务列表中,不产生任何设备动作。
const m3NoDeviceTaskType = "Award"

// TestM3RealCoreNoDeviceChain 验收真实库无设备完整链路(见文件头映
// 射):父进程准备锁定资源与临时用户目录后启动子进程;子进程内断言
// 全部通过才退出 0。退出码非 0、被信号杀死、超时都是本测试的明确
// 失败,不重试;父进程并对 stdout 的 OK 标记行做独立元数据复验。
func TestM3RealCoreNoDeviceChain(t *testing.T) {
	if os.Getenv(m3ChildModeEnv) == m3ModeChain {
		runM3NoDeviceChainChild(t)
		return
	}
	libPath, resourceRoot := requireUnpackedResources(t)
	userDir := t.TempDir() // 子进程专用的可写用户目录

	out := runM3Child(t, m3ChainChildTimeout, m3ModeChain, map[string]string{
		m3LibPathEnv:  libPath,
		m3ResourceEnv: resourceRoot,
		m3UserDirEnv:  userDir,
	})
	if out.err != nil {
		t.Fatalf("chain 子进程失败(非零退出/信号/超时一律视为失败,不重试):\n%s",
			childDiagnostics(m3ModeChain, out))
	}
	if err := verifyM3ChildMetadata(out.stdout); err != nil {
		t.Fatalf("chain 子进程 stdout 元数据复验失败(与子进程内断言独立):\n%v\n%s",
			err, childDiagnostics(m3ModeChain, out))
	}
	t.Logf("chain 子进程内部断言全部通过:\n%s", out.stdout)
	if out.stderr != "" {
		t.Logf("chain 子进程 stderr:\n%s", out.stderr)
	}
	t.Logf("锁定基线:tarball SHA256=%s,core SHA256=%s,resource/version.json SHA256=%s,core version=%s",
		lockedTarballSHA256, lockedCoreSHA256, lockedVersionSHA256, lockedCoreVersion)
}

// TestM3RealCoreExecutorTimeoutKillReportedAsFailure 验收本文件执行
// 器的超时语义:挂起子进程在短超时后被杀死,必须被报告为失败并保留
// 可辨识诊断——超时不是通过,不得 recover 后继续或自动重试。同形态
// 覆盖信号死亡(原生崩溃)的可见性:任何信号终止都映射为
// signaled=true 的失败。
func TestM3RealCoreExecutorTimeoutKillReportedAsFailure(t *testing.T) {
	if os.Getenv(m3ChildModeEnv) == m3ModeHang {
		// 子进程:打印启动标记后长时间 sleep,由父进程的外部超时杀死。
		fmt.Println(m3HangMarker + " started, sleeping " + m3HangSleepDuration.String())
		time.Sleep(m3HangSleepDuration)
		return
	}
	out := runM3Child(t, m3HangChildTimeout, m3ModeHang, nil)
	if out.err == nil {
		t.Fatalf("挂起子进程在 %s 超时后必须被报告为失败,实际退出码 0:\n%s",
			m3HangChildTimeout, childDiagnostics(m3ModeHang, out))
	}
	if !out.timedOut {
		t.Fatalf("失败必须可辨识为超时终止(timedOut=false, err=%v):\n%s",
			out.err, childDiagnostics(m3ModeHang, out))
	}
	if !out.signaled {
		t.Fatalf("超时终止必须经信号杀死子进程(signaled=false, err=%v):\n%s",
			out.err, childDiagnostics(m3ModeHang, out))
	}
	if !strings.Contains(out.stdout, m3HangMarker) {
		t.Fatalf("超时诊断必须保留子进程已产出的输出:\n%s", childDiagnostics(m3ModeHang, out))
	}
	t.Logf("挂起子进程按预期在 %s 超时被 SIGKILL,执行器报告为失败并保留诊断:\n%s",
		m3HangChildTimeout, childDiagnostics(m3ModeHang, out))
}

// TestM3RealCoreMissingNativeDepsReportedAsFailure 验收原生依赖缺失
// 的可见失败:libMaaCore.so 的捆绑依赖(OpenCV/ONNX Runtime 等 8 个
// .so,DT_RUNPATH=$ORIGIN)不在系统默认路径,仅复制 libMaaCore.so 的
// 隔离目录必然 dlopen 失败。子进程断言该失败以 *LibraryLoadError 可
// 见地返回(原始错误直接返回,不包 ErrRuntimeTerminated),且同进程
// 二次 Open 返回 errors.Is(…, ErrRuntimeTerminated)(预占即不可回
// 头);两条断言都通过才退出 0。
func TestM3RealCoreMissingNativeDepsReportedAsFailure(t *testing.T) {
	if os.Getenv(m3ChildModeEnv) == m3ModeMissDeps {
		runM3MissingDepsChild(t)
		return
	}
	libPath, _ := requireUnpackedResources(t)

	// 隔离目录:只放 libMaaCore.so,不含任何捆绑依赖。
	isolated := t.TempDir()
	isolatedLib := filepath.Join(isolated, lockedCoreSOName)
	if err := copyM3File(libPath, isolatedLib); err != nil {
		t.Fatalf("复制 libMaaCore.so 到隔离目录失败: %v", err)
	}

	out := runM3Child(t, m3DepsChildTimeout, m3ModeMissDeps, map[string]string{
		m3LibPathEnv: isolatedLib,
		m3UserDirEnv: t.TempDir(),
	})
	if out.err != nil {
		t.Fatalf("missing-deps 子进程失败(其内部断言未全部通过):\n%s",
			childDiagnostics(m3ModeMissDeps, out))
	}
	if !strings.Contains(out.stdout, m3DepsMarker) {
		t.Fatalf("missing-deps 子进程退出码 0,但 stdout 缺少成功标记:\n%s",
			childDiagnostics(m3ModeMissDeps, out))
	}
	t.Logf("missing-deps 子进程内部断言全部通过:\n%s", out.stdout)
}

// runM3NoDeviceChainChild 是 m3ModeChain 子进程入口:真实锁定库上的
// 无设备公开链路,步骤与断言见文件头映射。红线:全程不调用 Start、
// Connect、Click,不触达任何设备动作。断言失败以非 0 退出。
func runM3NoDeviceChainChild(t *testing.T) {
	libPath := os.Getenv(m3LibPathEnv)
	resourceRoot := os.Getenv(m3ResourceEnv)
	userDir := os.Getenv(m3UserDirEnv)
	for _, e := range []struct{ name, value string }{
		{m3LibPathEnv, libPath}, {m3ResourceEnv, resourceRoot}, {m3UserDirEnv, userDir},
	} {
		if !filepath.IsAbs(e.value) {
			t.Fatalf("%s = %q 不是绝对路径", e.name, e.value)
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
		t.Fatalf("Version() = %q, want %q (锁定基线)", got, lockedCoreVersion)
	}
	if got := rt.NullSize(); got != math.MaxUint64 {
		t.Fatalf("NullSize() = %#x, want %#x (锁定 profile 哨兵)", got, uint64(math.MaxUint64))
	}
	if entries, err := os.ReadDir(userDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Logf("Open 后用户目录条目(仅名称): %v", names)
	}
	caps := rt.Capabilities()
	t.Logf("Open ok: version=%s nullSize=%#x capabilities Create=%v ImageBgr=%v AsyncAttachWindow=%v",
		rt.Version(), rt.NullSize(), caps.Create(), caps.ImageBgr(), caps.AsyncAttachWindow())
	fmt.Printf("%s version=%s nullSize=%d\n", m3MetaMarker, rt.Version(), rt.NullSize())

	// 2. NewInstance:CreateEx(进程唯一 callback trampoline + token)。
	inst, err := rt.NewInstance(InstanceConfig{})
	if err != nil || inst == nil {
		t.Fatalf("NewInstance failed: err=%v, inst=%v", err, inst)
	}
	t.Logf("NewInstance ok: CreateEx 返回有效句柄并发布 ready 实例")

	// 3. 未连接 Screencap:提交被接受(非零 ID)不等于截图成功;最终
	//    结果由真实 AsyncCallInfo 按 async_call_id 关联结算为原生失败。
	op, err := inst.Screencap()
	if err != nil || op == nil {
		t.Fatalf("Screencap submit failed: err=%v, op=%v", err, op)
	}
	if op.ID() == 0 {
		t.Fatalf("Screencap submission returned ID 0")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), m3ScreencapWaitTimeout)
	res, waitErr := op.Wait(waitCtx)
	cancel()
	if waitErr == nil {
		t.Fatalf("未连接的 Screencap 报告成功 %+v:提交成功不得被解释为截图成功", res)
	}
	if !errors.Is(waitErr, ErrNativeRejected) {
		t.Fatalf("未连接 Screencap 的 Wait 错误 = %v, want errors.Is ErrNativeRejected", waitErr)
	}
	if res.OK {
		t.Fatalf("AsyncResult.OK = true, unconnected screencap must fail")
	}
	if res.What != asyncWhatScreencap {
		t.Fatalf("AsyncResult.What = %q, want %q (锁定 Core call_proc 冻结值)", res.What, asyncWhatScreencap)
	}
	if res.ID != op.ID() {
		t.Fatalf("AsyncResult.ID = %d, want 提交 ID %d (async_call_id 关联破坏)", res.ID, op.ID())
	}
	var det struct {
		Ret  *bool  `json:"ret"`
		Cost *int64 `json:"cost"`
	}
	if err := json.Unmarshal(res.Details, &det); err != nil {
		t.Fatalf("AsyncResult.Details 解码失败: %v (raw: %s)", err, res.Details)
	}
	if det.Ret == nil || *det.Ret {
		t.Fatalf("details.ret = %s, want false", res.Details)
	}
	cost := int64(-1)
	if det.Cost != nil {
		cost = *det.Cost
	}
	t.Logf("未连接 Screencap 的真实 AsyncCallInfo 已关联: id=%d what=%s ok=false ret=false cost=%dms details=%s",
		res.ID, res.What, cost, res.Details)

	// 4. 仅追加不执行的任务并读取列表:不 Start、不 Connect、不执行
	//    游戏操作;Connected()==false 佐证全程无连接。
	id1, err := inst.AppendTask(m3NoDeviceTaskType, json.RawMessage(`{"enable":true}`))
	if err != nil || id1 == 0 {
		t.Fatalf("AppendTask#1 = (%d, %v), want (非零, nil)", id1, err)
	}
	id2, err := inst.AppendTask(m3NoDeviceTaskType, json.RawMessage(`{"enable":true}`))
	if err != nil || id2 == 0 {
		t.Fatalf("AppendTask#2 = (%d, %v), want (非零, nil)", id2, err)
	}
	if id2 == id1 {
		t.Fatalf("两次 AppendTask 返回同一 TaskID %d", id1)
	}
	connected, err := inst.Connected()
	if err != nil {
		t.Fatalf("Connected() error: %v", err)
	}
	if connected {
		t.Fatalf("Connected() = true, 未调用 Connect 时不得报告已连接")
	}
	tasks, err := inst.Tasks()
	if err != nil {
		t.Fatalf("Tasks() error: %v", err)
	}
	if len(tasks) != 2 || !slices.Contains(tasks, id1) || !slices.Contains(tasks, id2) {
		t.Fatalf("Tasks() = %v, want 恰含已入队 {%d, %d} 的 2 元列表(顺序由 Core 决定,不核对次序)",
			tasks, id1, id2)
	}
	t.Logf("仅追加未执行: tasks=%v (Award×2, 无 Start/Connect), Connected=false", tasks)

	// 5. Instance.Close:真实一次 Stop(未启动实例返回 true)与一次
	//    Destroy(C void);Destroyed 在 Destroy 调用线程同步触发,经
	//    仍开放的准入门入队,由关闭流程排空后投递公开缓冲。
	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (未启动实例的正常关闭)", err)
	}

	// 5a. Destroyed 的诊断顺序:排空后的公开缓冲恰为 [AsyncCallInfo,
	//     Destroyed],本地序号单调递增;公开 Events 通道已随关闭关闭。
	evs := readM3DrainedEvents(t, inst)
	if len(evs) != 2 {
		t.Fatalf("Close 后公开事件缓冲 = %d 条 (%s), want 恰为 [AsyncCallInfo, Destroyed] 两条",
			len(evs), m3RenderEvents(evs))
	}
	if evs[0].MessageID != asyncCallInfoMessageID {
		t.Fatalf("公开事件[0] messageID = %d, want AsyncCallInfo(%d): %s",
			evs[0].MessageID, asyncCallInfoMessageID, m3RenderEvents(evs))
	}
	if evs[1].MessageID != m3DestroyedMessageID {
		t.Fatalf("公开事件[1] messageID = %d, want Destroyed(%d): %s",
			evs[1].MessageID, m3DestroyedMessageID, m3RenderEvents(evs))
	}
	if evs[0].Sequence != 1 || evs[1].Sequence != 2 || evs[0].Sequence >= evs[1].Sequence {
		t.Fatalf("公开事件本地序号 = (%d, %d), want (1, 2) 单调递增", evs[0].Sequence, evs[1].Sequence)
	}
	t.Logf("Destroyed 诊断顺序确认: AsyncCallInfo(seq=%d, json=%s) → Destroyed(seq=%d, json=%s), 之后通道已关闭",
		evs[0].Sequence, evs[0].JSON, evs[1].Sequence, evs[1].JSON)

	// 5b. Destroy 返回后 callback 契约:关门、退空、移除 token 之后,
	//     真实注册表不再把迟到的原生回调路由到本实例。
	if native.LookupCallbackDelivery(inst.inst.token) != nil {
		t.Fatalf("Close 后 callback token %d 仍可查到投递函数, 迟到回调契约被破坏", inst.inst.token)
	}

	// 5c. 实例关闭后 Operation 仍可等待,取得同一保存终态的独立副本。
	replayCtx, cancel := context.WithTimeout(context.Background(), m3ReplayWaitTimeout)
	res2, replayErr := op.Wait(replayCtx)
	cancel()
	if !errors.Is(replayErr, ErrNativeRejected) {
		t.Fatalf("关闭后重复 Wait 错误 = %v, want errors.Is ErrNativeRejected", replayErr)
	}
	if res2.ID != res.ID || res2.What != res.What || res2.OK != res.OK || !bytes.Equal(res2.Details, res.Details) {
		t.Fatalf("关闭后重复 Wait 结果漂移: first=%+v replay=%+v", res, res2)
	}

	// 6. Runtime.Close:实例已终止使用,重复 Close 直接返回已存储结果
	//    (nil,不再进入 native);此后 NewInstance 一律被拒。
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("Runtime 终止后的 NewInstance 错误 = %v, want errors.Is ErrRuntimeTerminated", err)
	}

	// 7. 全部断言通过后才打印 OK 标记(父进程复验其字段)。
	fmt.Printf("%s version=%s nullSize=%d screencap_id=%d screencap_what=%s screencap_ok=false details_ret=false tasks=2 destroyed_events=1 token_unregistered=true replay=true\n",
		m3OKMarker, rt.Version(), rt.NullSize(), res.ID, res.What)
}

// runM3MissingDepsChild 是 m3ModeMissDeps 子进程入口:以仅含
// libMaaCore.so 的隔离路径 Open,断言失败以 *LibraryLoadError 可见
// 返回且不包 ErrRuntimeTerminated,同进程二次 Open 得
// ErrRuntimeTerminated;两条断言都通过才退出 0。
func runM3MissingDepsChild(t *testing.T) {
	libPath := os.Getenv(m3LibPathEnv)
	userDir := os.Getenv(m3UserDirEnv)
	for _, e := range []struct{ name, value string }{
		{m3LibPathEnv, libPath}, {m3UserDirEnv, userDir},
	} {
		if !filepath.IsAbs(e.value) {
			t.Fatalf("%s = %q 不是绝对路径", e.name, e.value)
		}
	}

	cfg := RuntimeConfig{
		LibraryPath:   libPath,
		UserDir:       userDir,
		ResourceRoots: []string{userDir},
	}
	rt, err := Open(cfg)
	if rt != nil || err == nil {
		t.Fatalf("隔离目录(缺捆绑依赖)的 Open = (%v, %v), want (nil, error)", rt, err)
	}
	var lle *LibraryLoadError
	if !errors.As(err, &lle) {
		t.Fatalf("errors.As 未命中 *LibraryLoadError, got %T: %v", err, err)
	}
	if lle.Path != libPath {
		t.Fatalf("LibraryLoadError.Path = %q, want %q", lle.Path, libPath)
	}
	if errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("原始错误不得包成 ErrRuntimeTerminated: %v", err)
	}
	rt2, err2 := Open(cfg)
	if rt2 != nil {
		t.Fatalf("二次 Open 不得返回 Runtime: %v", rt2)
	}
	if !errors.Is(err2, ErrRuntimeTerminated) {
		t.Fatalf("二次 Open 的错误 = %v, want errors.Is ErrRuntimeTerminated", err2)
	}
	t.Logf("原生依赖缺失可见失败: %v; 二次 Open: errors.Is(ErrRuntimeTerminated)=true", err)
	fmt.Printf("%s errors_as_library_load_error=true second_open_runtime_terminated=true\n", m3DepsMarker)
}

// runM3Child 以 go test 自身二进制重执行一个子进程:-test.run 钉住
// 单个测试名,子进程在该测试函数顶部按 m3ChildModeEnv 守卫分岔。超
// 时经 exec.CommandContext 外部强制(超时杀死是明确的失败,不是通
// 过);WaitDelay 防止孙进程持有输出管道导致 Wait 挂起。不重试、不
// recover:执行结果原样交回调用方判定(形态与 M1 的 runM1Child 一致,
// 独立实现以避免跨任务文件改动)。
func runM3Child(t *testing.T, timeout time.Duration, mode string, extraEnv map[string]string) childOutcome {
	t.Helper()
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("定位测试二进制 %q 失败: %v", os.Args[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+t.Name()+"$", "-test.v=true")
	cmd.Env = m3ChildEnviron(mode)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = m3ChildWaitDelay

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

// m3ChildEnviron 构造子进程环境:与 M1 的 childEnviron 同一策略——
// 剥离动态链接器注入变量(LD_LIBRARY_PATH/LD_PRELOAD/LD_AUDIT)后
// 原样继承,再附上 M3 模式标记。锁定策略不依赖任何 LD_* 配置
// (libMaaCore.so 的依赖只经 $ORIGIN RUNPATH 与系统默认路径解析),
// 父进程环境残留会把子进程的依赖解析带离锁定策略。
func m3ChildEnviron(mode string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key == "LD_LIBRARY_PATH" || key == "LD_PRELOAD" || key == "LD_AUDIT" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, m3ChildModeEnv+"="+mode)
}

// readM3DrainedEvents 在 Instance.Close 返回后读尽公开 Events 缓冲:
// 关闭流程保证 Close 返回前通道已由分发器关闭,缓冲事件仍可读;防
// 挂死守卫仅在契约被破坏(通道未关闭)时兜底为明确失败,不作同步
// 手段。子进程内防挂,父进程外部超时最终兜底。
func readM3DrainedEvents(t *testing.T, inst *Instance) []Event {
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
		case <-time.After(m3DrainGuardTimeout):
			t.Fatalf("Close 返回后 %s 内公开 Events 通道仍未关闭(已读 %d 条), 关闭契约被破坏",
				m3DrainGuardTimeout, len(evs))
		}
	}
}

// m3RenderEvents 渲染事件列表用于失败诊断。
func m3RenderEvents(evs []Event) string {
	parts := make([]string, 0, len(evs))
	for _, ev := range evs {
		parts = append(parts, fmt.Sprintf("{msg=%d seq=%d json=%s}", ev.MessageID, ev.Sequence, ev.JSON))
	}
	return strings.Join(parts, " ")
}

// verifyM3ChildMetadata 对 chain 子进程的 stdout 做父进程侧元数据复
// 验:解析 OK 标记行的 version=/nullSize=/screencap_what=/screencap_
// ok/tasks= 实际值,独立核对锁定基线与链路结论(与子进程内断言互相
// 独立的纵深防御):缺行、格式异常或任一值不符都判定父测试失败。
func verifyM3ChildMetadata(stdout string) error {
	var (
		found      bool
		version    string
		nullSize   string
		what       string
		okField    string
		tasksCount string
	)
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, m3OKMarker+" ") {
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
		return fmt.Errorf("stdout 缺少 %s 元数据行", m3OKMarker)
	}
	if version != lockedCoreVersion {
		return fmt.Errorf("stdout 复验 Version = %q, want %q (锁定基线)", version, lockedCoreVersion)
	}
	gotNullSize, err := strconv.ParseUint(nullSize, 10, 64)
	if err != nil {
		return fmt.Errorf("stdout 复验 nullSize %q 不是十进制 uint64: %w", nullSize, err)
	}
	if gotNullSize != math.MaxUint64 {
		return fmt.Errorf("stdout 复验 NullSize = %d (0x%x), want %d (0x%x, 锁定 profile 哨兵)",
			gotNullSize, gotNullSize, uint64(math.MaxUint64), uint64(math.MaxUint64))
	}
	if what != asyncWhatScreencap {
		return fmt.Errorf("stdout 复验 screencap_what = %q, want %q", what, asyncWhatScreencap)
	}
	if okField != "false" {
		return fmt.Errorf("stdout 复验 screencap_ok = %q, want false", okField)
	}
	if tasksCount != "2" {
		return fmt.Errorf("stdout 复验 tasks = %q, want 2", tasksCount)
	}
	return nil
}

// copyM3File 复制一个普通文件(内容与权限),供隔离目录构造使用。
func copyM3File(src, dst string) error {
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
