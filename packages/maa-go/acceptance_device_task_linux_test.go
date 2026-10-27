//go:build linux

// 本文件是 MAA-30 的无消耗识别任务验收（M4 阶段；绑定设计 §1、§5、§7、§9）：
// 在真实设备上验证「AppendTask 入队 → Tasks 核对 → Start 受理 → 任务消息
// 与终态关联 → Stop 受理与收敛 → Close 与子进程退出」的任务执行真实语义。
// 任务完成依照锁定 Core 的任务消息判定，与 Connect/Screencap 的 Operation
// 完成语义严格区分：Operation 的 Wait 成功只证明该次原生调用完成，任务队
// 列的完成只认 AllTasksCompleted(3) 任务消息。
//
// # 选定的无消耗识别任务：Depot（仓库识别）
//
// 允许清单只登记 Depot 一个任务（任务文件执行步骤 1：先审定具体识别任务
// 并登记许可；无法证明则整任务阻塞——本场景随任务文件提交即为审定通过
// 的登记）。证据（核对自锁定 Core commit d94dd007aca3b519395fb1f0aee26c
// 7bd5ba5d5f 的源码——已逐文件确认与本地参考仓库 HEAD 一致——与锁定发行
// 包资源，整包 SHA256 814a128d…、libMaaCore.so 1e44dbf3…，与 MAA-08/29 冻
// 结基线一致）：
//
//   - 存在性：Assistant.cpp:337 经 append_task 的类型分支注册 DepotTask；
//     Interface/DepotTask.h:9 定义 TaskType="Depot"；锁定包
//     resource/tasks/tasks.json 含完整 pipeline（Depot、DepotBegin、
//     DepotAllTab、DepotMaterialTab、DepotMaterialTabClicked、
//     DepotSlowlySwipeToTheRight），resource/template/Depot/ 模板齐全。
//   - 非消耗性：DepotTask.cpp:7-18 的子任务仅 DepotBegin（导航
//     ProcessTask，ignore_error）与 DepotRecognitionTask（识别，
//     retry_times=0）；DepotRecognitionTask.cpp 的全部设备动作 =
//     ctrler()->get_image()（截图）、swipe()→DepotSlowlySwipeToTheRight
//     （Swipe，纯界面浏览滚动）、点击「全部」标签页（DepotAllTab，
//     ClickSelf）——没有理智/物品/招募/寻访/购买等资源消耗动作；滑动与
//     标签页点击只改变界面浏览状态，不消耗任何游戏内资源。
//   - 参数：DepotTask 未覆写 set_params（InterfaceTask.h:18 默认接受任意
//     JSON object），Assistant.cpp:354 只读取 "enable"。本场景登记
//     {"enable":true,"maa30_probe_bigint":9007199254740993}——其中
//     maa30_probe_bigint 是 Core 保留不解释的未知字段大整数探针
//     （9007199254740993 > 2^53）：允许清单按 json.Number 字面形式精确匹
//     配，Core 接受该 params 并完成任务，即证明绑定层未因未知字段/大整数
//     拒绝或破坏提交。逐字节透传的白箱断言由 instance_sync_test.go
//     （MAA-22）覆盖，本场景不重复实现。
//   - 前置画面：DepotBegin 为 JustReturn + next 导航链（tasks.json），任
//     务自行从游戏内画面导航到仓库页；场景在 AppendTask 之前截取一次前置
//     画面 PNG 落盘到 output_dir，供执行者人工核对——场景不解析画面内容、
//     不自动导航；执行者核对发现设备不在游戏内画面时停止运行任务场景
//     （任务文件执行步骤 2：条件不满足则停止）。判据要点：游戏进程在前台
//     不等于游戏内画面——仅启动应用后停在游戏标题画面（START 按钮、版本
//     号与城市介绍轮播）时，进入游戏需要点击开始，属未授权交互，此时必须
//     停止，不得由场景点击或导航；标题/登录/更新确认画面均不满足前置条件。
//   - 终态：任务最终成功 = AllTasksCompleted(3) 且顶层 finished_tasks 含
//     本任务 TaskID（Assistant.cpp:700-702），TaskChainCompleted(10002)
//     （Assistant.cpp:677-682，仅任务链 ret=true 时发出）与 DepotInfo
//     （SubTaskExtraInfo=20003，details.done=true，
//     DepotRecognitionTask.cpp:118-136）佐证；任务链失败 = TaskChainError
//     (10000)；停止 = TaskChainStopped(10004)（Assistant.cpp:677、711-712
//     ——stop 清空任务队列并置 m_thread_idle 后，working_proc 不再发出
//     AllTasksCompleted，被中断的任务不结算为成功）。
//
// # 场景
//
// 三个场景均在 MAA-28 显式设备验收入口下经 runM4DeviceScenario 于独立子
// 进程执行（显式外部超时 + 单次运行不重试不重放；超时分类 timeout-unknown
// = 结果未知，不是取消，不算通过；子进程断言失败即非 0 退出成为可见失
// 败）：
//
//   - depot-precondition-scenario（前置画面核对）：连接、截图一次并把 PNG
//     落盘 output_dir 供执行者人工核对设备画面；不执行任何任务动作——任
//     务场景在执行者核对前置画面后才运行。
//   - depot-success-scenario（成功场景）：同一未关闭实例连续执行两轮 Depot
//     任务（每轮 AppendTask → Tasks 核对 → Start 受理 → AllTasksCompleted
//     终态关联），随后空闲 Stop 受理与 Running 收敛，Close 与 Runtime 终
//     止——多轮复用是同一存活实例，不通过同进程销毁重建模拟（§8）。
//   - depot-stop-scenario（停止场景）：任务链开始（TaskChainStart）后按场
//     景约定请求一次 Stop，核对受理（nil）与收敛（TaskChainStopped +
//     Running=false），并断言被中断的任务不产生 AllTasksCompleted/
//     TaskChainCompleted/done=true 识别结果（中断不被记为成功或已取消；
//     本场景不重试、不重播原任务）。
//
// # 运行期事件观察（白箱边界）
//
// 公开事件流 API 尚未接入（doc.go），与 MAA-27/29 的白箱边界相同：场景经
// 同包的 disp.events() 读取公开 Events 缓冲。与 MAA-29 仅在 Close 后排空
// 不同，本文件的 m4tEventReader 在运行期持续消费公开缓冲（dispatcher 非
// 阻塞投递，及时消费不改变投递语义），使任务终态（AllTasksCompleted/
// TaskChainStopped 等）可在实例存活期间观察；Close 返回后读取器等待通道
// 关闭并交付全部事件（关闭顺序由 MAA-27 验收）。除该通道外不读取任何私
// 有状态。
//
// # 验收条件 → 用例映射（MAA-30）
//
//   - 「任务确实存在于锁定 Core/资源且非消耗性依据可复核；任务定义和参数
//     均在允许清单内，未执行真实培养或其他资源消耗动作」：本文件头的证据
//     记录 + 三个场景均先经 cfg.admitTask 精确准入（清单唯一任务
//     Depot、params 字面匹配；未列任务在任何原生调用前拒绝）+ 场景只提交
//     该任务、事件断言无许可范围之外的动作（AsyncCallInfo 恰
//     connect+screencap 两条）。
//   - 「入队、Start 受理和任务最终成功分别得到验证，没有混用 Operation
//     的完成语义」：AppendTask 返回非 0 TaskID（入队）→ Tasks() 含该 ID
//     → Start()=nil（受理，连同 Running()=true 的同步置位佐证）→
//     AllTasksCompleted 且 finished_tasks 含该 ID（最终成功）；Connect/
//     Screencap 的 Operation Wait 结果只用于连接/截图判定，从不充当任务
//     完成证据。
//   - 「TaskID、原始事件与任务终态可关联；未知字段、大整数和未知消息 ID
//     未被绑定层丢弃或改写」：TaskChainStart/Completed/AllTasksCompleted
//     的 taskid 与 AppendTask 返回的 TaskID 逐一相等、finished_tasks 精确
//     等于 [该 ID]；每条事件 JSON 完整可解析且含绑定层未建模的 uuid 字段
//     （未建模字段保留，值不打印——设备 uuid 属私有信息）；params 大整数
//     探针被 Core 接受；场景不假设事件集合封闭——未知 MessageID 只计数
//     不失败（绑定层逐字节保留由 dispatch_test.go 钉住）。
//   - 「同一未关闭实例可按场景执行多轮许可任务，不通过同进程销毁重建模
//     拟复用」：成功场景在同一实例上连续两轮任务，两轮之间不 Close、不
//     NewInstance；TaskID 递增且每轮终态独立关联。
//   - 「关闭与正常退出通过；故障/中断不被记为成功或已取消，未自动重试」：
//     每场景 Instance.Close/Runtime.Close 返回 nil、子进程退出 0 且 OK 标
//     记经父进程独立复验；停止场景断言被中断任务无成功终态消息；失败/
//     超时由 MAA-28 执行器归类为 failed/timeout-unknown（无 cancelled 分
//     类），单次运行不自动重试。
//
// # 隐私纪律
//
// 设备地址、ADB 路径与全部真实配置只存在于仓库外的显式配置文件；本文件
// 与 OK 标记只输出 ID、what、ret、计数与 PNG 字节数/尺寸等去敏元数据，不
// 打印原始事件 JSON（含设备 uuid）与 DepotInfo 的 data（物品数量属账号隐
// 私）；前置画面 PNG 只落盘 output_dir（仓库外，供执行者人工核对），绝不
// 进入任何提交；证据文件由写入器统一去敏并读回校验。
//
// # 复现命令（linux amd64；配置与输出必须在仓库外，配置格式见
// acceptance_device_linux_test.go 文件头；permissions 需同时授予
// connect/screencap 并登记 Depot 任务允许清单）
//
//	MAA_M4_DEVICE_CONFIG=/absolute/path/device-acceptance.json \
//	  CGO_ENABLED=0 go test ./packages/maa-go/ \
//	  -run 'TestM4DeviceDepot(Precondition)?(Stop)?Scenario' -count=1 -v
//
// 运行顺序（执行步骤 2 的前置画面核对）：
//
//  1. 先运行 TestM4DeviceDepotPreconditionScenario，人工查看 output_dir 中
//     的前置画面 PNG，确认设备处于游戏内画面（标题/登录/更新确认画面不
//     算）；
//  2. 确认后再运行 TestM4DeviceDepotScenario 与 TestM4DeviceDepotStopScenario。
package maa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// MAA-30 子进程模式标记:与 MAA-28 入口的 refuse-probe/smoke/hang/fail-exit
// 及 MAA-29 的 connect/screencap 场景互相独立,互不触发对方的守卫。
const (
	m4tModePrecondition = "depot-precondition-scenario"
	m4tModeSuccess      = "depot-success-scenario"
	m4tModeStop         = "depot-stop-scenario"
)

