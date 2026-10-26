//go:build linux

// 本文件是 MAA-29 的设备连接与截图验收(M4 阶段;绑定设计 §5–§7、§9):
// 在真实设备上验证「显式连接 → Operation 关联 → Connected 查询 →
// Screencap → PNG 缓存读取」的异步提交、最终回调与读取一致性。两个
// 场景均为 MAA-28 显式设备验收入口下的真实设备场景,只执行任务文件
// 明文允许的动作——Connect、Screencap、GetImagePNG 缓存读取、
// Connected 查询与 Close;全程不调用 Click/Start/Stop/AppendTask/
// BackToHome,不提交任何识别任务(允许清单准入归 MAA-30),不枚举
// 设备、不自动导航、不重试或重放任何设备动作。
//
// 与 acceptance_device_linux_test.go(MAA-28 交付)的关系:场景测试
// 以 requireM4DeviceAcceptance 开头(门契约:未显式配置时可见 SKIP,
// 跳过不计为通过);连接/截图场景各自先通过 requireConnectPermission
// / requireScreencapPermission 许可门,才发起任何原生调用。子进程经
// MAA-28 的 runM4DeviceScenario 执行(独立子进程 + 显式外部超时 +
// 单次运行;超时分类 timeout-unknown=结果未知,不是取消,不重放);
// OK 标记由父进程独立复验;证据经 m4WriteScenarioEvidence 去敏写入
// 仓库外的 output_dir。
//
// 与 acceptance_nodevice_linux_test.go(MAA-27 交付)的关系:复用其
// 关闭后排空公开事件缓冲的 readM3DrainedEvents 与锁定基线常量
// (lockedCoreVersion);本文件不改动既有文件。白箱边界与 M3 相同:
// 公开事件流 API 尚未接入(doc.go),事件一致性断言只能经同包的
// disp.events() 读取关闭后排空的公开缓冲;除此之外不读取任何私有
// 状态。
//
// 验收条件 → 用例映射(MAA-29):
//   - 「连接完成与提交 ID 正确关联,Connected=false 查询结果不会被
//     编造成 ABI 错误」:连接场景(及截图场景的连接段)——Connect
//     提交返回非零 ID 只登记为提交成功,连接完成一律以 Wait 的最终
//     结果确认(ID、what、ret 三项核对);连接前 Connected()==
//     (false, nil) 是正常查询结果,不转换成错误;连接成功后
//     Connected()==(true, nil)。
//   - 「PNG 在 Screencap 成功后获取且可解码,不把缓存读取等同于隐
//     式采集新截图」:截图场景——GetImagePNG 严格发生在 Screencap
//     的 Wait 成功之后;两次缓存读取之间没有提交任何新采集,事件流
//     恰一条 what=Screencap 的 AsyncCallInfo(隐式采集若存在会留下
//     第二条),两次读取字节一致(同一缓存帧);PNG 经 image/png 解
//     码且尺寸合法。缓存图像不携带采集时间,binding 不承诺新鲜度,
//     该限制记入证据元数据(png_cache_note)。
//   - 「Wait 取消仅终止该次等待,后续可读取结果且没有重复提交设备
//     动作」:连接与截图提交后各自先做一次预取消 Wait(返回
//     context.Canceled 与零值结果,不改写终态、不撤销在途原生动作),
//     再等待取得真实最终结果;每类设备动作在场景内只提交一次(事件
//     流恰一条对应 AsyncCallInfo,重复 Wait 的结果 ID 与提交 ID 一
//     致——重复提交会得到不同 ID 与第二条事件)。
//   - 「完成/失败事件和 Operation 结果一致,关闭无死锁且进程正常退
//     出」:关闭后排空公开事件缓冲,AsyncCallInfo 与各 Operation 的
//     ID/what/ret 逐一一致且无多余条目,最后一条事件为 Destroyed;
//     Instance.Close 与 Runtime.Close 返回 nil,子进程以退出码 0 与
//     OK 标记结束(死锁由外部超时可见化为 timeout-unknown,不算通
//     过)。
//   - 「记录精确支持组合及证据;失败未被静默跳过,未执行任何许可范
//     围之外的任务」:证据元数据记录 Go/OS/架构、锁定 Core 版本与场
//     景结论(去敏,写回读校验不含设备地址);子进程内部断言失败即
//     非 0 退出(父进程判 failed,不静默跳过);许可门在任何原生调
//     用之前拒绝越权场景。
//
// 隐私纪律:设备地址、ADB 路径与全部真实配置只存在于仓库外的显式
// 配置文件(MAA_M4_DEVICE_CONFIG 指向);本文件与 OK 标记只输出 ID、
// what、ret、PNG 字节数/尺寸等去敏元数据,不打印原始事件 JSON(其中
// 含设备 uuid)与任何配置值;证据文件由写入器统一去敏并读回校验。
// 不提交截图、运行日志与设备私有信息。
//
// 复现命令(linux amd64;配置与输出必须在仓库外,配置格式见
// acceptance_device_linux_test.go 文件头):
//
//	MAA_M4_DEVICE_CONFIG=/absolute/path/device-acceptance.json \
//	  CGO_ENABLED=0 go test ./packages/maa-go/ \
//	  -run 'TestM4Device(Connect|Screencap)Scenario' -count=1 -v
package maa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// MAA-29 子进程模式标记:与 MAA-28 入口的 refuse-probe/smoke/hang/
// fail-exit 及 M1/M3 的模式互相独立,互不触发对方的守卫;场景子进程
// 经 -test.run 钉住单个测试名,在测试函数顶部按本标记分岔。
const (
	m4ModeConnectScenario   = "connect-scenario"
	m4ModeScreencapScenario = "screencap-scenario"
)

