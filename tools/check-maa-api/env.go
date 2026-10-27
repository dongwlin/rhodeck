// env.go 冻结静态目标与宏环境:只接受 (linux, amd64) 与
// (windows, amd64),未知组合失败;宏取值按冻结,
// 不接受 -D,也不从 host 环境继承。ASST_WITH_EXTRA_CALLERS 运行
// profile 取 0,另以 1 做实验导出发现。这些宏是解析用的确定值,
// 不声称 Windows 编译器实际定义 __x86_64__ 或对应 GNU 版本。

package main

// Target 是静态检查目标 GOOS/GOARCH 组合。
type Target struct {
	GOOS   string
	GOARCH string
}

// validateTarget 校验目标组合;未知组合返回 input 类诊断。
func validateTarget(t Target) *Diagnostic {
	switch {
	case t.GOOS == "linux" && t.GOARCH == "amd64",
		t.GOOS == "windows" && t.GOARCH == "amd64":
		return nil
	}
	return &Diagnostic{
		Category: CatInput,
		Symbol:   "target",
		Expected: "(linux, amd64) or (windows, amd64)",
		Actual:   "(" + t.GOOS + ", " + t.GOARCH + ")",
		Message:  "unsupported static target",
	}
}

// ValidateTarget 供命令入口在解析参数后调用;失败返回诊断切片。
func ValidateTarget(t Target) []Diagnostic {
	if d := validateTarget(t); d != nil {
		return []Diagnostic{*d}
	}
	return nil
}

// macroEnv 是条件求值用的确定宏环境:defined 记录对象式定义,ints
// 记录带整数值的宏;未列出的宏一律未定义。
type macroEnv struct {
	defined map[string]bool
	ints    map[string]int64
}

func (e macroEnv) isDefined(name string) bool {
	if e.defined[name] {
		return true
	}
	_, ok := e.ints[name]
	return ok
}

func (e macroEnv) intValue(name string) int64 { return e.ints[name] }

// newMacroEnv 按目标构建冻结宏环境:两个目标都定义 __x86_64__;
// linux 的 __GNUC__ 规范化为 4,windows 定义 _WIN32 且 __GNUC__
// 规范化为 0;__WINE__/__CYGWIN__/ASST_CALL/ASST_DLL_EXPORTS/
// __cplusplus 在进入 AsstPort.h 前均未定义;extraCallers 只允许 0/1。
func newMacroEnv(t Target, extraCallers int64) macroEnv {
	e := macroEnv{defined: map[string]bool{}, ints: map[string]int64{}}
	e.defined["__x86_64__"] = true
	switch t.GOOS {
	case "linux":
		e.ints["__GNUC__"] = 4
	case "windows":
		e.defined["_WIN32"] = true
		e.ints["__GNUC__"] = 0
	}
	e.ints["ASST_WITH_EXTRA_CALLERS"] = extraCallers
	return e
}