// 子进程 stdout 成功标记:仅在子进程全部断言通过后打印,父进程独立复验其
// 字段(MAA-28/29 同一纵深防御)。
const (
	m4tPreconditionOKMarker = "MAA_M4_DEPOT_PRECONDITION_OK"
	m4tSuccessOKMarker      = "MAA_M4_DEPOT_SUCCESS_OK"
	m4tStopOKMarker         = "MAA_M4_DEPOT_STOP_OK"
)

// 超时参数:任务场景子进程 15 分钟(两轮识别任务的导航/滑动/分析耗时依赖
// 设备与仓库内容,给足余量;死锁由外部超时可见化为 timeout-unknown,不算通
// 过);单轮任务的终态等待 10 分钟;停止收敛等待 5 分钟;Running 收敛与事
// 件轮询间隔 50ms。
const (
	m4tChildTimeout   = 15 * time.Minute
	m4tTaskWait       = 10 * time.Minute
	m4tStopWait       = 5 * time.Minute
	m4tPollInterval   = 50 * time.Millisecond
	m4tRunningGuard   = 2 * time.Minute
	m4tMaxPollNumbers = 4096 // 事件计数渲染防御上限,防止畸形事件撑爆诊断
)

// 前置画面 PNG 文件名(写入 output_dir,仓库外;固定文件名重跑覆盖)。
const (
	m4tPreconditionPngName        = "maa30-precondition-screen.png"
	m4tSuccessPreconditionPngName = "maa30-success-precondition.png"
	m4tStopPreconditionPngName    = "maa30-stop-precondition.png"
)

// 任务消息编号,冻结自锁定 Core commit d94dd007 的
// src/MaaCore/Common/AsstMsg.h(InternalError=0、InitFailed=1、
// ConnectionInfo=2、AllTasksCompleted=3、AsyncCallInfo=4、Destroyed=5、
// TaskChainError=10000、TaskChainStart=10001、TaskChainCompleted=10002、
// TaskChainExtraInfo=10003、TaskChainStopped=10004、SubTaskError=20000、
// SubTaskStart=20001、SubTaskCompleted=20002、SubTaskExtraInfo=20003、
// SubTaskStopped=20004、ReportRequest=30000);已与参考仓库 HEAD 逐文件核对
// 一致,并与上游文档 docs/en-us/protocol/callback-schema.md:40-41 的编号
// 互相印证。注意顺序:ExtraInfo 在 Stopped 之前(10003 是额外信息,10004 才
// 是手动停止),两者不可互换——停止场景的收敛只认 10004。
const (
	m4tAllTasksCompletedMessageID  = MessageID(3)
	m4tTaskChainErrorMessageID     = MessageID(10000)
	m4tTaskChainStartMessageID     = MessageID(10001)
	m4tTaskChainCompletedMessageID = MessageID(10002)
	m4tTaskChainStoppedMessageID   = MessageID(10004)
	m4tSubTaskExtraInfoMessageID   = MessageID(20003)
)

// m4tKnownMessageIDs 是 AsstMsg.h 冻结枚举的已知消息集合:场景不假设事件
// 集合封闭——集合之外的消息 ID 照常交付、只计数不失败(绑定层未知消息保
// 留的行为断言见 dispatch_test.go;本计数是场景层证据)。
var m4tKnownMessageIDs = map[MessageID]bool{
	0: true, 1: true, 2: true, 3: true, 4: true, 5: true,
	10000: true, 10001: true, 10002: true, 10003: true, 10004: true,
	20000: true, 20001: true, 20002: true, 20003: true, 20004: true,
	30000: true,
}