// 子进程 stdout 成功标记:仅在子进程全部断言通过后打印,父进程独立
// 复验其字段(与 M3/M4 冒烟相同的纵深防御)。
const (
	m4ConnectOKMarker   = "MAA_M4_CONNECT_OK"
	m4ScreencapOKMarker = "MAA_M4_SCREENCAP_OK"
)

// 超时参数:场景子进程 10 分钟(Core 加载约 280MB 资源 + adb 连接在
// 慢环境下需要时间,与 M3 链路同参数;死锁由外部超时可见化为
// timeout-unknown,不算通过);单次连接/截图的最终 Wait 上限 5 分钟
// ——超时是本场景的明确失败,不重试、不重发设备动作。
const (
	m4ScenarioChildTimeout = 10 * time.Minute
	m4ActionWaitTimeout    = 5 * time.Minute
)

// TouchMode 实例选项:场景创建实例时显式设置 TouchMode="adb"——普通
// ADB 输入路径,不依赖 minitouch 的 /dev/input 设备探测(本验收设备
// 没有可用的 minitouch 触摸设备,General 默认派生的 minitouch 以
// ConnectionInfo what=TouchModeNotAvailable 失败);这不是被排除的
// MaaFwAdb profile,仍在首期普通 ADB 控制路径之内。场景固定该选项以
// 保证可复现性:任何 adb 可用的设备都支持它。
const (
	// m4TouchModeOptionKey 是 InstanceOptionKey::TouchMode 的锁定值
	// (2),冻结自绑定设计 §3.0(常量语义来自 AsstTypes.h 的
	// InstanceOptionKey,ABI 宽度由 AsstCaller.h 的 int32_t typedef
	// 确定)。
	m4TouchModeOptionKey = int32(2)
	// m4TouchModeADB 是 TouchMode 实例选项的 "adb" 取值,冻结自锁定
	// Core 的 Assistant::set_instance_option(Assistant.cpp)。
	m4TouchModeADB = "adb"
)

// m4DestroyedMessageID 是 AsstMsg::Destroyed 的消息编号,冻结自
// MaaAssistantArknights src/MaaCore/Common/AsstMsg.h(InternalError=0、
// InitFailed=1、ConnectionInfo=2、AllTasksCompleted=3、AsyncCallInfo=4、
// Destroyed=5);关闭流程保证它是公开缓冲中最后一条事件(MAA-27 已对
// 同一关闭流程验收该顺序)。
const m4DestroyedMessageID = MessageID(5)

