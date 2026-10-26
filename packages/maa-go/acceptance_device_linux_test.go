//go:build linux

// 本文件是 MAA-28 的显式设备验收入口（M4 阶段；绑定设计 §9、§10）：
// 建立「默认关闭、由执行者显式指定设备与允许任务」的设备验收门，使
// 后续设备测试（MAA-29 连接/截图、MAA-30 无消耗识别任务）可重复且范
// 围明确。本文件只交付入口与前置检查——显式配置格式、许可范围模型、
// 识别任务允许清单的精确准入、独立子进程执行器的外部超时纪律、证据
// 去敏方式——不编写 control、worker IPC 或任务编排，也不执行任何设备
// 动作：本文件全部代码路径不调用 Open/Connect/Screencap/AppendTask/
// Start（设备动作只允许出现在 MAA-29/30 按其任务文件明文定义的场景中）。
//
// 与 acceptance_linux_test.go（MAA-08）/acceptance_nodevice_linux_test.go
// （MAA-27）的关系：直接复用其中的 childOutcome/childDiagnostics；本
// 文件不改动那两个文件，自带独立的子进程执行器（runM4DeviceScenario，
// 与 runM1Child/runM3Child 同形）与环境剥离（m4ChildEnviron）。设备
// 验收与无设备测试严格分开：无设备链路验收（M3）不需要任何配置即可
// 运行；本文件的设备入口用例不接触设备、不需要锁定发行包，默认全部
// 可运行——真正连接设备的场景（MAA-29/30）必须以 requireM4DeviceAccep
// tance 开头，未显式配置时该门 SKIP（跳过不计为通过）。
//
// # 显式配置格式（唯一启用方式）
//
// 设备验收只经环境变量 MAA_M4_DEVICE_CONFIG 启用：其值为一个 JSON 配
// 置文件的绝对路径。变量未设置或为空 = 未启用（默认态）；设置后配置
// 加载/校验的任何失败都以 *deviceAcceptanceRefusal 拒绝，拒绝发生在
// 任何原生调用之前（本文件的加载与校验是纯 Go：只读文件与 os.Stat，
// 不构造 Runtime、不加载库、不触达设备）：
//
//	{
//	  "enable": true,                      // 必须显式 true；缺省/false 拒绝
//	  "device": {                          // 设备选择与 ADB 来源（必须显式给出，不得猜测或枚举）
//	    "adb_path":  "/abs/path/to/adb",   // ADB 可执行文件：非空、绝对路径、必须存在
//	    "address":   "<执行者私有地址>",     // 连接地址：非空；私有信息，绝不提交
//	    "config":    ""                    // 连接 profile：允许空串，原样透传
//	  },
//	  "core": {                            // Core/资源快照（必须存在；本文件只做存在性预检）
//	    "library_path":  "/abs/libMaaCore.so",
//	    "resource_root": "/abs/unpacked-root",  // 其下含 resource/ 的解包根
//	    "user_dir":      "/abs/writable-dir"    // 可写用户目录
//	  },
//	  "output_dir": "/abs/evidence-dir",   // 测试输出位置：必须存在的目录，且必须位于仓库外（入口拒绝仓库内路径）
//	  "permissions": {                     // 许可范围：三者皆无则拒绝执行
//	    "connect":   true,                 // 连接场景许可
//	    "screencap": true,                 // 截图场景许可
//	    "tasks": [                         // 识别任务允许清单：精确配置，可为空
//	      {"task_type": "Award", "params": {"enable": true}}
//	    ]
//	  }
//	}
//
// 配置规则：未知字段、尾随数据、缺任何必需字段、路径缺失/非绝对、
// 许可范围全空、允许清单条目缺 task_type 或 params 非完整 JSON
// object、task_type 重复，一律以带字段路径的 *deviceAcceptanceRefusal
// 拒绝。配置文件与 output_dir 必须位于仓库（Go 模块根）之外，入口在
// 读取/校验时强制拒绝仓库内路径——这不只是纪律声明，内含设备私有地
// 址的配置与伴随私有内容的证据输出都不能有被 git add 收入的可能。
// 识别清单为空且 connect/screencap 已许可时配置有效——此时仍
// 可显式验收连接/截图；空清单下任何任务提交都被 admitTask 在原生调
// 用前拒绝。允许清单是精确匹配：task_type 逐字节相等，params 按解码
// 值深度相等（键序与空白不敏感，数值按字面形式精确比较——"1" 与
// "1.0" 不同）。未列入清单的任务在原生调用前被拒绝：无默认任务、无
// 设备猜测、无自动选择识别任务（本文件不枚举 adb 设备、不运行 adb）。
//
// # 场景执行纪律（每个场景独立子进程 + 外部超时）
//
// 每个设备场景必须经 runM4DeviceScenario 在独立子进程中运行，调用时
// 必须显式给出外部超时（>0，零/负值拒绝启动）；场景退出判据 = 子进
// 程退出码 0 且 stdout 打印该场景的 OK 标记行、由父测试独立复验其字
// 段（与 M3 相同的纵深防御）。执行器单次运行：故障后不自动重试、不
// 自动重放设备动作；超时经 SIGKILL 终止并被分类为 timeout-unknown——
// 结果未知（最终状态不明），既不是成功、更不得写成设备动作已取消。
// 原生崩溃/死锁经信号死亡成为可见失败，不 recover 后继续。
//
// # 证据去敏
//
// 真实配置文件、输出目录必须位于仓库外，入口校验强制拒绝仓库内路径
// （TestM4DeviceConfigRefusesRepoInsidePaths）；设备地址等私有信息只
// 存在于执行者的本地配置中。m4WriteScenarioEvidence 只写入场景名与显
// 示给出的元数据字段，且每个字段值先经 m4RedactEvidence 把设备地址替
// 换为占位符——只有去敏后的元数据与断言结论可以抄录进 ops；设备地址、
// 凭据、截图与原始运行日志一律不提交。本文件源码中出现的 127.0.0.1:5555
// 是回环占位地址（文档通用示例，非私有设备）；预检用的库路径指向存
// 在但非 ELF 的占位文件，任何过早的原生加载都会大声失败而非静默通过。
//
// # 验收条件 → 用例映射（MAA-28）
//
//   - 「默认运行任何普通测试均不会连接设备或执行任务」：TestM4Device
//     EntryDisabledWithoutExplicitConfig（未显式配置时入口门判定为关
//     闭）、TestM4DeviceScenarioGateSkipsWithoutExplicitConfig（入口
//     门在默认运行中可见地 SKIP——跳过不计为通过）+ 本文件结构红线
//     （无任何设备动作调用；设备入口用例全部为纯 Go 预检，不需要设
//     备与锁定包）+ MAA-29/30 场景必须以 requireM4DeviceAcceptance 开
//     头的门契约。
//   - 「缺显式启用、设备选择或许可范围时拒绝执行；识别清单为空时仍
//     能显式验证连接/截图」：TestM4DeviceConfigRefusals（逐字段拒绝
//     矩阵 + 空清单正例 + 许可门负向断言——仅识别清单许可的配置下
//     requireConnectPermission/requireScreencapPermission 必须拒绝，
//     门被改成恒放行即失败）与 TestM4DeviceEntryRefusesInChildProcess
//     （子进程端到端：配置缺陷 → 拒绝退出，绝不触及原生库）。
//   - 「未列入精确允许清单的任务在原生调用前被拒绝，无猜测设备和自
//     动选择识别任务」：TestM4DeviceTaskAllowanceGate（未列类型/参数
//     不符/越界任务/空清单全部拒绝，精确匹配含大整数字面保真；admit
//     Task 只读配置，拒绝路径不含任何实例或原生调用）。
//   - 「每个场景有超时和退出判据，故障后不自动重试，不能将终止进程
//     写成设备动作已取消」：TestM4DeviceScenarioTimeoutResultUnknown
//     （挂起场景被 SIGKILL → 分类 timeout-unknown、启动标记恰好一次
//     即无重放、诊断写明结果未知且不含「取消/cancel」字样）与
//     TestM4DeviceScenarioFailedExitReportedAsFailure（非零退出判为
//     失败并保留诊断）。
//   - 「私有地址、凭据、截图和原始运行数据不会进入 ops 提交」：
//     TestM4DeviceEntrySmokeRunWithEmptyRecognitionList（有效配置 +
//     空识别清单端到端走通入口；证据文件经写入器去敏——原始地址传入
//     后文件中只有占位符；stdout 与证据文件断言无地址）+
//     TestM4DeviceConfigRefusesRepoInsidePaths（仓库内 output_dir/配
//     置文件路径被校验强制拒绝，仓库边界不只是纪律声明）+ 本文件头去
//     敏纪律与提交边界。
//
// # 运行说明
//
// 默认（无任何环境变量）——设备入口用例照常运行且不接触设备：
//
//	CGO_ENABLED=0 go test ./packages/maa-go/ -run 'TestM4Device' -count=1 -v
//
// 显式设备验收（MAA-29/30 场景执行时的入口用法；配置与输出必须在仓
// 库外，入口拒绝仓库内路径）：
//
//	MAA_M4_DEVICE_CONFIG=/absolute/path/device-acceptance.json \
//	  CGO_ENABLED=0 go test ./packages/maa-go/ -run 'TestM4DeviceEntry' -count=1 -v
package maa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// M4 子进程分派与配置传递的环境变量：与 M1 的 MAA_M1_* / M3 的
// MAA_M3_* 互相独立，互不触发对方的 TestMain/测试守卫。
const (
	m4ChildModeEnv = "MAA_M4_CHILD_MODE"    // 子进程模式标记；缺省即父进程
	m4ConfigEnv    = "MAA_M4_DEVICE_CONFIG" // 显式设备验收配置文件的绝对路径（唯一启用方式）
)