// 允许清单的 Go 侧登记字面形式,必须与显式配置 permissions.tasks 的条目
// 逐字面一致(m4SameJSONValue 按 json.Number 字面形式深度相等:配置里写成
// 9007199254740992 会被 admitTask 拒绝)。Depot 只读 "enable",
// maa30_probe_bigint 是 Core 保留不解释的大整数探针。
const (
	m4tTaskType          = "Depot"
	m4tTaskParamsLiteral = `{"enable":true,"maa30_probe_bigint":9007199254740993}`
)

// m4tTaskParams 返回允许清单 params 的独立副本:每次提交一份,杜绝场景代
// 码意外共享/修改同一底层数组(§6.1 快照纪律的测试侧对应)。
func m4tTaskParams() json.RawMessage {
	return json.RawMessage(m4tTaskParamsLiteral)
}

// m4tTaskMessage 是任务消息的必要关联字段(原始 JSON 完整保留在 Event.JSON;
// uuid 与其余未建模字段只验证存在、不解码不打印——设备 uuid 属私有信息)。
// taskid/finished_tasks 按整数解析,不经 float64 往返(标准库整数精确)。
type m4tTaskMessage struct {
	TaskChain     string  `json:"taskchain"`
	TaskID        int64   `json:"taskid"`
	FinishedTasks []int64 `json:"finished_tasks"`
	SubTask       string  `json:"subtask"`
	What          string  `json:"what"`
	Details       *struct {
		Done *bool   `json:"done"`
		Data *string `json:"data"`
	} `json:"details"`
	UUIDPresent bool `json:"-"`
}

// m4tParseMessage 校验事件 JSON 完整可解析(well-formed)并解析必要关联字
// 段;解析失败是明确失败(原始事件不得被静默忽略)。诊断只含消息编号/序
// 号,不含事件原文(原文含设备 uuid)。
func m4tParseMessage(t *testing.T, ev Event) m4tTaskMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.JSON, &raw); err != nil {
		t.Fatalf("事件 JSON 不完整 (msg=%d seq=%d): %v", ev.MessageID, ev.Sequence, err)
	}
	var msg m4tTaskMessage
	if err := json.Unmarshal(ev.JSON, &msg); err != nil {
		t.Fatalf("事件关联字段解析失败 (msg=%d seq=%d): %v", ev.MessageID, ev.Sequence, err)
	}
	_, msg.UUIDPresent = raw["uuid"]
	return msg
}

// m4tTaskEventCounts 是任务场景事件流的分类计数(去敏元数据,可进入证据)。
type m4tTaskEventCounts struct {
	AllTasksCompleted  int
	TaskChainStart     int
	TaskChainCompleted int
	TaskChainError     int
	TaskChainStopped   int
	DepotInfoDone      int
	UnknownMessageIDs  int
	WellFormedChecked  int
}

// m4tClassifyTaskEvents 对全部事件做 well-formed 断言与分类计数:每条事件
// 必须完整可解析且含未建模的 uuid 字段(未知字段保留的场景层证据);未知消
// 息 ID 只计数不失败。
func m4tClassifyTaskEvents(t *testing.T, evs []Event) m4tTaskEventCounts {
	t.Helper()
	var counts m4tTaskEventCounts
	for _, ev := range evs {
		msg := m4tParseMessage(t, ev)
		if !msg.UUIDPresent {
			t.Fatalf("事件缺少 uuid 字段 (msg=%d seq=%d): 绑定层不得丢弃未建模字段", ev.MessageID, ev.Sequence)
		}
		counts.WellFormedChecked++
		switch ev.MessageID {
		case m4tAllTasksCompletedMessageID:
			counts.AllTasksCompleted++
		case m4tTaskChainStartMessageID:
			counts.TaskChainStart++
		case m4tTaskChainCompletedMessageID:
			counts.TaskChainCompleted++
		case m4tTaskChainErrorMessageID:
			counts.TaskChainError++
		case m4tTaskChainStoppedMessageID:
			counts.TaskChainStopped++
		case m4tSubTaskExtraInfoMessageID:
			if msg.What == "DepotInfo" && msg.Details != nil && msg.Details.Done != nil && *msg.Details.Done {
				counts.DepotInfoDone++
			}
		}
		if !m4tKnownMessageIDs[ev.MessageID] {
			counts.UnknownMessageIDs++
		}
	}
	return counts
}

// m4tTaskEventsOf 筛选指定消息编号的事件。
func m4tTaskEventsOf(evs []Event, id MessageID) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.MessageID == id {
			out = append(out, ev)
		}
	}
	return out
}

// m4tRenderTaskEvents 渲染事件列表用于失败诊断(去敏:只含消息编号、序号、
// taskchain、taskid——不含原文、uuid 与 DepotInfo 的物品数据)。超过防御
// 上限时截断。
func m4tRenderTaskEvents(evs []Event) string {
	n := len(evs)
	if n > m4tMaxPollNumbers {
		evs = evs[:m4tMaxPollNumbers]
	}
	parts := make([]string, 0, len(evs))
	for _, ev := range evs {
		var m m4tTaskMessage
		if err := json.Unmarshal(ev.JSON, &m); err != nil {
			parts = append(parts, fmt.Sprintf("{msg=%d seq=%d <unparsed>}", ev.MessageID, ev.Sequence))
			continue
		}
		parts = append(parts, fmt.Sprintf("{msg=%d seq=%d taskchain=%s taskid=%d}", ev.MessageID, ev.Sequence, m.TaskChain, m.TaskID))
	}
	if n > m4tMaxPollNumbers {
		parts = append(parts, fmt.Sprintf("(+%d more)", n-m4tMaxPollNumbers))
	}
	return strings.Join(parts, " ")
}

// m4tEventReader 是任务场景的运行期事件读取器:后台 goroutine 持续消费公
// 开 Events 缓冲(dispatcher 非阻塞投递,及时消费不改变投递语义;公开通道
// 只由 dispatcher 发送与关闭)。Close 返回前通道保持开放,waitFor 在实例存
// 活期间按序观察任务终态;Close 返回后通道已关闭,finalEvents 交付全部事
// 件。除该公开缓冲外不读取任何私有状态。
type m4tEventReader struct {
	ch     <-chan Event
	mu     sync.Mutex
	evs    []Event
	cursor int
	closed chan struct{}
}

// m4tStartEventReader 启动读取器:读取器从公开缓冲按序取得全部已投递未消
// 费的事件——dispatcher 先结算 Operation 再非阻塞投递(§7.1),因此即使在
// 连接/截图完成之后启动,其完成事件也仍在缓冲中按序可读,收集始终完整;
// 任务事件全部发生在启动之后。
func m4tStartEventReader(t *testing.T, inst *Instance) *m4tEventReader {
	t.Helper()
	r := &m4tEventReader{ch: inst.inst.disp.events(), closed: make(chan struct{})}
	go func() {
		defer close(r.closed)
		for ev := range r.ch {
			r.mu.Lock()
			r.evs = append(r.evs, ev)
			r.mu.Unlock()
		}
	}()
	return r
}