// m4AsyncCallEvent 是 AsyncCallInfo 事件的必要关联字段(与
// parseAsyncCallInfo 同一 JSON 形状;uuid 与未知字段按原文保留、不解
// 码、不打印——其中含设备 uuid,属私有信息)。
type m4AsyncCallEvent struct {
	What        string `json:"what"`
	AsyncCallID int64  `json:"async_call_id"`
	Details     struct {
		Ret  *bool  `json:"ret"`
		Cost *int64 `json:"cost"`
	} `json:"details"`
}

// m4OpenLockedSnapshot 在子进程内经公开 API 初始化配置指定的锁定
// Core/资源快照并创建实例;Version 必须等于锁定基线——快照被调包
// 即明确失败,不是 skip。
func m4OpenLockedSnapshot(t *testing.T, cfg *deviceAcceptanceConfig) (*Runtime, *Instance) {
	t.Helper()
	rt, err := Open(RuntimeConfig{
		LibraryPath:   cfg.Core.LibraryPath,
		UserDir:       cfg.Core.UserDir,
		ResourceRoots: []string{cfg.Core.ResourceRoot},
	})
	if err != nil {
		t.Fatalf("Open(锁定 Core/资源快照) 失败: %v", err)
	}
	if got := rt.Version(); got != lockedCoreVersion {
		t.Fatalf("Version() = %q, want %q (锁定基线;Core 快照与锁定基线不符)", got, lockedCoreVersion)
	}
	// 实例选项:显式 TouchMode="adb"(普通 ADB 输入路径;场景级配置
	// 决策,见上方常量说明),不改变许可范围与设备动作集合。
	inst, err := rt.NewInstance(InstanceConfig{
		Options: []InstanceOption{{Key: m4TouchModeOptionKey, Value: m4TouchModeADB}},
	})
	if err != nil || inst == nil {
		t.Fatalf("NewInstance failed: err=%v, inst=%v", err, inst)
	}
	return rt, inst
}

// m4RequireNotConnected 验收连接前的 Connected 查询语义:(false, nil)
// 是正常查询结果,不得被编造成 ABI 错误(验收条件 1)。
func m4RequireNotConnected(t *testing.T, inst *Instance) {
	t.Helper()
	connected, err := inst.Connected()
	if err != nil {
		t.Fatalf("连接前 Connected() = (_, %v): false 是正常查询结果,不得编造成 ABI 错误", err)
	}
	if connected {
		t.Fatalf("连接前 Connected() = true, 未连接时不得报告已连接")
	}
}

// m4ConnectDevice 显式连接配置指定的设备并等待最终结果(执行步骤 1–2):
//  1. Connect 提交返回非零 ID——只登记为提交成功,不以非零 ID 代替
//     连接完成;
//  2. 预取消的一次 Wait:返回 context.Canceled 与零值结果,仅终止该
//     次等待,不改写 Operation 终态、不撤销已提交的原生动作(验收条
//     件 3);
//  3. 再次 Wait 取得真实最终结果:ID 与提交 ID 一致(没有重复提交设
//     备动作)、What=Connect、OK=true、details.ret=true。
func m4ConnectDevice(t *testing.T, inst *Instance, cfg *deviceAcceptanceConfig) AsyncResult {
	t.Helper()
	op, err := inst.Connect(ConnectConfig{
		ADBPath: cfg.Device.ADBPath,
		Address: cfg.Device.Address,
		Config:  cfg.Device.Config,
	})
	if err != nil || op == nil {
		t.Fatalf("Connect submit failed: err=%v, op=%v", err, op)
	}
	if op.ID() == 0 {
		t.Fatalf("Connect submission returned ID 0")
	}
	m4RequireCanceledWait(t, op)
	waitCtx, cancel := context.WithTimeout(context.Background(), m4ActionWaitTimeout)
	defer cancel()
	res, err := op.Wait(waitCtx)
	if err != nil {
		t.Fatalf("连接的最终 Wait 失败 (提交 id=%d): %v", op.ID(), err)
	}
	if res.ID != op.ID() || res.What != asyncWhatConnect || !res.OK {
		t.Fatalf("连接结果关联不符: id=%d what=%q ok=%v, want id=%d what=%q ok=true",
			res.ID, res.What, res.OK, op.ID(), asyncWhatConnect)
	}
	m4RequireDetailsRetTrue(t, res)
	return res
}

