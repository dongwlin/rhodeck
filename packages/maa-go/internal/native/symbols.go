// 本文件的符号表是加载策略的唯一清单:symbolSpecs 把每项 C 符号
// 名、api 字段目标、R/O 策略与适用平台逐项关联;加载实现只能依据
// 本清单,不做运行期拼接符号名或反射猜测 ABI。
//
// 策略语义(与绑定设计 §3.1 一致):
//   - R(required)缺失则拒绝初始化,收集全部必需项失败后一次性
//     整体报告;
//   - O(optional)缺失只关闭对应 capability,不阻塞加载;
//   - D 与实验项不查找、不注册,只能出现在 exclusions;每条排除
//     记录来源头文件与原因,不靠通配符隐藏遗漏。
//
// 声明形式冻结自设计 §3.1:symbolSpecs 只有一条直接 return 的
// []symbolSpec 字面量,每条目恰好四个具名 key(Name/Target/
// Policy/Platform);禁止 helper、append、循环、条件、字符串拼接、
// 反射与间接变量目标——形式漂移由 symbols_static_test.go 在构建
// 期报错。

package native

// symbolPolicy 表达符号的 R/O 策略。
type symbolPolicy uint8

const (
	required symbolPolicy = iota
	optional
)

// symbolPlatform 表达符号的适用平台;非目标平台不查找。
type symbolPlatform uint8

const (
	allPlatforms symbolPlatform = iota
	windowsOnly
)

// symbolSpec 是单条符号清单:Name 为原文 C 符号,Target 指向 api
// 字段(仅适配注册器的函数指针入参,不用于动态推断 ABI)。
type symbolSpec struct {
	Name     string
	Target   any
	Policy   symbolPolicy
	Platform symbolPlatform
}

// symbolSpecs 返回首期 26 条 R/O 符号清单,条目顺序同 api 字段
// 顺序。形式冻结:单一直接 return 字面量,每条目全部四个具名 key。
func symbolSpecs(table *api) []symbolSpec {
	return []symbolSpec{
		{Name: "AsstSetUserDir", Target: &table.SetUserDir, Policy: required, Platform: allPlatforms},
		{Name: "AsstLoadResource", Target: &table.LoadResource, Policy: required, Platform: allPlatforms},
		{Name: "AsstSetStaticOption", Target: &table.SetStaticOption, Policy: required, Platform: allPlatforms},
		{Name: "AsstCreate", Target: &table.Create, Policy: optional, Platform: allPlatforms},
		{Name: "AsstCreateEx", Target: &table.CreateEx, Policy: required, Platform: allPlatforms},
		{Name: "AsstDestroy", Target: &table.Destroy, Policy: required, Platform: allPlatforms},
		{Name: "AsstSetInstanceOption", Target: &table.SetInstanceOption, Policy: required, Platform: allPlatforms},
		{Name: "AsstAppendTask", Target: &table.AppendTask, Policy: required, Platform: allPlatforms},
		{Name: "AsstSetTaskParams", Target: &table.SetTaskParams, Policy: required, Platform: allPlatforms},
		{Name: "AsstStart", Target: &table.Start, Policy: required, Platform: allPlatforms},
		{Name: "AsstStop", Target: &table.Stop, Policy: required, Platform: allPlatforms},
		{Name: "AsstRunning", Target: &table.Running, Policy: required, Platform: allPlatforms},
		{Name: "AsstConnected", Target: &table.Connected, Policy: required, Platform: allPlatforms},
		{Name: "AsstBackToHome", Target: &table.BackToHome, Policy: required, Platform: allPlatforms},
		{Name: "AsstAsyncConnect", Target: &table.AsyncConnect, Policy: required, Platform: allPlatforms},
		{Name: "AsstSetConnectionExtras", Target: &table.SetConnectionExtras, Policy: required, Platform: allPlatforms},
		{Name: "AsstAsyncClick", Target: &table.AsyncClick, Policy: required, Platform: allPlatforms},
		{Name: "AsstAsyncScreencap", Target: &table.AsyncScreencap, Policy: required, Platform: allPlatforms},
		{Name: "AsstGetImage", Target: &table.GetImage, Policy: required, Platform: allPlatforms},
		{Name: "AsstGetImageBgr", Target: &table.GetImageBgr, Policy: optional, Platform: allPlatforms},
		{Name: "AsstGetUUID", Target: &table.GetUUID, Policy: required, Platform: allPlatforms},
		{Name: "AsstGetTasksList", Target: &table.GetTasksList, Policy: required, Platform: allPlatforms},
		{Name: "AsstGetNullSize", Target: &table.GetNullSize, Policy: required, Platform: allPlatforms},
		{Name: "AsstGetVersion", Target: &table.GetVersion, Policy: required, Platform: allPlatforms},
		{Name: "AsstLog", Target: &table.Log, Policy: required, Platform: allPlatforms},
		{Name: "AsstAsyncAttachWindow", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},
	}
}

// exclusionSpec 记录一条精确排除:D 项与实验项不绑定,Name/
// Header/Reason 均必填,与设计 §3.0 的排除表逐字对应。
type exclusionSpec struct {
	Name   string
	Header string
	Reason string
}

// exclusions 是唯一的排除清单,恰好 4 条;不得新增排除项,已有
// 项不得改绑或失效。
var exclusions = []exclusionSpec{
	{Name: "AsstConnect", Header: "AsstCaller.h", Reason: "上游 deprecated 注释；同步连接由 AsyncConnect 代替"},
	{Name: "AsstAttachWindow", Header: "AsstCaller.h", Reason: "上游 deprecated 注释；同步绑定由 AsyncAttachWindow 代替"},
	{Name: "AsstGetMapLevelKey", Header: "AsstCallerExtra.h", Reason: "实验且返回结构体，超出首期调用范围；struct AsstMapLevelKey ASSTAPI AsstGetMapLevelKey(const char* key)"},
	{Name: "AsstGetItemName", Header: "AsstCallerExtra.h", Reason: "上游明确无稳定兼容保证；const char* ASSTAPI AsstGetItemName(const char* id)"},
}