// waitFor 按序扫描尚未消费的事件并等待下一条匹配事件:任务终态在实例存活
// 期间的观察手段。超时是本场景的明确失败,不重试;诊断含已见事件的去敏渲
// 染。不匹配的事件推进游标后继续等待(事件仍保留在 evs 中供终局断言)。
func (r *m4tEventReader) waitFor(t *testing.T, timeout time.Duration, what string, pred func(Event) bool) Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		for r.cursor < len(r.evs) {
			ev := r.evs[r.cursor]
			r.cursor++
			if pred(ev) {
				r.mu.Unlock()
				return ev
			}
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时 (%s): 已见 %d 条事件仍无匹配 (事件: %s)",
				what, timeout, r.count(), m4tRenderTaskEvents(r.snapshot()))
		}
		time.Sleep(m4tPollInterval)
	}
}

// count 返回已见事件总数。
func (r *m4tEventReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.evs)
}

// snapshot 返回全部已见事件的副本(Event.JSON 为不可变原文,共享底层数组
// 与 MAA-27 的排空读取同一约定)。
func (r *m4tEventReader) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.evs))
	copy(out, r.evs)
	return out
}

// waitClosed 等待源通道关闭:关闭流程保证 Close 返回前公开通道已由
// dispatcher 关闭(MAA-27 已验收该顺序);防挂死守卫兜底为明确失败,不作同
// 步手段。
func (r *m4tEventReader) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(m3DrainGuardTimeout):
		t.Fatalf("Close 返回后 %s 内公开 Events 通道仍未关闭 (已见 %d 条), 关闭契约被破坏",
			m3DrainGuardTimeout, r.count())
	}
}

// finalEvents 在 Close 之后交付全部事件(含关闭过程产生的 Destroyed)。
func (r *m4tEventReader) finalEvents(t *testing.T) []Event {
	t.Helper()
	r.waitClosed(t)
	return r.snapshot()
}

// m4tPrepareDevice 完成三个场景共用的前置段:初始化锁定 Core/资源快照、
// 显式连接配置指定的设备并核对连接查询语义;不执行任何任务动作。
func m4tPrepareDevice(t *testing.T, cfg *deviceAcceptanceConfig) (*Runtime, *Instance, AsyncResult) {
	t.Helper()
	rt, inst := m4OpenLockedSnapshot(t, cfg)
	m4RequireNotConnected(t, inst)
	res := m4ConnectDevice(t, inst, cfg)
	if connected, err := inst.Connected(); err != nil || !connected {
		t.Fatalf("连接成功后 Connected() = (%v, %v), want (true, nil)", connected, err)
	}
	return rt, inst, res
}

// m4tScreencapPrecondition 提交一次截图并等待成功,把缓存 PNG 落盘
// output_dir:这是任务场景的前置画面核对证据(任务文件执行步骤 2),由执行
// 者人工核对——场景不解析画面内容、不自动导航。PNG 是设备屏幕内容,只允
// 许存在于仓库外 output_dir,绝不进入提交;诊断只输出字节数与尺寸。返回
// 截图结果与 PNG 元数据。
func m4tScreencapPrecondition(t *testing.T, inst *Instance, cfg *deviceAcceptanceConfig, name string) (AsyncResult, int, int, int) {
	t.Helper()
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
		t.Fatalf("前置截图的最终 Wait 失败 (提交 id=%d): %v", op.ID(), err)
	}
	if res.ID != op.ID() || res.What != asyncWhatScreencap || !res.OK {
		t.Fatalf("截图结果关联不符: id=%d what=%q ok=%v, want id=%d what=%q ok=true",
			res.ID, res.What, res.OK, op.ID(), asyncWhatScreencap)
	}
	m4RequireDetailsRetTrue(t, res)

	data, err := inst.GetImagePNG()
	if err != nil {
		t.Fatalf("Screencap 成功后 GetImagePNG 失败: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("Screencap 成功后 GetImagePNG = 0 字节, want 合法 PNG")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("前置画面 PNG 解码失败 (%d 字节): %v", len(data), err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("前置画面 PNG 尺寸非法: %dx%d", b.Dx(), b.Dy())
	}
	path := filepath.Join(cfg.OutputDir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("前置画面 PNG 写入 %s 失败: %v", path, err)
	}
	t.Logf("前置画面 PNG 已写入 %s (%d 字节, %dx%d)——由执行者人工核对设备画面;场景不解析画面内容、不自动导航", path, len(data), b.Dx(), b.Dy())
	return res, len(data), b.Dx(), b.Dy()
}

// m4tWaitRunningFalse 轮询等待 Running() 收敛为 false(任务队列空闲;正常
// false 不转换成错误,错误即失败)。AllTasksCompleted 到达时 working_proc
// 可能尚未回到空闲分支(m_running 置 false 在循环顶),收敛通常在数百毫秒
// 内;超时是明确失败。
func m4tWaitRunningFalse(t *testing.T, inst *Instance, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		running, err := inst.Running()
		if err != nil {
			t.Fatalf("Running() error: %v (false 是正常查询结果,生命周期错误不得与正常 false 混淆)", err)
		}
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务队列 %s 内未收敛为 Running=false: 状态未收敛", timeout)
		}
		time.Sleep(m4tPollInterval)
	}
}