// m4RequireCanceledWait 对刚提交的 Operation 做一次预取消等待:上下
// 文已取消时,该次等待必须返回 context.Canceled 与零值结果——取消仅
// 终止该次等待(§7、§8.1);真实设备动作仍在 Core 内执行,不撤销、
// 不重发,随后的一次 Wait 仍能取得最终结果(取消不使 pending 提前
// 回收)。
func m4RequireCanceledWait(t *testing.T, op *Operation) {
	t.Helper()
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := op.Wait(cancelCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("预取消的 Wait = (%+v, %v), want (零值, context.Canceled): 取消必须只终止该次等待", res, err)
	}
	if res.ID != 0 || res.What != "" || res.OK || len(res.Details) != 0 {
		t.Fatalf("取消的等待返回非零结果 %+v: 取消不得编造结果", res)
	}
}

// m4RequireDetailsRetTrue 解析 AsyncResult.Details 的 ret 字段(与 M3
// 同一冻结形状 {"ret":bool,"cost":int64}):缺失或非 true 均为失败,
// 不把矛盾证据伪装成成功。返回 cost(缺失时 -1)。
func m4RequireDetailsRetTrue(t *testing.T, res AsyncResult) int64 {
	t.Helper()
	var det struct {
		Ret  *bool  `json:"ret"`
		Cost *int64 `json:"cost"`
	}
	if err := json.Unmarshal(res.Details, &det); err != nil {
		t.Fatalf("AsyncResult.Details 解码失败 (what=%s, id=%d): %v", res.What, res.ID, err)
	}
	if det.Ret == nil || !*det.Ret {
		t.Fatalf("details.ret 不为 true (what=%s, id=%d): 不把矛盾证据伪装成成功", res.What, res.ID)
	}
	if det.Cost != nil {
		return *det.Cost
	}
	return -1
}

// m4ParseAsyncCallEvents 从关闭后排空的公开事件中筛选并解析全部
// AsyncCallInfo 事件:解码失败是明确失败(完成/失败事件不得被静默忽
// 略);原始 JSON(含设备 uuid)不进入任何日志或诊断。
func m4ParseAsyncCallEvents(t *testing.T, evs []Event) []m4AsyncCallEvent {
	t.Helper()
	var calls []m4AsyncCallEvent
	for _, ev := range evs {
		if ev.MessageID != asyncCallInfoMessageID {
			continue
		}
		var c m4AsyncCallEvent
		if err := json.Unmarshal(ev.JSON, &c); err != nil {
			t.Fatalf("AsyncCallInfo 事件解码失败 (seq=%d): %v", ev.Sequence, err)
		}
		calls = append(calls, c)
	}
	return calls
}

// m4RenderAsyncCallIDs 渲染 AsyncCallInfo 事件的 what/id/ret 列表(去
// 敏:只含关联字段,不含原始 JSON)。
func m4RenderAsyncCallIDs(calls []m4AsyncCallEvent) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		ret := c.Details.Ret != nil && *c.Details.Ret
		parts = append(parts, fmt.Sprintf("{what=%s id=%d ret=%v}", c.What, c.AsyncCallID, ret))
	}
	return strings.Join(parts, " ")
}