// 子进程模式：refuse-probe 验收缺参拒绝的端到端行为；smoke 以有效配
// 置走通入口接线（不执行任何原生/设备动作）；hang/fail-exit 验收执行
// 器的超时与非零退出语义。
const (
	m4ModeRefuseProbe = "refuse-probe"
	m4ModeSmoke       = "smoke"
	m4ModeHang        = "hang"
	m4ModeFailExit    = "fail-exit"
)

// 子进程 stdout 标记：refusal 在子进程确认拒绝后打印（父进程复验其
// 字段）；wrongly-accepted 表示配置被意外接受（拒绝机制失效，子进程
// 以非 0 退出）；smoke OK 仅在子进程全部断言通过后打印。
const (
	m4RefusalMarker       = "MAA_M4_REFUSAL"
	m4WronglyAcceptedMark = "MAA_M4_WRONGLY_ACCEPTED"
	m4SmokeOKMarker       = "MAA_M4_SMOKE_OK"
	m4StartMarker         = "MAA_M4_SCENARIO_STARTED"
	m4FailExitMarker      = "MAA_M4_FAIL_EXIT"
)

// 场景结果的分类（退出判据的父侧归类）：timeout-unknown 是「结果未
// 知」——超时终止既不是成功，也不是设备动作已取消；signaled-failed
// 覆盖原生崩溃等信号死亡。不存在「cancelled」分类：终止进程不得被
// 写成动作已取消。
const (
	m4ClassOK             = "ok"
	m4ClassTimeoutUnknown = "timeout-unknown"
	m4ClassSignaledFailed = "signaled-failed"
	m4ClassFailed         = "failed"
)

// 场景超时参数：refuse-probe 与 smoke 不加载原生库（毫秒级），仍给足
// 2 分钟防慢环境误杀；hang 路径用 15 秒短超时专门验收超时终止语义
// （与 M1/M3 同参数）。WaitDelay 防止孙进程持有输出管道导致 Wait 挂起。
const (
	m4ProbeChildTimeout = 2 * time.Minute
	m4SmokeChildTimeout = 2 * time.Minute
	m4HangChildTimeout  = 15 * time.Second
	m4HangSleepDuration = 30 * time.Minute
	m4ChildWaitDelay    = 30 * time.Second
)

// m4FakeAddress 是本文件自造配置中的回环占位地址（文档通用示例值，
// 非私有设备）；m4RedactedAddress 是证据去敏后的设备地址占位符。
const (
	m4FakeAddress     = "127.0.0.1:5555"
	m4RedactedAddress = "[redacted:device-address]"
)

// deviceAcceptanceRefusal 是设备验收前置检查的统一拒绝错误：Field 为
// 配置字段路径（如 device.address、permissions.tasks[0].params），
// Reason 为可读原因。加载/校验的任何失败都归入此类型，拒绝一律发生
// 在任何原生调用之前（纯 Go 检查：读文件与 os.Stat）。
type deviceAcceptanceRefusal struct {
	Field  string
	Reason string
}

func (r *deviceAcceptanceRefusal) Error() string {
	return fmt.Sprintf("maa device acceptance refused (field %s): %s", r.Field, r.Reason)
}