// m4tInt64SliceEquals 判定 finished_tasks 与期望序列精确相等(顺序与内容
// 都精确;空与非空不等)。
func m4tInt64SliceEquals(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// m4tAppendStartAndComplete 执行一轮许可任务的完整生命周期(执行步骤 2–3;
// 成功场景调用两轮):
//  1. AppendTask 返回非 0 TaskID——入队成功;入队不等于开始或完成;
//  2. Tasks() 含新 TaskID——入队与任务列表一致(get_tasks_list 返回当前队
//     列,任务开始后逐个弹出,必须在 Start 前核对);
//  3. Start 返回 nil——启动请求被 Core 受理;连同 Running()=true 的同步置
//     位佐证;受理不当作任务成功;
//  4. 等待任务终态:AllTasksCompleted(3) 的 taskid 与本轮 TaskID 相等且
//     finished_tasks 精确等于 [本轮 ID],TaskChainCompleted(10002) 佐证
//     (taskchain=Depot、taskid 一致)——任务完成只认任务消息,与 Connect/
//     Screencap 的 Operation 完成语义严格区分;
//  5. Running() 收敛 false(本轮队列排空)。
func m4tAppendStartAndComplete(t *testing.T, inst *Instance, reader *m4tEventReader, round int) TaskID {
	t.Helper()
	id, err := inst.AppendTask(m4tTaskType, m4tTaskParams())
	if err != nil || id == 0 {
		t.Fatalf("第 %d 轮 AppendTask = (%d, %v): 入队失败或返回 0", round, id, err)
	}
	tasks, err := inst.Tasks()
	if err != nil {
		t.Fatalf("第 %d 轮 Tasks() error: %v", round, err)
	}
	found := false
	for _, got := range tasks {
		if got == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("第 %d 轮 Tasks() 不含新入队 TaskID %d (len=%d): 入队与任务列表不一致", round, id, len(tasks))
	}
	if err := inst.Start(); err != nil {
		t.Fatalf("第 %d 轮 Start = %v, want nil (C bool=true 受理)", round, err)
	}
	if running, err := inst.Running(); err != nil || !running {
		t.Fatalf("第 %d 轮 Start 受理后 Running() = (%v, %v), want (true, nil)", round, running, err)
	}

	all := reader.waitFor(t, m4tTaskWait, fmt.Sprintf("第 %d 轮 AllTasksCompleted (taskid=%d)", round, id), func(ev Event) bool {
		if ev.MessageID != m4tAllTasksCompletedMessageID {
			return false
		}
		var m m4tTaskMessage
		return json.Unmarshal(ev.JSON, &m) == nil && m.TaskID == int64(id)
	})
	msg := m4tParseMessage(t, all)
	if !m4tInt64SliceEquals(msg.FinishedTasks, []int64{int64(id)}) {
		t.Fatalf("第 %d 轮 AllTasksCompleted.finished_tasks = %v, want [%d] (TaskID 与任务终态必须可关联)", round, msg.FinishedTasks, id)
	}
	comp := reader.waitFor(t, m4tTaskWait, fmt.Sprintf("第 %d 轮 TaskChainCompleted (taskid=%d)", round, id), func(ev Event) bool {
		if ev.MessageID != m4tTaskChainCompletedMessageID {
			return false
		}
		var m m4tTaskMessage
		return json.Unmarshal(ev.JSON, &m) == nil && m.TaskID == int64(id)
	})
	cmsg := m4tParseMessage(t, comp)
	if cmsg.TaskChain != m4tTaskType {
		t.Fatalf("第 %d 轮 TaskChainCompleted.taskchain = %q, want %q", round, cmsg.TaskChain, m4tTaskType)
	}
	m4tWaitRunningFalse(t, inst, m4tRunningGuard)
	return id
}

// m4tRequireTaskOutcome 是成功场景的终局事件断言(验收条件 2/3/4;执行步骤
// 5):两轮任务的全部终态消息可关联且无矛盾证据——AllTasksCompleted 恰两
// 条(finished_tasks 精确 [id1]/[id2]、taskid 对应),TaskChainStart/
// Completed 各恰两条且 taskid/taskchain 对应,TaskChainError=0(任务链失
// 败不得混入成功场景),done=true 的 DepotInfo 恰两条(每轮识别成功一条);
// AsyncCallInfo 的封闭计数由 m4RequireConsistentCompletion 单独断言。返回
// 分类计数供 OK 标记记录。
func m4tRequireTaskOutcome(t *testing.T, evs []Event, ids []TaskID) m4tTaskEventCounts {
	t.Helper()
	counts := m4tClassifyTaskEvents(t, evs)
	if counts.AllTasksCompleted != len(ids) {
		t.Fatalf("AllTasksCompleted = %d 条 (%s), want 恰 %d 条 (每轮任务恰好一次终态)",
			counts.AllTasksCompleted, m4tRenderTaskEvents(evs), len(ids))
	}
	completions := m4tTaskEventsOf(evs, m4tAllTasksCompletedMessageID)
	for i, want := range ids {
		msg := m4tParseMessage(t, completions[i])
		if msg.TaskID != int64(want) || !m4tInt64SliceEquals(msg.FinishedTasks, []int64{int64(want)}) {
			t.Fatalf("第 %d 条 AllTasksCompleted 关联不符: taskid=%d finished_tasks=%v, want taskid=%d finished_tasks=[%d]",
				i+1, msg.TaskID, msg.FinishedTasks, want, want)
		}
	}
	if counts.TaskChainStart != len(ids) || counts.TaskChainCompleted != len(ids) {
		t.Fatalf("TaskChainStart=%d TaskChainCompleted=%d (%s), want 各恰 %d 条",
			counts.TaskChainStart, counts.TaskChainCompleted, m4tRenderTaskEvents(evs), len(ids))
	}
	for _, mid := range []MessageID{m4tTaskChainStartMessageID, m4tTaskChainCompletedMessageID} {
		evsOf := m4tTaskEventsOf(evs, mid)
		for i, want := range ids {
			msg := m4tParseMessage(t, evsOf[i])
			if msg.TaskID != int64(want) || msg.TaskChain != m4tTaskType {
				t.Fatalf("第 %d 条消息 (msg=%d) 关联不符: taskchain=%q taskid=%d, want taskchain=%q taskid=%d",
					i+1, mid, msg.TaskChain, msg.TaskID, m4tTaskType, want)
			}
		}
	}
	if counts.TaskChainError != 0 {
		t.Fatalf("成功场景出现 TaskChainError=%d 条 (%s): 任务链失败不得混入成功场景",
			counts.TaskChainError, m4tRenderTaskEvents(evs))
	}
	if counts.DepotInfoDone != len(ids) {
		t.Fatalf("done=true 的 DepotInfo = %d 条, want 恰 %d 条 (每轮识别成功一条)",
			counts.DepotInfoDone, len(ids))
	}
	return counts
}

// m4tRequireStoppedOutcome 是停止场景的终局事件断言(执行步骤 4、验收条件
// 5):被中断的任务不结算为成功——无 AllTasksCompleted/TaskChainCompleted
// /TaskChainError/done=true 识别结果,停止以 TaskChainStopped 收敛
// (taskchain=Depot;stop 清空队列后 working_proc 不再发 AllTasksCompleted,
// Assistant.cpp:685-698)。返回分类计数供 OK 标记记录。
func m4tRequireStoppedOutcome(t *testing.T, evs []Event, id TaskID) m4tTaskEventCounts {
	t.Helper()
	counts := m4tClassifyTaskEvents(t, evs)
	if counts.TaskChainStart != 1 {
		t.Fatalf("TaskChainStart = %d 条 (%s), want 恰 1 条 (停止前任务链必须已开始)",
			counts.TaskChainStart, m4tRenderTaskEvents(evs))
	}
	start := m4tParseMessage(t, m4tTaskEventsOf(evs, m4tTaskChainStartMessageID)[0])
	if start.TaskID != int64(id) || start.TaskChain != m4tTaskType {
		t.Fatalf("TaskChainStart 关联不符: taskchain=%q taskid=%d, want taskchain=%q taskid=%d",
			start.TaskChain, start.TaskID, m4tTaskType, id)
	}
	if counts.TaskChainStopped < 1 {
		t.Fatalf("Stop 受理后 %d 条 TaskChainStopped (%s): 停止状态未收敛", counts.TaskChainStopped, m4tRenderTaskEvents(evs))
	}
	for _, ev := range m4tTaskEventsOf(evs, m4tTaskChainStoppedMessageID) {
		msg := m4tParseMessage(t, ev)
		if msg.TaskChain != m4tTaskType {
			t.Fatalf("TaskChainStopped.taskchain = %q, want %q", msg.TaskChain, m4tTaskType)
		}
	}
	if counts.AllTasksCompleted != 0 {
		t.Fatalf("被中断的任务产生 %d 条 AllTasksCompleted: 中断不得记为成功", counts.AllTasksCompleted)
	}
	if counts.TaskChainCompleted != 0 {
		t.Fatalf("被中断的任务产生 %d 条 TaskChainCompleted: 中断不得记为成功", counts.TaskChainCompleted)
	}
	if counts.TaskChainError != 0 {
		t.Fatalf("停止场景出现 %d 条 TaskChainError (%s): 停止语义不得与任务链失败混淆",
			counts.TaskChainError, m4tRenderTaskEvents(evs))
	}
	if counts.DepotInfoDone != 0 {
		t.Fatalf("被中断的任务产生 %d 条 done=true 的 DepotInfo: 中断不得记为识别成功", counts.DepotInfoDone)
	}
	return counts
}

// runM4tPreconditionChild 是前置画面核对场景子进程入口(执行步骤 2 的前置
// 核对):连接设备、截图一次并把 PNG 落盘 output_dir 供执行者人工核对;不
// 执行任何任务动作(无 AppendTask/Start/Stop)。断言失败即非 0 退出。
func runM4tPreconditionChild(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("前置画面核对场景需要连接设备,连接未获许可: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("前置画面核对场景需要截图许可: %v", err)
	}
	rt, inst, connRes := m4tPrepareDevice(t, cfg)
	capRes, n, w, h := m4tScreencapPrecondition(t, inst, cfg, m4tPreconditionPngName)

	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (正常关闭无死锁)", err)
	}
	evs := readM3DrainedEvents(t, inst)
	m4RequireConsistentCompletion(t, evs, map[AsyncID]string{
		connRes.ID: asyncWhatConnect,
		capRes.ID:  asyncWhatScreencap,
	})
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("Runtime 终止后的 NewInstance 错误 = %v, want errors.Is ErrRuntimeTerminated", err)
	}
	fmt.Printf("%s version=%s connect_id=%d screencap_id=%d png_bytes=%d png_w=%d png_h=%d precondition_png=%s async_call_events=2\n",
		m4tPreconditionOKMarker, rt.Version(), connRes.ID, capRes.ID, n, w, h, m4tPreconditionPngName)
}