// m4RequireConsistentCompletion 验收完成事件与 Operation 结果一致
// (验收条件 4):事件流恰含 want 列出的每条 AsyncCallInfo(what 与
// async_call_id 逐一对应、details.ret=true),没有多余条目——多余条
// 目意味着发生了许可范围之外的设备动作;事件本地序号单调递增;最后
// 一条事件为 Destroyed(关闭诊断顺序)。
func m4RequireConsistentCompletion(t *testing.T, evs []Event, want map[AsyncID]string) {
	t.Helper()
	calls := m4ParseAsyncCallEvents(t, evs)
	if len(calls) != len(want) {
		t.Fatalf("AsyncCallInfo 事件 = %d 条 (%s), want 恰 %d 条(每类设备动作恰好提交一次,无隐式重复提交)",
			len(calls), m4RenderAsyncCallIDs(calls), len(want))
	}
	seen := make(map[AsyncID]string, len(calls))
	for _, c := range calls {
		if c.Details.Ret == nil || !*c.Details.Ret {
			t.Fatalf("完成事件 ret 不为 true: what=%s async_call_id=%d", c.What, c.AsyncCallID)
		}
		if prev, dup := seen[AsyncID(c.AsyncCallID)]; dup {
			t.Fatalf("完成事件 id=%d 重复 (先前 what=%s): 不允许重复提交或重复结算", c.AsyncCallID, prev)
		}
		seen[AsyncID(c.AsyncCallID)] = c.What
	}
	for id, what := range want {
		got, ok := seen[id]
		if !ok {
			t.Fatalf("完成事件缺少提交 id=%d 的关联 (事件: %s)", id, m4RenderAsyncCallIDs(calls))
		}
		if got != what {
			t.Fatalf("提交 id=%d 的完成事件 what = %q, want %q", id, got, what)
		}
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Sequence <= evs[i-1].Sequence {
			t.Fatalf("事件本地序号非单调递增: seq[%d]=%d, seq[%d]=%d",
				i-1, evs[i-1].Sequence, i, evs[i].Sequence)
		}
	}
	if len(evs) == 0 || evs[len(evs)-1].MessageID != m4DestroyedMessageID {
		t.Fatalf("公开事件缓冲的最后一条不是 Destroyed (%d 条事件): 关闭诊断顺序被破坏", len(evs))
	}
}

// runM4ConnectScenarioChild 是连接场景子进程入口(执行步骤 1–2、5):
// 初始化锁定快照、显式连接指定设备、核对 ID/what/ret 关联与 Connected
// 查询语义,然后单次关闭、正常退出。断言失败即非 0 退出(父进程判
// failed),不静默跳过。
func runM4ConnectScenarioChild(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("连接场景未获许可: %v", err)
	}
	rt, inst := m4OpenLockedSnapshot(t, cfg)
	m4RequireNotConnected(t, inst)

	res := m4ConnectDevice(t, inst, cfg)

	connected, err := inst.Connected()
	if err != nil {
		t.Fatalf("连接成功后 Connected() error: %v", err)
	}
	if !connected {
		t.Fatalf("连接成功后 Connected() = false, want true")
	}

	// 关闭并核对退出(执行步骤 5):Instance.Close 与 Runtime.Close 正
	// 常返回 nil,事件排空恰一条 Connect 完成且最后一条为 Destroyed,
	// 此后 Runtime 终止使用;子进程以退出码 0 结束即正常退出证据。
	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (正常关闭无死锁)", err)
	}
	evs := readM3DrainedEvents(t, inst)
	m4RequireConsistentCompletion(t, evs, map[AsyncID]string{res.ID: asyncWhatConnect})

	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("Runtime 终止后的 NewInstance 错误 = %v, want errors.Is ErrRuntimeTerminated", err)
	}
	fmt.Printf("%s version=%s connect_id=%d connect_what=%s connect_ret=true connected_pre=false connected_post=true canceled_wait=true async_call_events=1 connect_cost=%d\n",
		m4ConnectOKMarker, rt.Version(), res.ID, res.What, m4RequireDetailsRetTrue(t, res))
}

