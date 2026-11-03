package native

type symbolPolicy uint8

const (
	required symbolPolicy = iota
	optional
)

type symbolPlatform uint8

const (
	allPlatforms symbolPlatform = iota
	windowsOnly
)

type symbolSpec struct {
	Name     string
	Target   any
	Policy   symbolPolicy
	Platform symbolPlatform
}

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

type exclusionSpec struct {
	Name   string
	Header string
	Reason string
}

var exclusions = []exclusionSpec{
	{Name: "AsstConnect", Header: "AsstCaller.h", Reason: "upstream deprecated comment; synchronous connect replaced by AsyncConnect"},
	{Name: "AsstAttachWindow", Header: "AsstCaller.h", Reason: "upstream deprecated comment; synchronous attach replaced by AsyncAttachWindow"},
}
