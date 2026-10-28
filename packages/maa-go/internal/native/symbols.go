// 本文件的符号表是加载策略的唯一清单:symbolSpecs 把每项 C 符号名、api
// 字段目标、R/O 策略与适用平台逐项关联;加载实现只能依据本清单,不做
// 运行期拼接符号名或反射猜测 ABI。
//
// 策略语义:
//   - R(required)缺失则拒绝初始化,收集全部必需项失败后一次性整体报告;
//   - O(optional)缺失只关闭对应 capability,不阻塞加载;两个
//     ASST_WITH_EXTRA_CALLERS 条件的实验导出(AsstGetMapLevelKey、
//     AsstGetItemName)按 optional 绑定——上游发行包仅在 Apple 构建
//     打开该选项,其余平台缺席时只关闭能力;
//   - D 项不查找、不注册,只能出现在 exclusions;每条排除记录来源
//     头文件与原因,不靠通配符隐藏遗漏。
//
// symbolSpecs 只有一条直接 return 的 []symbolSpec 字面量,每条目恰好四个
// 具名 key(Name/Target/Policy/Platform);禁止 helper、append、循环、条件、
// 字符串拼接、反射与间接变量目标——形式漂移由 symbols_static_test.go 在
// 构建期报错。

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

// symbolSpecs 返回首期 28 条 R/O 符号清单,条目顺序同 api 字段
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
		{Name: "AsstGetMapLevelKey", Target: &table.GetMapLevelKey, Policy: optional, Platform: allPlatforms},
		{Name: "AsstGetItemName", Target: &table.GetItemName, Policy: optional, Platform: allPlatforms},
	}
}

// exclusionSpec 记录一条精确排除:D 项不绑定,Name/Header/Reason 均
// 必填,与的排除表逐字对应。
type exclusionSpec struct {
	Name   string
	Header string
	Reason string
}

// exclusions 是唯一的排除清单,恰好 2 条 deprecated 项;不得新增
// 排除项,已有项不得改绑或失效。
var exclusions = []exclusionSpec{
	{Name: "AsstConnect", Header: "AsstCaller.h", Reason: "upstream deprecated comment; synchronous connect replaced by AsyncConnect"},
	{Name: "AsstAttachWindow", Header: "AsstCaller.h", Reason: "upstream deprecated comment; synchronous attach replaced by AsyncAttachWindow"},
}