// m4Refuse 构造一条拒绝错误。
func m4Refuse(field, format string, args ...any) *deviceAcceptanceRefusal {
	return &deviceAcceptanceRefusal{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// m4DeviceSelection 是显式设备选择与 ADB 来源：地址必须显式给出（入
// 口不枚举 adb 设备、不猜测地址）；config 为连接 profile，允许空串并
// 原样透传给后续 Connect 输入（本文件不发起连接）。
type m4DeviceSelection struct {
	ADBPath string `json:"adb_path"`
	Address string `json:"address"`
	Config  string `json:"config"`
}

// m4CoreSnapshot 是执行者指定的 Core/资源快照位置：本入口只做存在性
// 预检（纯 Go），不加载库；真实加载由设备场景在子进程内经公开 Open
// 完成，其失败语义由 M3 验收覆盖。
type m4CoreSnapshot struct {
	LibraryPath  string `json:"library_path"`
	ResourceRoot string `json:"resource_root"`
	UserDir      string `json:"user_dir"`
}

// m4TaskAllowance 是识别任务允许清单的一条精确配置：task_type 逐字节
// 匹配，params 必须是完整 JSON object 并按解码值精确匹配（键序/空白
// 不敏感，数值按字面形式比较）。清单登记的存在性/非消耗性证明由
// MAA-30 承担；本入口只执行精确准入与拒绝。
type m4TaskAllowance struct {
	TaskType string          `json:"task_type"`
	Params   json.RawMessage `json:"params"`
}

// m4Permissions 是许可范围：connect/screencap 是场景许可开关；tasks
// 是识别任务允许清单（可为空）。三者皆无 = 未提供许可范围，拒绝执行。
type m4Permissions struct {
	Connect   bool              `json:"connect"`
	Screencap bool              `json:"screencap"`
	Tasks     []m4TaskAllowance `json:"tasks"`
}

// deviceAcceptanceConfig 是显式设备验收配置（严格解析：未知字段与尾
// 随数据都拒绝，防止执行者拼写错误被静默忽略后以缺省值继续）。
type deviceAcceptanceConfig struct {
	Enable    bool              `json:"enable"`
	Device    m4DeviceSelection `json:"device"`
	Core      m4CoreSnapshot    `json:"core"`
	OutputDir string            `json:"output_dir"`
	Perms     m4Permissions     `json:"permissions"`
}

// m4ConfigPathFromEnv 读取显式配置路径：变量未设置或为空 = 未启用
// （默认关闭），返回 ok=false；这是设备场景「默认不连接设备」的唯一
// 开关，不接受任何隐式启用。
func m4ConfigPathFromEnv() (string, bool) {
	p := os.Getenv(m4ConfigEnv)
	if p == "" {
		return "", false
	}
	return p, true
}

// requireM4DeviceAcceptance 是设备场景的统一入口门（MAA-29/30 的场景
// 测试必须以它开头）：未显式配置时 SKIP（跳过不计为通过，普通测试默
// 认不连接设备）；已显式配置但加载/校验被拒时 Fatal——执行者明确要
// 求设备验收却给出无效配置是可见失败，不是静默跳过。返回通过全部前
// 置检查的配置。
func requireM4DeviceAcceptance(t *testing.T) *deviceAcceptanceConfig {
	t.Helper()
	path, ok := m4ConfigPathFromEnv()
	if !ok {
		t.Skipf("设备验收默认关闭：未显式设置 %s（跳过不计为通过；显式启用与配置格式见本文件头运行说明）", m4ConfigEnv)
	}
	cfg, err := loadM4DeviceConfig(path)
	if err != nil {
		t.Fatalf("设备验收配置被拒绝，不执行任何原生设备调用: %v", err)
	}
	return cfg
}

// loadM4DeviceConfig 读取并校验显式配置：任何失败（文件不可读、JSON
// 非法、未知字段、尾随数据、缺参、越界、位于仓库内）都返回
// *deviceAcceptanceRefusal。校验为纯 Go：只读文件与 os.Stat，不构造
// Runtime、不加载库、不触达设备——拒绝必然发生在任何原生调用之前。
// 配置文件本身必须位于仓库外（内含设备私有地址，绝不入库），入口在
// 读取之前先做仓库边界拒绝。
func loadM4DeviceConfig(path string) (*deviceAcceptanceConfig, error) {
	if !filepath.IsAbs(path) {
		return nil, m4Refuse("config_file", "显式配置路径必须是绝对路径, got %q", path)
	}
	if err := m4RequireOutsideModuleRoot("config_file", path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, m4Refuse("config_file", "显式配置必须经 %s 提供且可读: %v", m4ConfigEnv, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg deviceAcceptanceConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, m4Refuse("config", "配置 JSON 非法或含未知字段: %v", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, m4Refuse("config", "配置 JSON 对象之后存在尾随数据")
	}
	if err := m4ValidateDeviceConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// m4ValidateDeviceConfig 按固定顺序执行全部前置检查，失败返回带字段
// 路径的拒绝：显式启用 → 设备选择与 ADB 来源 → Core/资源快照存在性 →
// 测试输出位置 → 许可范围 → 识别任务允许清单。
func m4ValidateDeviceConfig(cfg *deviceAcceptanceConfig) error {
	if !cfg.Enable {
		return m4Refuse("enable", "设备验收必须显式启用（\"enable\": true）；默认关闭，不得隐式运行")
	}
	// 设备选择与 ADB 来源：必须显式给出，入口不枚举设备、不猜测地址。
	if err := m4RequireNonEmptyText("device.adb_path", cfg.Device.ADBPath); err != nil {
		return err
	}
	if !filepath.IsAbs(cfg.Device.ADBPath) {
		return m4Refuse("device.adb_path", "ADB 路径必须是绝对路径, got %q", cfg.Device.ADBPath)
	}
	if err := m4RequireExistingPath("device.adb_path", cfg.Device.ADBPath, false); err != nil {
		return err
	}
	if err := m4RequireNonEmptyText("device.address", cfg.Device.Address); err != nil {
		return err
	}
	if strings.ContainsRune(cfg.Device.Config, '\x00') {
		return m4Refuse("device.config", "连接 profile 不得包含 NUL 字节")
	}
	// Core/资源快照：非空绝对路径且必须存在（库为文件，资源根/用户目
	// 录为目录）。存在性预检是纯 Go，不加载库。
	if err := m4RequireAbsExisting("core.library_path", cfg.Core.LibraryPath, false); err != nil {
		return err
	}
	if err := m4RequireAbsExisting("core.resource_root", cfg.Core.ResourceRoot, true); err != nil {
		return err
	}
	if err := m4RequireAbsExisting("core.user_dir", cfg.Core.UserDir, true); err != nil {
		return err
	}
	// 测试输出位置：存在的目录，且必须位于仓库外（校验强制，防执行者
	// 误配仓库内路径后证据文件被 git add 收入提交）。
	if err := m4RequireAbsExisting("output_dir", cfg.OutputDir, true); err != nil {
		return err
	}
	if err := m4RequireOutsideModuleRoot("output_dir", cfg.OutputDir); err != nil {
		return err
	}
	// 许可范围：三者皆无 = 未提供许可范围，拒绝执行。
	if !cfg.Perms.Connect && !cfg.Perms.Screencap && len(cfg.Perms.Tasks) == 0 {
		return m4Refuse("permissions", "未提供许可范围：请授予 connect/screencap 或登记识别任务允许清单；无显式范围不执行")
	}
	// 识别任务允许清单：精确配置，无默认任务。
	seen := make(map[string]bool, len(cfg.Perms.Tasks))
	for i, a := range cfg.Perms.Tasks {
		field := fmt.Sprintf("permissions.tasks[%d]", i)
		if err := m4RequireNonEmptyText(field+".task_type", a.TaskType); err != nil {
			return err
		}
		if seen[a.TaskType] {
			return m4Refuse(field, "task_type %q 重复：允许清单必须精确、无歧义", a.TaskType)
		}
		seen[a.TaskType] = true
		if err := m4RequireJSONObject(field+".params", a.Params); err != nil {
			return err
		}
	}
	return nil
}

// m4RequireNonEmptyText 要求非空且不含 NUL 的文本（地址、路径等将流
// 入 C 字符串的字段）。
func m4RequireNonEmptyText(field, value string) error {
	switch {
	case value == "":
		return m4Refuse(field, "不得为空")
	case strings.ContainsRune(value, '\x00'):
		return m4Refuse(field, "不得包含 NUL 字节")
	}
	return nil
}

// m4RequireAbsExisting 要求非空绝对路径且文件系统上存在；wantDir 规
// 定必须是目录（true）或非目录（false）。
func m4RequireAbsExisting(field, value string, wantDir bool) error {
	if err := m4RequireNonEmptyText(field, value); err != nil {
		return err
	}
	if !filepath.IsAbs(value) {
		return m4Refuse(field, "必须是绝对路径, got %q", value)
	}
	info, err := os.Stat(value)
	if err != nil {
		return m4Refuse(field, "路径必须存在: %v", err)
	}
	if info.IsDir() != wantDir {
		kind, want := "目录", "文件"
		if wantDir {
			kind, want = "文件", "目录"
		}
		return m4Refuse(field, "是%s, want %s", kind, want)
	}
	return nil
}

// m4RequireExistingPath 是不要求绝对路径的存在性检查（用于与执行者
// 环境相关的 ADB 来源）。
func m4RequireExistingPath(field, value string, wantDir bool) error {
	info, err := os.Stat(value)
	if err != nil {
		return m4Refuse(field, "路径必须存在: %v", err)
	}
	if info.IsDir() != wantDir {
		return m4Refuse(field, "路径类型不符 (isDir=%v, wantDir=%v)", info.IsDir(), wantDir)
	}
	return nil
}

// m4ModuleRoot 从当前运行目录（go test 的运行目录是包源码目录）向上
// 寻找包含 go.mod 的目录作为 Go 模块根（= 仓库根）；找不到时返回空
// 串。子进程继承父进程工作目录，父子进程得到同一模块根。
func m4ModuleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// m4RequireOutsideModuleRoot 拒绝位于 Go 模块根（仓库根）之内的路径：
// 设备验收的配置文件与证据输出内含/伴随执行者私有数据，绝不能被
// git add 收入提交，因此仓库边界由校验强制而非仅靠纪律声明。无法定
// 位模块根（非模块布局运行）时不猜测、不拒绝。判定为词法相对路径
// 检查，不解析符号链接。
func m4RequireOutsideModuleRoot(field, path string) error {
	root := m4ModuleRoot()
	if root == "" {
		return nil
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return m4Refuse(field, "路径 %q 无法相对模块根 %s 解析: %v", path, root, err)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return m4Refuse(field, "路径 %q 位于仓库（模块根 %s）之内：设备验收的配置与证据输出必须位于仓库外", path, root)
	}
	return nil
}

// m4RequireJSONObject 要求 params 是非空、完整的 JSON object（不限制
// 内部结构；未知字段保留原文语义，由精确匹配把关）。
func m4RequireJSONObject(field string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return m4Refuse(field, "params 必须是非空 JSON object")
	}
	v, err := m4DecodeSingleJSON(trimmed)
	if err != nil {
		return m4Refuse(field, "params 必须是完整 JSON: %v", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return m4Refuse(field, "params 必须是 JSON object")
	}
	return nil
}

// m4DecodeSingleJSON 解码单个 JSON 值（数值保留为 json.Number 字面形
// 式），拒绝值之后的尾随数据。
func m4DecodeSingleJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, errors.New("尾随数据")
	}
	return v, nil
}

// requireConnectPermission 是连接场景的许可门：未授予时拒绝——场景
// 在发起任何原生连接调用之前必须先通过本门。
func (c *deviceAcceptanceConfig) requireConnectPermission() error {
	if !c.Perms.Connect {
		return m4Refuse("permissions.connect", "连接场景未获许可：不得发起任何原生连接调用")
	}
	return nil
}

// requireScreencapPermission 是截图场景的许可门：未授予时拒绝——场
// 景在发起任何原生截图调用之前必须先通过本门。
func (c *deviceAcceptanceConfig) requireScreencapPermission() error {
	if !c.Perms.Screencap {
		return m4Refuse("permissions.screencap", "截图场景未获许可：不得发起任何原生截图调用")
	}
	return nil
}

// admitTask 是识别任务的精确准入门：task_type 未列入允许清单、或
// params 与清单的精确配置不符时，在任何原生调用（AppendTask 等）之
// 前拒绝。本方法只读配置、不触达实例与原生层——拒绝路径没有设备动
// 作；无默认任务、无自动选择。
func (c *deviceAcceptanceConfig) admitTask(taskType string, params json.RawMessage) error {
	for _, a := range c.Perms.Tasks {
		if a.TaskType != taskType {
			continue
		}
		if m4SameJSONValue(a.Params, params) {
			return nil
		}
		return m4Refuse("permissions.tasks", "任务 %q 已列入允许清单但 params 与精确配置不符：在原生调用前拒绝", taskType)
	}
	return m4Refuse("permissions.tasks", "任务类型 %q 未列入显式允许清单：在原生调用前拒绝（无默认任务、无自动选择识别任务）", taskType)
}

// m4SameJSONValue 判定两份 JSON 是否为同一解码值：键序与空白不敏感；
// 数值经 UseNumber 保留字面形式（1 与 1.0 视为不同，杜绝宽化猜测）。
// 任一侧解析失败都判不等（拒绝是安全方向）。
func m4SameJSONValue(a, b json.RawMessage) bool {
	va, err := m4DecodeSingleJSON(bytes.TrimSpace(a))
	if err != nil {
		return false
	}
	vb, err := m4DecodeSingleJSON(bytes.TrimSpace(b))
	if err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// m4RedactEvidence 去敏一条证据文本：设备地址的所有出现替换为占位符。
// 空地址（未配置态）原样返回。
func m4RedactEvidence(s, deviceAddress string) string {
	if deviceAddress == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, deviceAddress, m4RedactedAddress)
}

// m4WriteScenarioEvidence 把一个场景的去敏证据元数据写入配置指定的测
// 试输出位置（output_dir，必须在仓库外）：只写场景名与显式给出的字
// 段——不写整份配置、不写原始运行数据；每个字段值先经
// m4RedactEvidence 去敏设备地址。返回写入路径。
func m4WriteScenarioEvidence(t *testing.T, cfg *deviceAcceptanceConfig, scenario string, fields map[string]string) string {
	t.Helper()
	redacted := make(map[string]string, len(fields))
	for k, v := range fields {
		redacted[k] = m4RedactEvidence(v, cfg.Device.Address)
	}
	data, err := json.MarshalIndent(map[string]any{"scenario": scenario, "fields": redacted}, "", "  ")
	if err != nil {
		t.Fatalf("证据元数据编码失败: %v", err)
	}
	path := filepath.Join(cfg.OutputDir, "evidence-"+scenario+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("证据元数据写入 %s 失败: %v", path, err)
	}
	return path
}

// m4ScenarioClassification 归类一次场景执行结果（退出判据的父侧判定）：
// 退出码 0 → ok（是否真正通过还须父测试复验 OK 标记字段）；外部超时
// 终止 → timeout-unknown（结果未知）；信号死亡 → signaled-failed；
// 其余非零退出 → failed。
func m4ScenarioClassification(out childOutcome) string {
	switch {
	case out.err == nil:
		return m4ClassOK
	case out.timedOut:
		return m4ClassTimeoutUnknown
	case out.signaled:
		return m4ClassSignaledFailed
	default:
		return m4ClassFailed
	}
}

// m4RenderScenarioDiagnostics 渲染场景诊断：分类 + 完整子进程输出。
// 超时分类的说明文字必须写明「结果未知」，且不得出现「取消/cancel」
// 字样——被终止的进程不得被描述为设备动作已取消（验收条件明文）。
func m4RenderScenarioDiagnostics(mode string, out childOutcome) string {
	var b strings.Builder
	kind := m4ScenarioClassification(out)
	fmt.Fprintf(&b, "scenario=%s classification=%s\n", mode, kind)
	if kind == m4ClassTimeoutUnknown {
		fmt.Fprintf(&b, "外部超时已终止子进程：场景结果未知（最终状态不明，不是成功）；不自动重试，不重放设备动作。\n")
	}
	b.WriteString(childDiagnostics(mode, out))
	return b.String()
}

// runM4DeviceScenario 以 go test 自身二进制在独立子进程中运行一个设
// 备场景（-test.run 钉住单个测试名，子进程在该测试函数顶部按
// m4ChildModeEnv 守卫分岔）。timeout 必须显式给出且 >0（零/负值拒绝
// 启动——无超时的设备场景不得运行）；超时经 exec.CommandContext 外部
// 强制。单次运行：不重试、不重放、不 recover，执行结果原样交回调用
// 方判定（形态与 M1/M3 执行器一致）。
func runM4DeviceScenario(t *testing.T, timeout time.Duration, mode string, extraEnv map[string]string) childOutcome {
	t.Helper()
	if timeout <= 0 {
		t.Fatalf("场景 %s 必须携带显式外部超时（>0）；无超时的设备场景不得启动", mode)
	}
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("定位测试二进制 %q 失败: %v", os.Args[0], err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+t.Name()+"$", "-test.v=true")
	cmd.Env = m4ChildEnviron(mode)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = m4ChildWaitDelay

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

// m4ChildEnviron 构造子进程环境：与 M1/M3 同一策略——剥离动态链接器
// 注入变量（LD_LIBRARY_PATH/LD_PRELOAD/LD_AUDIT）后原样继承，再附上
// M4 模式标记（显式配置路径由调用方经 extraEnv 传入）。
func m4ChildEnviron(mode string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key == "LD_LIBRARY_PATH" || key == "LD_PRELOAD" || key == "LD_AUDIT" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, m4ChildModeEnv+"="+mode)
}

// m4FindMarkerLine 在子进程 stdout 中定位以 marker 开头的行。
func m4FindMarkerLine(stdout, marker string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, marker+" ") {
			return line, true
		}
	}
	return "", false
}

// TestM4DeviceEntryDisabledWithoutExplicitConfig 验收默认关闭语义：
// 环境变量未设置或为空时，设备入口门判定为未启用——这是「默认运行任
// 何普通测试均不会连接设备或执行任务」的开关依据；显式启用只能来自
// MAA_M4_DEVICE_CONFIG 指向的有效配置。
func TestM4DeviceEntryDisabledWithoutExplicitConfig(t *testing.T) {
	if _, present := os.LookupEnv(m4ConfigEnv); !present {
		if _, ok := m4ConfigPathFromEnv(); ok {
			t.Fatalf("未设置 %s 时入口门必须判定为未启用", m4ConfigEnv)
		}
		t.Logf("默认环境未设置 %s：入口门未启用（与预期一致）", m4ConfigEnv)
	}
	t.Setenv(m4ConfigEnv, "")
	if path, ok := m4ConfigPathFromEnv(); ok {
		t.Fatalf("空字符串 %s 不得视为显式启用 (got %q)", m4ConfigEnv, path)
	}
	t.Logf("入口门仅由 %s 显式启用，默认关闭已验收", m4ConfigEnv)
}

// TestM4DeviceScenarioGateSkipsWithoutExplicitConfig 让设备场景入口门
// （requireM4DeviceAcceptance——MAA-29/30 的场景测试必须以它开头）在
// 默认测试运行中直接可见：未显式配置时本测试被该门 SKIP（跳过不计为
// 通过，普通测试默认不连接设备）；已显式配置时退化为配置有效性检查
// ——配置被拒即失败，配置有效则通过。
func TestM4DeviceScenarioGateSkipsWithoutExplicitConfig(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	t.Logf("设备验收已显式启用：配置通过全部前置检查 (output_dir=%s, connect=%v, screencap=%v, tasks=%d)",
		cfg.OutputDir, cfg.Perms.Connect, cfg.Perms.Screencap, len(cfg.Perms.Tasks))
}

// TestM4DeviceConfigRefusals 验收显式配置的缺参/越界拒绝矩阵（纯 Go，
// 不触达设备与原生库）：缺显式启用、缺设备地址、ADB 路径缺失/相对、
// Core 快照缺失、输出目录缺失、许可范围全空、未知字段、尾随数据——
// 全部以带字段路径的 *deviceAcceptanceRefusal 拒绝且不返回配置；同时
// 验收正例：识别清单为空且 connect/screencap 已许可时配置有效（仍可
// 显式验收连接/截图），识别清单非空时同样有效。
func TestM4DeviceConfigRefusals(t *testing.T) {
	dir := m4PrepareProbeTree(t)

	write := func(name string, mutate func(*deviceAcceptanceConfig)) string {
		t.Helper()
		cfg := m4ValidProbeBaseConfig(dir)
		if mutate != nil {
			mutate(&cfg)
		}
		return m4WriteConfigFile(t, dir, name, &cfg)
	}

	cases := []struct {
		name      string
		mutate    func(*deviceAcceptanceConfig)
		wantField string
	}{
		{"missing-enable", func(c *deviceAcceptanceConfig) { c.Enable = false }, "enable"},
		{"missing-device-address", func(c *deviceAcceptanceConfig) { c.Device.Address = "" }, "device.address"},
		{"relative-adb-path", func(c *deviceAcceptanceConfig) { c.Device.ADBPath = "adb" }, "device.adb_path"},
		{"missing-adb-binary", func(c *deviceAcceptanceConfig) {
			c.Device.ADBPath = filepath.Join(dir, "no-such-adb")
		}, "device.adb_path"},
		{"missing-library", func(c *deviceAcceptanceConfig) {
			c.Core.LibraryPath = filepath.Join(dir, "no-such-lib.so")
		}, "core.library_path"},
		{"missing-resource-root", func(c *deviceAcceptanceConfig) {
			c.Core.ResourceRoot = filepath.Join(dir, "no-such-resource")
		}, "core.resource_root"},
		{"missing-user-dir", func(c *deviceAcceptanceConfig) {
			c.Core.UserDir = filepath.Join(dir, "no-such-user")
		}, "core.user_dir"},
		{"missing-output-dir", func(c *deviceAcceptanceConfig) {
			c.OutputDir = filepath.Join(dir, "no-such-out")
		}, "output_dir"},
		{"no-permission-scope", func(c *deviceAcceptanceConfig) {
			c.Perms.Connect = false
			c.Perms.Screencap = false
			c.Perms.Tasks = nil
		}, "permissions"},
	}
	for _, tc := range cases {
		path := write(tc.name+".json", tc.mutate)
		cfg, err := loadM4DeviceConfig(path)
		if cfg != nil || err == nil {
			t.Fatalf("%s: 配置被意外接受 (cfg=%v)", tc.name, cfg)
		}
		var r *deviceAcceptanceRefusal
		if !errors.As(err, &r) {
			t.Fatalf("%s: 错误类型不符: %T (%v)", tc.name, err, err)
		}
		if r.Field != tc.wantField {
			t.Fatalf("%s: 拒绝字段 = %q, want %q (%v)", tc.name, r.Field, tc.wantField, err)
		}
	}

	// 未知字段与尾随数据（对序列化后的 JSON 文本注入，结构体变异表达
	// 不了这两类破坏）。
	base := m4ValidProbeBaseConfig(dir)
	validJSON := m4WriteConfigFile(t, dir, "valid.json", &base)
	raw, err := os.ReadFile(validJSON)
	if err != nil {
		t.Fatalf("读取有效配置失败: %v", err)
	}
	unknown := bytes.Replace(raw, []byte(`{"enable"`), []byte(`{"m4_unknown_field":true,"enable"`), 1)
	if bytes.Equal(unknown, raw) {
		t.Fatalf("未知字段注入失败: %s", raw)
	}
	unknownPath := filepath.Join(dir, "unknown-field.json")
	if err := os.WriteFile(unknownPath, unknown, 0o644); err != nil {
		t.Fatalf("写入未知字段配置失败: %v", err)
	}
	trailingPath := filepath.Join(dir, "trailing-data.json")
	if err := os.WriteFile(trailingPath, append(bytes.Clone(raw), ' ', '{', '}'), 0o644); err != nil {
		t.Fatalf("写入尾随数据配置失败: %v", err)
	}
	for _, tc := range []struct{ name, path string }{
		{"unknown-field", unknownPath},
		{"trailing-data", trailingPath},
	} {
		if cfg, err := loadM4DeviceConfig(tc.path); cfg != nil || err == nil {
			t.Fatalf("%s: 配置被意外接受", tc.name)
		} else if r := (*deviceAcceptanceRefusal)(nil); errors.As(err, &r) && r.Field != "config" {
			t.Fatalf("%s: 拒绝字段 = %q, want \"config\" (%v)", tc.name, r.Field, err)
		}
	}

	// 正例 1：识别清单为空 + connect/screencap 已许可 → 有效（此时仍
	// 能显式验证连接/截图）。
	emptyList, err := loadM4DeviceConfig(validJSON)
	if err != nil {
		t.Fatalf("空识别清单的有效配置被拒绝: %v", err)
	}
	if !emptyList.Perms.Connect || !emptyList.Perms.Screencap || len(emptyList.Perms.Tasks) != 0 {
		t.Fatalf("空清单正例的许可范围不符: %+v", emptyList.Perms)
	}
	if err := emptyList.requireConnectPermission(); err != nil {
		t.Fatalf("空清单正例的连接许可门被拒: %v", err)
	}
	if err := emptyList.requireScreencapPermission(); err != nil {
		t.Fatalf("空清单正例的截图许可门被拒: %v", err)
	}
	// 正例 2：仅识别清单许可（connect/screencap 关闭）→ 有效。
	recogOnly := m4ValidProbeBaseConfig(dir)
	recogOnly.Perms.Connect = false
	recogOnly.Perms.Screencap = false
	recogOnly.Perms.Tasks = []m4TaskAllowance{{TaskType: "Award", Params: json.RawMessage(`{"enable":true}`)}}
	recogPath := m4WriteConfigFile(t, dir, "recognition-only.json", &recogOnly)
	if _, err := loadM4DeviceConfig(recogPath); err != nil {
		t.Fatalf("仅识别清单许可的配置被拒绝: %v", err)
	}
	// 负向断言：同一份仅识别清单许可的配置下，连接与截图许可门必须
	// 拒绝——未授权场景在发起任何原生连接/截图调用之前被拒（门的拒绝
	// 分支可证伪：门被改成恒放行时本断言失败）。
	if err := recogOnly.requireConnectPermission(); err == nil {
		t.Fatalf("仅识别清单许可的配置必须被连接许可门拒绝（未授予 connect 不得放行）")
	} else {
		var r *deviceAcceptanceRefusal
		if !errors.As(err, &r) || r.Field != "permissions.connect" {
			t.Fatalf("连接许可门的拒绝错误类型/字段不符: %v", err)
		}
	}
	if err := recogOnly.requireScreencapPermission(); err == nil {
		t.Fatalf("仅识别清单许可的配置必须被截图许可门拒绝（未授予 screencap 不得放行）")
	} else {
		var r *deviceAcceptanceRefusal
		if !errors.As(err, &r) || r.Field != "permissions.screencap" {
			t.Fatalf("截图许可门的拒绝错误类型/字段不符: %v", err)
		}
	}
}

// TestM4DeviceConfigRefusesRepoInsidePaths 验收仓库边界拒绝由校验强制
// 而非仅纪律声明（审查第 1 轮发现 2）：output_dir 指向仓库内已存在的
// 目录（包源码目录本身）→ 以 output_dir 拒绝；配置文件路径位于仓库内
// → 在读取之前以 config_file 拒绝（路径不存在也可判定）；相对配置路
// 径同样在 config_file 拒绝。无法定位模块根的运行环境跳过（不猜测、
// 不误伤）。本用例只做加载校验，不在仓库内写任何文件。
func TestM4DeviceConfigRefusesRepoInsidePaths(t *testing.T) {
	root := m4ModuleRoot()
	if root == "" {
		t.Skip("无法定位模块根（运行目录祖先链上无 go.mod），仓库边界检查不可用；跳过不计为通过")
	}
	dir := m4PrepareProbeTree(t)

	// output_dir 指向仓库内已存在的目录：存在性预检通过后仍必须被仓库
	// 边界拒绝——去敏只保护地址字段，证据文件本身可能含执行者私有内容。
	cfg := m4ValidProbeBaseConfig(dir)
	cfg.OutputDir = filepath.Join(root, "packages", "maa-go")
	_, err := loadM4DeviceConfig(m4WriteConfigFile(t, dir, "inside-output.json", &cfg))
	if err == nil {
		t.Fatalf("仓库内 output_dir 必须被拒绝")
	}
	var r *deviceAcceptanceRefusal
	if !errors.As(err, &r) || r.Field != "output_dir" || !strings.Contains(r.Reason, "仓库") {
		t.Fatalf("仓库内 output_dir 的拒绝不符（want output_dir + 仓库原因）: %v", err)
	}

	// 配置文件位于仓库内：拒绝发生在读取之前（路径不存在也可判定）。
	_, err = loadM4DeviceConfig(filepath.Join(root, "packages", "maa-go", "m4-no-such-in-repo-config.json"))
	if err == nil {
		t.Fatalf("仓库内配置文件路径必须被拒绝（配置内含设备私有地址）")
	}
	if !errors.As(err, &r) || r.Field != "config_file" || !strings.Contains(r.Reason, "仓库") {
		t.Fatalf("仓库内配置文件的拒绝不符: %v", err)
	}

	// 相对配置路径：同样在 config_file 处拒绝（显式配置要求绝对路径）。
	_, err = loadM4DeviceConfig("relative-config.json")
	if err == nil {
		t.Fatalf("相对配置路径必须被拒绝")
	}
	if !errors.As(err, &r) || r.Field != "config_file" {
		t.Fatalf("相对配置路径的拒绝不符: %v", err)
	}
}

// TestM4DeviceTaskAllowanceGate 验收识别任务的精确准入与拒绝机制：未
// 列入允许清单的任务类型、参数与精确配置不符的任务、空清单下的任何
// 任务，全部在原生调用前被拒绝（admitTask 只读配置、不触达实例与原
// 生层，拒绝路径无设备动作）；精确匹配对键序/空白不敏感、对数值字面
// 形式（大整数、1 与 1.0）敏感——无宽化猜测。
func TestM4DeviceTaskAllowanceGate(t *testing.T) {
	dir := m4PrepareProbeTree(t)
	listed := m4ValidProbeBaseConfig(dir)
	listed.Perms.Connect = false
	listed.Perms.Screencap = false
	listed.Perms.Tasks = []m4TaskAllowance{
		{TaskType: "Award", Params: json.RawMessage(`{"enable":true}`)},
		{TaskType: "BigNumber", Params: json.RawMessage(`{"n":12345678901234567890}`)},
	}
	cfg, err := loadM4DeviceConfig(m4WriteConfigFile(t, dir, "allowance.json", &listed))
	if err != nil {
		t.Fatalf("允许清单配置被拒绝: %v", err)
	}

	// 精确匹配 → 准入。
	if err := cfg.admitTask("Award", json.RawMessage(`{"enable":true}`)); err != nil {
		t.Fatalf("清单内任务的精确匹配被误拒: %v", err)
	}
	// 键序与空白不敏感。
	if err := cfg.admitTask("Award", json.RawMessage("  { \"enable\" : true }  ")); err != nil {
		t.Fatalf("键序/空白等价形式的精确匹配被误拒: %v", err)
	}
	// 大整数字面保真。
	if err := cfg.admitTask("BigNumber", json.RawMessage(`{"n":12345678901234567890}`)); err != nil {
		t.Fatalf("大整数精确匹配被误拒: %v", err)
	}

	// 拒绝矩阵：全部在原生调用前拒绝。
	cases := []struct {
		name    string
		task    string
		params  json.RawMessage
		wantMsg string
	}{
		{"unlisted-task-type", "Click", json.RawMessage(`{}`), "未列入显式允许清单"},
		{"params-value-mismatch", "Award", json.RawMessage(`{"enable":false}`), "params 与精确配置不符"},
		{"params-extra-field", "Award", json.RawMessage(`{"enable":true,"extra":1}`), "params 与精确配置不符"},
		{"params-missing-field", "Award", json.RawMessage(`{}`), "params 与精确配置不符"},
		{"params-numeric-form", "BigNumber", json.RawMessage(`{"n":12345678901234567891}`), "params 与精确配置不符"},
		{"params-widened-number", "BigNumber", json.RawMessage(`{"n":1.0}`), "params 与精确配置不符"},
		{"params-invalid-json", "Award", json.RawMessage(`{`), "params 与精确配置不符"},
	}
	for _, tc := range cases {
		err := cfg.admitTask(tc.task, tc.params)
		if err == nil {
			t.Fatalf("%s: 越界任务被意外准入 (%s %s)", tc.name, tc.task, tc.params)
		}
		var r *deviceAcceptanceRefusal
		if !errors.As(err, &r) || r.Field != "permissions.tasks" {
			t.Fatalf("%s: 错误类型/字段不符: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Fatalf("%s: 拒绝原因不含 %q: %v", tc.name, tc.wantMsg, err)
		}
	}

	// 空清单：任何任务提交都被拒绝（无默认任务、无自动选择）。
	empty := m4ValidProbeBaseConfig(dir)
	empty.Perms.Connect = true
	empty.Perms.Screencap = true
	empty.Perms.Tasks = nil
	emptyCfg, err := loadM4DeviceConfig(m4WriteConfigFile(t, dir, "empty-allowance.json", &empty))
	if err != nil {
		t.Fatalf("空清单配置被拒绝: %v", err)
	}
	if err := emptyCfg.admitTask("Award", json.RawMessage(`{"enable":true}`)); err == nil {
		t.Fatalf("空识别清单下任务提交必须被拒绝")
	} else if !strings.Contains(err.Error(), "未列入显式允许清单") {
		t.Fatalf("空清单拒绝原因不符: %v", err)
	}
}

// TestM4DeviceEntryRefusesInChildProcess 在子进程端到端验收缺参拒绝：
// 执行者显式设置了配置环境变量但配置缺显式启用/缺设备地址/缺许可范围
// /缺 Core 快照/不可读/含未知字段时，入口在子进程内被拒绝并以拒绝元
// 数据退出（退出码 0、打印 MAA_M4_REFUSAL 行）——拒绝发生在任何原生
// 库接触之前（占位库路径是存在但非 ELF 的文本文件：若实现过早尝试加
// 载，子进程会以库加载失败而非拒绝元数据收场，本用例即失败）。
func TestM4DeviceEntryRefusesInChildProcess(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4ModeRefuseProbe {
		runM4RefuseProbeChild(t)
		return
	}
	dir := m4PrepareProbeTree(t)

	build := func(name string, mutate func(*deviceAcceptanceConfig)) string {
		t.Helper()
		cfg := m4ValidProbeBaseConfig(dir)
		if mutate != nil {
			mutate(&cfg)
		}
		return m4WriteConfigFile(t, dir, name, &cfg)
	}
	cases := []struct {
		name       string
		configPath string // 非空表示直接使用该绝对路径（不生成配置文件）；空串表示生成有效配置再按 mutate 变异
		mutate     func(*deviceAcceptanceConfig)
		wantField  string
	}{
		// 绝对路径但文件不存在（tempdir 在仓库外）：拒绝原因是不可读，
		// 不是相对路径或仓库边界。
		{"missing-config-file", filepath.Join(dir, "no-such-config.json"), nil, "config_file"},
		{"not-enabled", "", func(c *deviceAcceptanceConfig) { c.Enable = false }, "enable"},
		{"missing-device-address", "", func(c *deviceAcceptanceConfig) { c.Device.Address = "" }, "device.address"},
		{"no-permission-scope", "", func(c *deviceAcceptanceConfig) {
			c.Perms.Connect = false
			c.Perms.Screencap = false
			c.Perms.Tasks = nil
		}, "permissions"},
		{"missing-library", "", func(c *deviceAcceptanceConfig) {
			c.Core.LibraryPath = filepath.Join(dir, "no-such-lib.so")
		}, "core.library_path"},
	}
	for _, tc := range cases {
		configPath := tc.configPath
		if configPath == "" {
			configPath = build(tc.name+".json", tc.mutate)
		}
		out := runM4DeviceScenario(t, m4ProbeChildTimeout, m4ModeRefuseProbe, map[string]string{
			m4ConfigEnv: configPath,
		})
		if out.err != nil {
			t.Fatalf("%s: refuse-probe 子进程失败:\n%s", tc.name, m4RenderScenarioDiagnostics(m4ModeRefuseProbe, out))
		}
		if err := verifyM4RefusalProbe(out.stdout, tc.wantField); err != nil {
			t.Fatalf("%s: 拒绝元数据复验失败:\n%v\n%s", tc.name, err, m4RenderScenarioDiagnostics(m4ModeRefuseProbe, out))
		}
		if strings.Contains(out.stdout, m4WronglyAcceptedMark) {
			t.Fatalf("%s: 配置被意外接受，拒绝机制失效:\n%s", tc.name, out.stdout)
		}
	}
}

// runM4RefuseProbeChild 是 m4ModeRefuseProbe 子进程入口：加载显式配置
// 并预期拒绝——拒绝时打印带字段路径的元数据行后正常退出（测试通过）；
// 配置被意外接受（拒绝机制失效）或拒绝错误类型不符时断言失败（非 0
// 退出）。子进程不执行任何原生调用。
func runM4RefuseProbeChild(t *testing.T) {
	path, ok := m4ConfigPathFromEnv()
	if !ok {
		t.Fatalf("refuse-probe 子进程缺少 %s", m4ConfigEnv)
	}
	cfg, err := loadM4DeviceConfig(path)
	if err == nil {
		fmt.Printf("%s config=%+v\n", m4WronglyAcceptedMark, cfg)
		t.Fatalf("配置被意外接受，拒绝机制失效: %+v", cfg)
	}
	var r *deviceAcceptanceRefusal
	if !errors.As(err, &r) {
		t.Fatalf("拒绝错误类型不符: %T (%v)", err, err)
	}
	fmt.Printf("%s field=%s reason=%s\n", m4RefusalMarker, r.Field, r.Reason)
}

// verifyM4RefusalProbe 对 refuse-probe 子进程的 stdout 做父侧复验：
// 必须存在 MAA_M4_REFUSAL 行且 field= 与预期的拒绝字段一致（与子进程
// 内断言互相独立的纵深防御）。
func verifyM4RefusalProbe(stdout, wantField string) error {
	line, ok := m4FindMarkerLine(stdout, m4RefusalMarker)
	if !ok {
		return fmt.Errorf("stdout 缺少 %s 元数据行", m4RefusalMarker)
	}
	field := ""
	for _, f := range strings.Fields(line)[1:] {
		if strings.HasPrefix(f, "field=") {
			field = strings.TrimPrefix(f, "field=")
		}
	}
	if field != wantField {
		return fmt.Errorf("拒绝字段 = %q, want %q (line: %s)", field, wantField, line)
	}
	return nil
}

// TestM4DeviceScenarioTimeoutResultUnknown 验收场景执行器的超时纪律：
// 挂起场景在显式外部超时后被 SIGKILL，结果必须分类为 timeout-unknown
// （结果未知——既不是成功，也不得写成设备动作已取消），诊断保留子进
// 程已产出输出；启动标记恰好出现一次，证明没有自动重放。同时验收无
// 超时（零值）的场景启动被拒绝。
func TestM4DeviceScenarioTimeoutResultUnknown(t *testing.T) {
	if err := m4CheckScenarioTimeout(m4ModeHang, 0); err == nil {
		t.Fatalf("零超时的场景启动必须被拒绝")
	}
	if err := m4CheckScenarioTimeout(m4ModeHang, -time.Second); err == nil {
		t.Fatalf("负超时的场景启动必须被拒绝")
	}

	if os.Getenv(m4ChildModeEnv) == m4ModeHang {
		fmt.Println(m4StartMarker + " started, sleeping " + m4HangSleepDuration.String())
		time.Sleep(m4HangSleepDuration)
		return
	}
	out := runM4DeviceScenario(t, m4HangChildTimeout, m4ModeHang, nil)
	if out.err == nil {
		t.Fatalf("挂起场景在 %s 超时后必须被报告为失败，实际退出码 0:\n%s",
			m4HangChildTimeout, m4RenderScenarioDiagnostics(m4ModeHang, out))
	}
	if !out.timedOut {
		t.Fatalf("失败必须可辨识为超时终止 (timedOut=false, err=%v):\n%s", out.err, m4RenderScenarioDiagnostics(m4ModeHang, out))
	}
	if !out.signaled {
		t.Fatalf("超时终止必须经信号杀死子进程 (signaled=false, err=%v):\n%s", out.err, m4RenderScenarioDiagnostics(m4ModeHang, out))
	}
	if got := strings.Count(out.stdout, m4StartMarker); got != 1 {
		t.Fatalf("场景启动标记出现 %d 次, want 恰好 1 次（故障后不自动重试、不重放设备动作）:\n%s",
			got, m4RenderScenarioDiagnostics(m4ModeHang, out))
	}
	if kind := m4ScenarioClassification(out); kind != m4ClassTimeoutUnknown {
		t.Fatalf("超时场景分类 = %q, want %q（结果未知）", kind, m4ClassTimeoutUnknown)
	}
	rendered := m4RenderScenarioDiagnostics(m4ModeHang, out)
	if !strings.Contains(rendered, "结果未知") {
		t.Fatalf("超时诊断必须写明结果未知:\n%s", rendered)
	}
	for _, forbidden := range []string{"取消", "cancel"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("超时诊断不得把被终止的场景写成已取消（含 %q）:\n%s", forbidden, rendered)
		}
	}
	if !strings.Contains(rendered, m4StartMarker) {
		t.Fatalf("超时诊断必须保留子进程已产出的输出:\n%s", rendered)
	}
	t.Logf("挂起场景按预期在 %s 被外部超时 SIGKILL，分类 timeout-unknown，诊断写明结果未知:\n%s",
		m4HangChildTimeout, rendered)
}

// m4CheckScenarioTimeout 是场景超时的启动前置检查：每个场景必须携带
// 显式外部超时（>0），零/负值拒绝启动（runM4DeviceScenario 在生成任
// 何进程之前执行本检查）。
func m4CheckScenarioTimeout(mode string, timeout time.Duration) error {
	if timeout <= 0 {
		return m4Refuse("scenario_timeout", "场景 %s 必须携带显式外部超时（>0）: got %v", mode, timeout)
	}
	return nil
}

// TestM4DeviceScenarioFailedExitReportedAsFailure 验收场景执行器的非
// 零退出判据：场景子进程以非 0 退出时必须被判为 failed（不是成功，
// 不是跳过），诊断保留完整输出，且没有第二次启动（不自动重试）。
func TestM4DeviceScenarioFailedExitReportedAsFailure(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4ModeFailExit {
		fmt.Println(m4FailExitMarker + " failing now")
		os.Exit(3)
	}
	out := runM4DeviceScenario(t, m4ProbeChildTimeout, m4ModeFailExit, nil)
	if out.err == nil {
		t.Fatalf("非零退出的场景必须被报告为失败，实际退出码 0:\n%s", m4RenderScenarioDiagnostics(m4ModeFailExit, out))
	}
	if out.timedOut || out.signaled {
		t.Fatalf("本用例是普通非零退出，不得归类为超时/信号死亡:\n%s", m4RenderScenarioDiagnostics(m4ModeFailExit, out))
	}
	if out.exitCode != 3 {
		t.Fatalf("退出码 = %d, want 3", out.exitCode)
	}
	if kind := m4ScenarioClassification(out); kind != m4ClassFailed {
		t.Fatalf("非零退出场景分类 = %q, want %q", kind, m4ClassFailed)
	}
	if got := strings.Count(out.stdout, m4FailExitMarker); got != 1 {
		t.Fatalf("场景标记出现 %d 次, want 恰好 1 次（不自动重试）:\n%s", got, out.stdout)
	}
	rendered := m4RenderScenarioDiagnostics(m4ModeFailExit, out)
	if !strings.Contains(rendered, m4FailExitMarker) {
		t.Fatalf("失败诊断必须保留子进程输出:\n%s", rendered)
	}
	t.Logf("非零退出场景被判定为 failed 并保留诊断:\n%s", rendered)
}

// TestM4DeviceEntrySmokeRunWithEmptyRecognitionList 以有效显式配置端
// 到端走通设备验收入口（识别清单为空 + connect/screencap 已许可——
// 缺识别流程不阻塞本入口或连接/截图验收）：子进程通过入口门与两个许
// 可门、确认空清单下任务提交被拒绝、写入去敏证据元数据并打印 OK 标
// 记；父进程独立复验 OK 标记字段、断言 stdout 与证据文件都不含设备
// 地址（写入器把原始地址去敏为占位符）。本冒烟场景不执行任何原生/设
// 备动作；Core 快照路径仅为存在性预检提供占位文件。
func TestM4DeviceEntrySmokeRunWithEmptyRecognitionList(t *testing.T) {
	if os.Getenv(m4ChildModeEnv) == m4ModeSmoke {
		runM4SmokeChild(t)
		return
	}
	dir := m4PrepareProbeTree(t)
	cfg := m4ValidProbeBaseConfig(dir) // connect/screencap=true, tasks=[]
	configPath := m4WriteConfigFile(t, dir, "smoke-config.json", &cfg)

	out := runM4DeviceScenario(t, m4SmokeChildTimeout, m4ModeSmoke, map[string]string{
		m4ConfigEnv: configPath,
	})
	if out.err != nil {
		t.Fatalf("smoke 子进程失败:\n%s", m4RenderScenarioDiagnostics(m4ModeSmoke, out))
	}
	if err := verifyM4SmokeMetadata(out.stdout); err != nil {
		t.Fatalf("smoke 子进程 stdout 元数据复验失败:\n%v\n%s", err, m4RenderScenarioDiagnostics(m4ModeSmoke, out))
	}
	if strings.Contains(out.stdout, m4FakeAddress) {
		t.Fatalf("smoke 子进程 stdout 不得包含设备地址:\n%s", out.stdout)
	}

	// 证据文件：写入器已把原始地址去敏为占位符。
	data, err := os.ReadFile(filepath.Join(cfg.OutputDir, "evidence-smoke.json"))
	if err != nil {
		t.Fatalf("读取证据元数据失败: %v", err)
	}
	var doc struct {
		Scenario string            `json:"scenario"`
		Fields   map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("证据元数据解码失败: %v (%s)", err, data)
	}
	if doc.Scenario != m4ModeSmoke {
		t.Fatalf("证据 scenario = %q, want %q", doc.Scenario, m4ModeSmoke)
	}
	if got := doc.Fields["device_address"]; got != m4RedactedAddress {
		t.Fatalf("证据 device_address = %q, want 去敏占位符 %q", got, m4RedactedAddress)
	}
	if strings.Contains(string(data), m4FakeAddress) {
		t.Fatalf("证据元数据不得包含设备地址: %s", data)
	}
	if doc.Fields["recognition_tasks"] != "0" {
		t.Fatalf("证据 recognition_tasks = %q, want \"0\"（识别清单为空）", doc.Fields["recognition_tasks"])
	}
	t.Logf("smoke 子进程走通入口门与许可门，证据已去敏:\n%s", out.stdout)
}

// runM4SmokeChild 是 m4ModeSmoke 子进程入口：经统一入口门加载有效配
// 置，通过连接/截图许可门，确认空识别清单下任何任务提交都在原生调用
// 前被拒绝，然后写入去敏证据并打印 OK 标记。本场景不执行任何原生/
// 设备动作（入口接线验收）。
func runM4SmokeChild(t *testing.T) {
	cfg := requireM4DeviceAcceptance(t)
	if err := cfg.requireConnectPermission(); err != nil {
		t.Fatalf("空识别清单的有效配置必须能通过连接许可门: %v", err)
	}
	if err := cfg.requireScreencapPermission(); err != nil {
		t.Fatalf("空识别清单的有效配置必须能通过截图许可门: %v", err)
	}
	if err := cfg.admitTask("Award", json.RawMessage(`{"enable":true}`)); err == nil {
		t.Fatalf("空识别清单下任务提交必须在原生调用前被拒绝")
	} else {
		t.Logf("空清单任务提交按预期被拒绝: %v", err)
	}
	m4WriteScenarioEvidence(t, cfg, m4ModeSmoke, map[string]string{
		"enabled":           "true",
		"connect":           strconv.FormatBool(cfg.Perms.Connect),
		"screencap":         strconv.FormatBool(cfg.Perms.Screencap),
		"recognition_tasks": strconv.Itoa(len(cfg.Perms.Tasks)),
		"device_address":    cfg.Device.Address, // 由写入器统一去敏
	})
	fmt.Printf("%s enabled=true connect=%v screencap=%v tasks=%d address=%s evidence=written\n",
		m4SmokeOKMarker, cfg.Perms.Connect, cfg.Perms.Screencap, len(cfg.Perms.Tasks), m4RedactedAddress)
}

// verifyM4SmokeMetadata 对 smoke 子进程的 stdout 做父侧复验：解析 OK
// 标记行的全部动态字段并核对（缺行、格式异常或任一值不符都判失败）。
func verifyM4SmokeMetadata(stdout string) error {
	line, ok := m4FindMarkerLine(stdout, m4SmokeOKMarker)
	if !ok {
		return fmt.Errorf("stdout 缺少 %s 元数据行", m4SmokeOKMarker)
	}
	want := map[string]string{
		"enabled":   "true",
		"connect":   "true",
		"screencap": "true",
		"tasks":     "0",
		"address":   m4RedactedAddress,
		"evidence":  "written",
	}
	got := map[string]string{}
	for _, field := range strings.Fields(line)[1:] {
		if k, v, found := strings.Cut(field, "="); found {
			got[k] = v
		}
	}
	for k, v := range want {
		if got[k] != v {
			return fmt.Errorf("OK 标记字段 %s = %q, want %q (line: %s)", k, got[k], v, line)
		}
	}
	return nil
}

// m4PrepareProbeTree 构造预检用目录树：adb 占位可执行路径（存在、非
// 目录）、存在但非 ELF 的占位库（任何过早原生加载都会大声失败）、
// resource/user/out 三个目录。返回根目录。
func m4PrepareProbeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m4TouchFile(t, filepath.Join(dir, "adb"))
	m4TouchFile(t, filepath.Join(dir, "libMaaCore.so.placeholder"))
	for _, d := range []string{"resource", "user", "out"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatalf("创建预检目录 %s 失败: %v", d, err)
		}
	}
	return dir
}