// runM4ScreencapScenarioChild 是截图场景子进程入口(执行步骤 3–5):
// 连接设备后提交一次截图,预取消等待后取得成功结果,再读取缓存 PNG
// 并解码;截图动作只提交一次,缓存读取不隐式采集,关闭并正常退出。
func runM4ScreencapScenarioChild(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	// 截图场景先连接设备:connect 未许可时在原生调用之前拒绝——许可
	// 范围之外的设备动作一律不执行。
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("截图场景需要先连接设备,连接未获许可: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("截图场景未获许可: %v", err)
	}
	rt, inst := m4OpenLockedSnapshot(t, cfg)
	m4RequireNotConnected(t, inst)

	connRes := m4ConnectDevice(t, inst, cfg)
	if connected, err := inst.Connected(); err != nil || !connected {
		t.Fatalf("连接成功后 Connected() = (%v, %v), want (true, nil)", connected, err)
	}

	// Screencap:提交一次;预取消等待只终止该次等待;再次等待取得成功
	// 结果(ID 与提交 ID 一致——没有重复提交设备动作)。
	op, err := inst.Screencap()
	if err != nil || op == nil {
		t.Fatalf("Screencap submit failed: err=%v, op=%v", err, op)
	}
	if op.ID() == 0 {
		t.Fatalf("Screencap submission returned ID 0")
	}
	m4RequireCanceledWait(t, op)
	waitCtx, cancel := context.WithTimeout(context.Background(), m4ActionWaitTimeout)
	defer cancel()
	res, err := op.Wait(waitCtx)
	if err != nil {
		t.Fatalf("截图的最终 Wait 失败 (提交 id=%d): %v", op.ID(), err)
	}
	if res.ID != op.ID() || res.What != asyncWhatScreencap || !res.OK {
		t.Fatalf("截图结果关联不符: id=%d what=%q ok=%v, want id=%d what=%q ok=true",
			res.ID, res.What, res.OK, op.ID(), asyncWhatScreencap)
	}
	screencapCost := m4RequireDetailsRetTrue(t, res)

	// PNG 缓存读取:严格发生在 Screencap 成功之后(执行步骤 3)。读取
	// 是 Core 缓存读取,不隐式采集新截图;PNG 不携带采集时间,binding
	// 不承诺新鲜度——该限制记入证据元数据(png_cache_note)。
	first, err := inst.GetImagePNG()
	if err != nil {
		t.Fatalf("Screencap 成功后 GetImagePNG 失败: %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("Screencap 成功后 GetImagePNG = 0 字节, want 合法 PNG")
	}
	firstImg, err := png.Decode(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("Screencap 成功后读取的 PNG 解码失败 (%d 字节): %v", len(first), err)
	}
	bounds := firstImg.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		t.Fatalf("PNG 尺寸非法: %dx%d", bounds.Dx(), bounds.Dy())
	}

	// 第二次缓存读取:两次读取之间没有提交任何新采集;事件流将证明恰
	// 一条 what=Screencap 的 AsyncCallInfo(隐式采集若存在会留下第二
	// 条),同一缓存帧的两次编码字节逐位一致。
	second, err := inst.GetImagePNG()
	if err != nil {
		t.Fatalf("第二次 GetImagePNG 失败: %v", err)
	}
	secondImg, err := png.Decode(bytes.NewReader(second))
	if err != nil {
		t.Fatalf("第二次读取的 PNG 解码失败 (%d 字节): %v", len(second), err)
	}
	if secondImg.Bounds().Dx() != bounds.Dx() || secondImg.Bounds().Dy() != bounds.Dy() {
		t.Fatalf("两次缓存读取的 PNG 尺寸不一致: %dx%d vs %dx%d",
			bounds.Dx(), bounds.Dy(), secondImg.Bounds().Dx(), secondImg.Bounds().Dy())
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("两次缓存读取的字节不一致: 缓存读取不得隐式采集新截图,同一缓存帧不得漂移")
	}

	// 关闭并核对退出(执行步骤 5),事件一致性含连接与截图两条。
	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (正常关闭无死锁)", err)
	}
	evs := readM3DrainedEvents(t, inst)
	m4RequireConsistentCompletion(t, evs, map[AsyncID]string{
		connRes.ID: asyncWhatConnect,
		res.ID:     asyncWhatScreencap,
	})

	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("Runtime 终止后的 NewInstance 错误 = %v, want errors.Is ErrRuntimeTerminated", err)
	}
	fmt.Printf("%s version=%s connect_id=%d connect_ret=true screencap_id=%d screencap_what=%s screencap_ret=true connected_pre=false connected_post=true canceled_wait=true png_bytes=%d png_w=%d png_h=%d png_reads=2 png_identical=true screencap_events=1 async_call_events=2 screencap_cost=%d\n",
		m4ScreencapOKMarker, rt.Version(), connRes.ID, res.ID, res.What,
		len(first), bounds.Dx(), bounds.Dy(), screencapCost)
}