// runM4tSuccessChild 是成功场景子进程入口(执行步骤 2–5):同一未关闭实例连
// 续两轮许可任务(入队/列表核对/Start 受理/终态关联),空闲 Stop 受理与收
// 敛,关闭与正常退出;多轮复用不经过任何 Close/NewInstance。
func runM4tSuccessChild(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("任务场景需要先连接设备,连接未获许可: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("任务场景需要截图许可(前置画面核对): %v", err)
	}
	rt, inst, connRes := m4tPrepareDevice(t, cfg)
	capRes, n, w, h := m4tScreencapPrecondition(t, inst, cfg, m4tSuccessPreconditionPngName)

	// 运行期事件读取器在第一个原生动作之前启动(连接/截图事件也经它收集)。
	reader := m4tStartEventReader(t, inst)

	// 允许清单准入在任何原生任务调用之前(执行步骤 2;未列任务拒绝)。
	if err := cfg.admitTask(m4tTaskType, m4tTaskParams()); err != nil {
		t.Fatalf("任务未在允许清单内,拒绝入队: %v", err)
	}
	id1 := m4tAppendStartAndComplete(t, inst, reader, 1)
	id2 := m4tAppendStartAndComplete(t, inst, reader, 2)
	if id1 == id2 {
		t.Fatalf("两轮任务的 TaskID 相同 (%d): TaskID 必须递增且每轮独立登记", id1)
	}

	// 空闲 Stop(执行步骤 4 的受理语义):队列已排空后请求停止——受理(nil)
	// 且状态收敛于已停止;不再有任务终态消息需要等待。
	if err := inst.Stop(); err != nil {
		t.Fatalf("空闲 Stop = %v, want nil (C bool=true 受理)", err)
	}
	m4tWaitRunningFalse(t, inst, m4tStopWait)

	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (正常关闭无死锁)", err)
	}
	evs := reader.finalEvents(t)
	m4RequireConsistentCompletion(t, evs, map[AsyncID]string{
		connRes.ID: asyncWhatConnect,
		capRes.ID:  asyncWhatScreencap,
	})
	counts := m4tRequireTaskOutcome(t, evs, []TaskID{id1, id2})

	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("Runtime 终止后的 NewInstance 错误 = %v, want errors.Is ErrRuntimeTerminated", err)
	}
	fmt.Printf("%s version=%s connect_id=%d screencap_id=%d task_id_1=%d task_id_2=%d tasks_query=true start_accepted=true all_tasks_completed=%d task_chain_start=%d task_chain_completed=%d task_chain_error=%d task_chain_stopped_idle=%d depot_info_done=%d async_call_events=2 unknown_messages=%d events_total=%d png_bytes=%d png_w=%d png_h=%d running_after_idle_stop=false\n",
		m4tSuccessOKMarker, rt.Version(), connRes.ID, capRes.ID, id1, id2,
		counts.AllTasksCompleted, counts.TaskChainStart, counts.TaskChainCompleted,
		counts.TaskChainError, counts.TaskChainStopped, counts.DepotInfoDone,
		counts.UnknownMessageIDs, len(evs), n, w, h)
}

// runM4tStopChild 是停止场景子进程入口(执行步骤 4、验收条件 5):任务链开
// 始后按场景约定请求一次 Stop,核对受理(nil)与收敛(TaskChainStopped +
// Running=false),断言被中断的任务不产生成功终态;之后关闭并正常退出。本
// 场景不重试、不重播原任务(中断的任务按任务文件记录为结果未知,由父侧失
// 败分类与超时分类承载)。
func runM4tStopChild(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("停止场景需要先连接设备,连接未获许可: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("停止场景需要截图许可(前置画面核对): %v", err)
	}
	rt, inst, connRes := m4tPrepareDevice(t, cfg)
	capRes, n, w, h := m4tScreencapPrecondition(t, inst, cfg, m4tStopPreconditionPngName)
	reader := m4tStartEventReader(t, inst)
	if err := cfg.admitTask(m4tTaskType, m4tTaskParams()); err != nil {
		t.Fatalf("任务未在允许清单内,拒绝入队: %v", err)
	}

	id, err := inst.AppendTask(m4tTaskType, m4tTaskParams())
	if err != nil || id == 0 {
		t.Fatalf("AppendTask = (%d, %v): 入队失败或返回 0", id, err)
	}
	tasks, err := inst.Tasks()
	if err != nil {
		t.Fatalf("Tasks() error: %v", err)
	}
	found := false
	for _, got := range tasks {
		if got == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Tasks() 不含新入队 TaskID %d (len=%d)", id, len(tasks))
	}
	if err := inst.Start(); err != nil {
		t.Fatalf("Start = %v, want nil (C bool=true 受理)", err)
	}
	if running, err := inst.Running(); err != nil || !running {
		t.Fatalf("Start 受理后 Running() = (%v, %v), want (true, nil)", running, err)
	}

	// 等任务链开始执行(证据:TaskChainStart 且 taskid 关联),随后按场景约定
	// 请求一次 Stop(执行步骤 4:仅按场景约定;无重试)。
	reader.waitFor(t, m4tTaskWait, fmt.Sprintf("TaskChainStart (taskid=%d)", id), func(ev Event) bool {
		if ev.MessageID != m4tTaskChainStartMessageID {
			return false
		}
		var m m4tTaskMessage
		return json.Unmarshal(ev.JSON, &m) == nil && m.TaskID == int64(id)
	})
	if err := inst.Stop(); err != nil {
		t.Fatalf("Stop = %v, want nil (C bool=true 受理)", err)
	}
	// 收敛双证:TaskChainStopped(taskchain=Depot) + Running=false。
	reader.waitFor(t, m4tStopWait, "TaskChainStopped", func(ev Event) bool {
		if ev.MessageID != m4tTaskChainStoppedMessageID {
			return false
		}
		var m m4tTaskMessage
		return json.Unmarshal(ev.JSON, &m) == nil && m.TaskChain == m4tTaskType
	})
	m4tWaitRunningFalse(t, inst, m4tStopWait)

	if err := inst.Close(); err != nil {
		t.Fatalf("Instance.Close = %v, want nil (正常关闭无死锁)", err)
	}
	evs := reader.finalEvents(t)
	m4RequireConsistentCompletion(t, evs, map[AsyncID]string{
		connRes.ID: asyncWhatConnect,
		capRes.ID:  asyncWhatScreencap,
	})
	counts := m4tRequireStoppedOutcome(t, evs, id)

	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close = %v, want nil", err)
	}
	if _, err := rt.NewInstance(InstanceConfig{}); !errors.Is(err, ErrRuntimeTerminated) {
		t.Fatalf("Runtime 终止后的 NewInstance 错误 = %v, want errors.Is ErrRuntimeTerminated", err)
	}
	fmt.Printf("%s version=%s connect_id=%d screencap_id=%d task_id=%d tasks_query=true start_accepted=true stop_accepted=true running_after_stop=false task_chain_start=%d task_chain_completed=%d all_tasks_completed=%d task_chain_error=%d task_chain_stopped=%d async_call_events=2 unknown_messages=%d events_total=%d png_bytes=%d png_w=%d png_h=%d\n",
		m4tStopOKMarker, rt.Version(), connRes.ID, capRes.ID, id,
		counts.TaskChainStart, counts.TaskChainCompleted, counts.AllTasksCompleted,
		counts.TaskChainError, counts.TaskChainStopped,
		counts.UnknownMessageIDs, len(evs), n, w, h)
}

