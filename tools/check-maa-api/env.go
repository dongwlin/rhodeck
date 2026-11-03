package main

type Target struct {
	GOOS   string
	GOARCH string
}

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

func ValidateTarget(t Target) []Diagnostic {
	if d := validateTarget(t); d != nil {
		return []Diagnostic{*d}
	}
	return nil
}

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