// TestM4DeviceConnectScenario 连接场景(MAA-29 执行步骤 1–2、5):仅
// 在显式配置 + connect 许可下,经独立子进程连接真实设备并核对关联;
// 父进程复验 OK 标记并写入去敏证据。未显式配置时由入口门 SKIP(跳过
// 不计为通过);许可缺失时在任何原生调用之前拒绝。
func TestM4DeviceConnectScenario(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4ModeConnectScenario {
		runM4ConnectScenarioChild(t)
		return
	}
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("连接场景未获许可,拒绝执行任何原生连接调用: %v", err)
	}
	out := runM4DeviceScenario(t, m4ScenarioChildTimeout, m4ModeConnectScenario, map[string]string{
		m4ConfigEnv: os.Getenv(m4ConfigEnv),
	})
	if out.err != nil {
		t.Fatalf("connect-scenario 子进程失败(非零退出/信号/超时一律视为失败,不重试、不重放设备动作):\n%s",
			m4RenderScenarioDiagnostics(m4ModeConnectScenario, out))
	}
	fields, err := verifyM4ConnectMetadata(out.stdout)
	if err != nil {
		t.Fatalf("connect-scenario 子进程 stdout 元数据复验失败(与子进程内断言独立):\n%v\n%s",
			err, m4RenderScenarioDiagnostics(m4ModeConnectScenario, out))
	}
	m4WriteRedactedEvidence(t, cfg, m4ModeConnectScenario, m4EvidenceFields(fields))
}

// TestM4DeviceScreencapScenario 截图场景(MAA-29 执行步骤 3–5):仅在
// 显式配置 + connect/screencap 双许可下,经独立子进程连接设备、截图、
// 读取并解码 PNG;父进程复验 OK 标记并写入去敏证据。截图场景先连接
// 设备,connect 未许可时在任何原生调用之前拒绝。
func TestM4DeviceScreencapScenario(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4ModeScreencapScenario {
		runM4ScreencapScenarioChild(t)
		return
	}
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("截图场景需要先连接设备,连接未获许可,拒绝执行: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("截图场景未获许可,拒绝执行任何原生截图调用: %v", err)
	}
	out := runM4DeviceScenario(t, m4ScenarioChildTimeout, m4ModeScreencapScenario, map[string]string{
		m4ConfigEnv: os.Getenv(m4ConfigEnv),
	})
	if out.err != nil {
		t.Fatalf("screencap-scenario 子进程失败(非零退出/信号/超时一律视为失败,不重试、不重放设备动作):\n%s",
			m4RenderScenarioDiagnostics(m4ModeScreencapScenario, out))
	}
	fields, err := verifyM4ScreencapMetadata(out.stdout)
	if err != nil {
		t.Fatalf("screencap-scenario 子进程 stdout 元数据复验失败(与子进程内断言独立):\n%v\n%s",
			err, m4RenderScenarioDiagnostics(m4ModeScreencapScenario, out))
	}
	m4WriteRedactedEvidence(t, cfg, m4ModeScreencapScenario, m4EvidenceFields(fields))
}

// m4ParseMarkerFields 定位子进程 stdout 中的 OK 标记行并解析全部
// key=value 字段(缺行即失败)。
func m4ParseMarkerFields(stdout, marker string) (map[string]string, error) {
	line, ok := m4FindMarkerLine(stdout, marker)
	if !ok {
		return nil, fmt.Errorf("stdout 缺少 %s 元数据行", marker)
	}
	fields := make(map[string]string)
	for _, f := range strings.Fields(line)[1:] {
		if k, v, found := strings.Cut(f, "="); found {
			fields[k] = v
		}
	}
	return fields, nil
}