// m4TouchFile 创建一个空的普通占位文件。
func m4TouchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("placeholder\n"), 0o644); err != nil {
		t.Fatalf("创建占位文件 %s 失败: %v", path, err)
	}
}

// m4ValidProbeBaseConfig 返回一份通过全部前置检查的基础配置（占位路
// 径 + 回环示例地址；真实执行者配置在仓库外，绝不提交）。
func m4ValidProbeBaseConfig(dir string) deviceAcceptanceConfig {
	return deviceAcceptanceConfig{
		Enable: true,
		Device: m4DeviceSelection{
			ADBPath: filepath.Join(dir, "adb"),
			Address: m4FakeAddress,
			Config:  "",
		},
		Core: m4CoreSnapshot{
			LibraryPath:  filepath.Join(dir, "libMaaCore.so.placeholder"),
			ResourceRoot: filepath.Join(dir, "resource"),
			UserDir:      filepath.Join(dir, "user"),
		},
		OutputDir: filepath.Join(dir, "out"),
		Perms: m4Permissions{
			Connect:   true,
			Screencap: true,
			Tasks:     []m4TaskAllowance{},
		},
	}
}

// m4WriteConfigFile 把配置序列化写入 dir 下的 name 文件并返回路径。
func m4WriteConfigFile(t *testing.T, dir, name string, cfg *deviceAcceptanceConfig) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("配置序列化失败: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写入配置 %s 失败: %v", path, err)
	}
	return path
}