// TestM4DeviceDepotPreconditionScenario 前置画面核对场景(执行步骤 2 的前
// 置核对):仅在显式配置 + connect/screencap 许可下,经独立子进程连接设备、
// 截图并把 PNG 落盘 output_dir;父进程复验 OK 标记并写入去敏证据。执行者
// 必须人工核对该 PNG 确认设备处于游戏内画面后,才运行任务场景。
func TestM4DeviceDepotPreconditionScenario(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4tModePrecondition {
		runM4tPreconditionChild(t)
		return
	}
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("前置画面核对场景需要连接设备,拒绝执行: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("前置画面核对场景需要截图许可,拒绝执行: %v", err)
	}
	out := runM4DeviceScenario(t, m4tChildTimeout, m4tModePrecondition, map[string]string{
		m4ConfigEnv: os.Getenv(m4ConfigEnv),
	})
	if out.err != nil {
		t.Fatalf("depot-precondition-scenario 子进程失败(非零退出/信号/超时一律视为失败,不重试、不重放设备动作):\n%s",
			m4RenderScenarioDiagnostics(m4tModePrecondition, out))
	}
	fields, err := verifyM4tPreconditionMetadata(out.stdout)
	if err != nil {
		t.Fatalf("depot-precondition-scenario 子进程 stdout 元数据复验失败(与子进程内断言独立):\n%v\n%s",
			err, m4RenderScenarioDiagnostics(m4tModePrecondition, out))
	}
	m4WriteRedactedEvidence(t, cfg, m4tModePrecondition, m4tEvidenceFields(fields))
}

// TestM4DeviceDepotScenario 成功场景(执行步骤 2–3、5):仅在显式配置 +
// connect/screencap 许可 + Depot 允许清单下,经独立子进程在同一未关闭实例
// 上连续执行两轮 Depot 任务并核对终态关联;父进程复验 OK 标记并写入去敏
// 证据。任务场景的前置画面由执行者经 precondition 场景人工核对。
func TestM4DeviceDepotScenario(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4tModeSuccess {
		runM4tSuccessChild(t)
		return
	}
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("任务场景需要先连接设备,连接未获许可,拒绝执行: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("任务场景需要截图许可(前置画面核对),拒绝执行: %v", err)
	}
	if err := cfg.admitTask(m4tTaskType, m4tTaskParams()); err != nil {
		t.Fatalf("Depot 任务未在允许清单内,拒绝执行任何原生任务调用: %v", err)
	}
	out := runM4DeviceScenario(t, m4tChildTimeout, m4tModeSuccess, map[string]string{
		m4ConfigEnv: os.Getenv(m4ConfigEnv),
	})
	if out.err != nil {
		t.Fatalf("depot-success-scenario 子进程失败(非零退出/信号/超时一律视为失败,不重试、不重放设备动作):\n%s",
			m4RenderScenarioDiagnostics(m4tModeSuccess, out))
	}
	fields, err := verifyM4tSuccessMetadata(out.stdout)
	if err != nil {
		t.Fatalf("depot-success-scenario 子进程 stdout 元数据复验失败(与子进程内断言独立):\n%v\n%s",
			err, m4RenderScenarioDiagnostics(m4tModeSuccess, out))
	}
	m4WriteRedactedEvidence(t, cfg, m4tModeSuccess, m4tEvidenceFields(fields))
}

// TestM4DeviceDepotStopScenario 停止场景(执行步骤 4、5):仅在显式配置 +
// connect/screencap 许可 + Depot 允许清单下,经独立子进程在任务链开始后请
// 求一次 Stop,核对受理与收敛,断言中断不被记为成功;父进程复验 OK 标记并
// 写入去敏证据。
func TestM4DeviceDepotStopScenario(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4tModeStop {
		runM4tStopChild(t)
		return
	}
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("停止场景需要先连接设备,连接未获许可,拒绝执行: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("停止场景需要截图许可(前置画面核对),拒绝执行: %v", err)
	}
	if err := cfg.admitTask(m4tTaskType, m4tTaskParams()); err != nil {
		t.Fatalf("Depot 任务未在允许清单内,拒绝执行任何原生任务调用: %v", err)
	}
	out := runM4DeviceScenario(t, m4tChildTimeout, m4tModeStop, map[string]string{
		m4ConfigEnv: os.Getenv(m4ConfigEnv),
	})
	if out.err != nil {
		t.Fatalf("depot-stop-scenario 子进程失败(非零退出/信号/超时一律视为失败,不重试、不重放设备动作):\n%s",
			m4RenderScenarioDiagnostics(m4tModeStop, out))
	}
	fields, err := verifyM4tStopMetadata(out.stdout)
	if err != nil {
		t.Fatalf("depot-stop-scenario 子进程 stdout 元数据复验失败(与子进程内断言独立):\n%v\n%s",
			err, m4RenderScenarioDiagnostics(m4tModeStop, out))
	}
	m4WriteRedactedEvidence(t, cfg, m4tModeStop, m4tEvidenceFields(fields))
}