// verifyM4ConnectMetadata 父侧复验连接场景 OK 标记(与子进程内断言互
// 相独立的纵深防御):固定字段逐一核对,提交 ID 必须为非零整数。返回
// 解析出的字段供证据写入复用。
func verifyM4ConnectMetadata(stdout string) (map[string]string, error) {
	fields, err := m4ParseMarkerFields(stdout, m4ConnectOKMarker)
	if err != nil {
		return nil, err
	}
	want := map[string]string{
		"version":           lockedCoreVersion,
		"connect_what":      asyncWhatConnect,
		"connect_ret":       "true",
		"connected_pre":     "false",
		"connected_post":    "true",
		"canceled_wait":     "true",
		"async_call_events": "1",
	}
	for k, w := range want {
		if got := fields[k]; got != w {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want %q", k, got, w)
		}
	}
	if id, err := strconv.Atoi(fields["connect_id"]); err != nil || id == 0 {
		return fields, fmt.Errorf("OK 标记字段 connect_id = %q, want 非零整数", fields["connect_id"])
	}
	return fields, nil
}

// verifyM4ScreencapMetadata 父侧复验截图场景 OK 标记:固定字段、非零
// ID、PNG 字节数与尺寸为正、两次读取一致、截图完成事件恰一条(隐式
// 采集与重复提交都会破坏该计数)。返回解析出的字段供证据写入复用。
func verifyM4ScreencapMetadata(stdout string) (map[string]string, error) {
	fields, err := m4ParseMarkerFields(stdout, m4ScreencapOKMarker)
	if err != nil {
		return nil, err
	}
	want := map[string]string{
		"version":           lockedCoreVersion,
		"connect_ret":       "true",
		"screencap_what":    asyncWhatScreencap,
		"screencap_ret":     "true",
		"connected_pre":     "false",
		"connected_post":    "true",
		"canceled_wait":     "true",
		"png_reads":         "2",
		"png_identical":     "true",
		"screencap_events":  "1",
		"async_call_events": "2",
	}
	for k, w := range want {
		if got := fields[k]; got != w {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want %q", k, got, w)
		}
	}
	for _, k := range []string{"connect_id", "screencap_id"} {
		if id, err := strconv.Atoi(fields[k]); err != nil || id == 0 {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want 非零整数", k, fields[k])
		}
	}
	for _, k := range []string{"png_bytes", "png_w", "png_h"} {
		if n, err := strconv.Atoi(fields[k]); err != nil || n <= 0 {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want 正整数", k, fields[k])
		}
	}
	return fields, nil
}

// m4EvidenceFields 在场景 OK 标记字段之上补充精确支持组合与缓存限制
// 说明(全部为去敏元数据,不含设备地址与原始运行数据):Go/OS/架构
// 与锁定 Core 版本记录本次支持的精确组合;png_cache_note 记录「缓存
// 读取不承诺采集时间」的限制。
func m4EvidenceFields(marker map[string]string) map[string]string {
	fields := make(map[string]string, len(marker)+5)
	for k, v := range marker {
		fields[k] = v
	}
	fields["go"] = runtime.Version()
	fields["os_arch"] = runtime.GOOS + "/" + runtime.GOARCH
	fields["core_version"] = lockedCoreVersion
	fields["core_snapshot"] = "sha256-pinned locked release tarball"
	fields["png_cache_note"] = "PNG 在 Screencap 成功后读取,属缓存读取:不带采集时间,binding 不承诺新鲜度;两次读取均未提交新采集(screencap_events 由子进程事件断言)"
	return fields
}

// m4WriteRedactedEvidence 写入场景证据元数据并读回校验:文件不得包含
// 设备地址(写入器去敏之外的二道防线)。路径为配置指定的仓库外
// output_dir。
func m4WriteRedactedEvidence(t *testing.T, cfg *deviceAcceptanceConfig, scenario string, fields map[string]string) {
	t.Helper()
	path := m4WriteScenarioEvidence(t, cfg, scenario, fields)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回证据文件失败: %v", err)
	}
	if cfg.Device.Address != "" && strings.Contains(string(data), cfg.Device.Address) {
		t.Fatalf("证据文件包含设备地址,去敏失效: %s", path)
	}
}