// m4tRequireIDs 校验 OK 标记字段中的 ID 均为非零整数且互不相等(至少两个
// 时)。
func m4tRequireIDs(fields map[string]string, keys ...string) error {
	seen := make(map[int]bool, len(keys))
	for _, k := range keys {
		id, err := strconv.Atoi(fields[k])
		if err != nil || id == 0 {
			return fmt.Errorf("OK 标记字段 %s = %q, want 非零整数", k, fields[k])
		}
		if seen[id] {
			return fmt.Errorf("OK 标记字段 %s = %q 与另一 ID 重复: TaskID/AsyncID 必须逐任务独立", k, fields[k])
		}
		seen[id] = true
	}
	return nil
}

// m4tRequireCounts 校验 OK 标记字段中的整数字段(可解析即可,供证据记录;
// 固定期望值由 verify 函数的 want 表校验)。
func m4tRequireCounts(fields map[string]string, keys ...string) error {
	for _, k := range keys {
		if _, err := strconv.Atoi(fields[k]); err != nil {
			return fmt.Errorf("OK 标记字段 %s = %q, want 整数", k, fields[k])
		}
	}
	return nil
}

// verifyM4tPreconditionMetadata 父侧复验前置画面核对场景 OK 标记:固定字
// 段逐一核对、ID 非零、PNG 元数据为正、连接与截图完成事件恰两条。
func verifyM4tPreconditionMetadata(stdout string) (map[string]string, error) {
	fields, err := m4ParseMarkerFields(stdout, m4tPreconditionOKMarker)
	if err != nil {
		return nil, err
	}
	want := map[string]string{
		"version":           lockedCoreVersion,
		"precondition_png":  m4tPreconditionPngName,
		"async_call_events": "2",
	}
	for k, w := range want {
		if got := fields[k]; got != w {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want %q", k, got, w)
		}
	}
	if err := m4tRequireIDs(fields, "connect_id", "screencap_id"); err != nil {
		return fields, err
	}
	for _, k := range []string{"png_bytes", "png_w", "png_h"} {
		if n, err := strconv.Atoi(fields[k]); err != nil || n <= 0 {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want 正整数", k, fields[k])
		}
	}
	return fields, nil
}

// verifyM4tSuccessMetadata 父侧复验成功场景 OK 标记:固定字段(两轮终态计
// 数、零错误、零未知消息容忍记录)、四个 ID 非零且互异、PNG 元数据为正。
func verifyM4tSuccessMetadata(stdout string) (map[string]string, error) {
	fields, err := m4ParseMarkerFields(stdout, m4tSuccessOKMarker)
	if err != nil {
		return nil, err
	}
	want := map[string]string{
		"version":                 lockedCoreVersion,
		"tasks_query":             "true",
		"start_accepted":          "true",
		"all_tasks_completed":     "2",
		"task_chain_start":        "2",
		"task_chain_completed":    "2",
		"task_chain_error":        "0",
		"depot_info_done":         "2",
		"async_call_events":       "2",
		"running_after_idle_stop": "false",
	}
	for k, w := range want {
		if got := fields[k]; got != w {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want %q", k, got, w)
		}
	}
	if err := m4tRequireIDs(fields, "connect_id", "screencap_id", "task_id_1", "task_id_2"); err != nil {
		return fields, err
	}
	if err := m4tRequireCounts(fields, "unknown_messages", "events_total", "task_chain_stopped_idle"); err != nil {
		return fields, err
	}
	for _, k := range []string{"png_bytes", "png_w", "png_h"} {
		if n, err := strconv.Atoi(fields[k]); err != nil || n <= 0 {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want 正整数", k, fields[k])
		}
	}
	return fields, nil
}

// verifyM4tStopMetadata 父侧复验停止场景 OK 标记:固定字段(受理/收敛/无成
// 功终态/停止消息至少一条)、ID 非零、PNG 元数据为正。
func verifyM4tStopMetadata(stdout string) (map[string]string, error) {
	fields, err := m4ParseMarkerFields(stdout, m4tStopOKMarker)
	if err != nil {
		return nil, err
	}
	want := map[string]string{
		"version":              lockedCoreVersion,
		"tasks_query":          "true",
		"start_accepted":       "true",
		"stop_accepted":        "true",
		"running_after_stop":   "false",
		"task_chain_start":     "1",
		"task_chain_completed": "0",
		"all_tasks_completed":  "0",
		"task_chain_error":     "0",
		"async_call_events":    "2",
	}
	for k, w := range want {
		if got := fields[k]; got != w {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want %q", k, got, w)
		}
	}
	if err := m4tRequireIDs(fields, "connect_id", "screencap_id", "task_id"); err != nil {
		return fields, err
	}
	if stopped, err := strconv.Atoi(fields["task_chain_stopped"]); err != nil || stopped < 1 {
		return fields, fmt.Errorf("OK 标记字段 task_chain_stopped = %q, want >=1 (停止必须以 TaskChainStopped 收敛)", fields["task_chain_stopped"])
	}
	if err := m4tRequireCounts(fields, "unknown_messages", "events_total"); err != nil {
		return fields, err
	}
	for _, k := range []string{"png_bytes", "png_w", "png_h"} {
		if n, err := strconv.Atoi(fields[k]); err != nil || n <= 0 {
			return fields, fmt.Errorf("OK 标记字段 %s = %q, want 正整数", k, fields[k])
		}
	}
	return fields, nil
}

// m4tEvidenceFields 在 MAA-29 的证据字段(go/os_arch/core_version/
// core_snapshot/png_cache_note)之上补充 MAA-30 的任务审定元数据:任务类型
// 与允许清单字面形式、非消耗性依据、大整数探针、前置画面核对方式与终态
// 判定依据(全部为去敏元数据,不含设备地址、事件原文与物品数据)。
func m4tEvidenceFields(marker map[string]string) map[string]string {
	fields := m4EvidenceFields(marker)
	fields["task_type"] = m4tTaskType
	fields["task_params_literal"] = m4tTaskParamsLiteral
	fields["task_allowance_note"] = "允许清单仅登记 Depot;admitTask 在任何原生任务调用前拒绝未列任务"
	fields["bigint_probe"] = "params 含未知字段 maa30_probe_bigint=9007199254740993(>2^53):允许清单按 json.Number 字面形式精确匹配,Core 接受并完成任务;逐字节透传白箱断言见 instance_sync_test.go(MAA-22)"
	fields["non_consumption_basis"] = "DepotTask=DepotBegin(navigate)+DepotRecognitionTask(recognize);device actions=get_image/swipe(DepotSlowlySwipeToTheRight)/click-all-tab(DepotAllTab) only;no resource-consuming action;locked commit d94dd007: Assistant.cpp:337, DepotTask.cpp:7-18, DepotRecognitionTask.cpp"
	fields["precondition_note"] = "前置画面 PNG 落盘 output_dir 由执行者人工核对;场景不解析画面内容、不自动导航"
	fields["terminal_state_basis"] = "success=AllTasksCompleted(3).finished_tasks+TaskChainCompleted(10002)+DepotInfo(20003).done=true;stop=TaskChainStopped(10004);AsstMsg IDs frozen from AsstMsg.h@d94dd007"
	fields["multi_round_note"] = "成功场景为同一未关闭实例连续两轮任务,无任何 Close/NewInstance(§8 多轮复用契约)"
	return fields
}
