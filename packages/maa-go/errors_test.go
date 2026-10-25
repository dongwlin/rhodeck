package maa

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// stubCause 模拟错误链最深处的自定义底层原因
// (例如系统 loader 文本、注册期 panic 携带的 error)。
type stubCause struct {
	msg string
}

func (e *stubCause) Error() string { return e.msg }

func allSentinels() []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"ErrClosed", ErrClosed},
		{"ErrClosing", ErrClosing},
		{"ErrRuntimeInUse", ErrRuntimeInUse},
		{"ErrRuntimeTerminated", ErrRuntimeTerminated},
		{"ErrUnsupported", ErrUnsupported},
		{"ErrInvalidInput", ErrInvalidInput},
		{"ErrNativeRejected", ErrNativeRejected},
		{"ErrBufferLimit", ErrBufferLimit},
		{"ErrEventOverflow", ErrEventOverflow},
		{"ErrOperationLimit", ErrOperationLimit},
		{"ErrProtocol", ErrProtocol},
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	sentinels := allSentinels()
	if len(sentinels) != 11 {
		t.Fatalf("sentinel count = %d, want 11", len(sentinels))
	}
	for i, a := range sentinels {
		if a.err == nil {
			t.Fatalf("%s is nil", a.name)
		}
		for j, b := range sentinels {
			if i != j && a.err == b.err {
				t.Errorf("%s and %s are the same error value", a.name, b.name)
			}
		}
	}
}

func TestSentinelErrorsMatchThroughWrappedChain(t *testing.T) {
	sentinels := allSentinels()
	for _, s := range sentinels {
		wrapped := fmt.Errorf("open runtime: %w", fmt.Errorf("inner step: %w", s.err))
		if !errors.Is(wrapped, s.err) {
			t.Errorf("errors.Is(wrapped, %s) = false, want true", s.name)
		}
	}
	// 任意两个哨兵互不匹配。
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i != j && errors.Is(a.err, b.err) {
				t.Errorf("errors.Is(%s, %s) = true, want false", a.name, b.name)
			}
		}
	}
}

func TestLibraryLoadError(t *testing.T) {
	cause := &stubCause{msg: "libMaaTools.so: cannot open shared object file"}
	err := &LibraryLoadError{
		LibraryName: "MaaCore",
		Path:        "/opt/maa/bin/libMaaCore.so",
		GOOS:        "linux",
		GOARCH:      "amd64",
		Err:         cause,
	}
	wrapped := fmt.Errorf("open runtime: %w", err)

	var lle *LibraryLoadError
	if !errors.As(wrapped, &lle) {
		t.Fatal("errors.As failed to find *LibraryLoadError")
	}
	if lle.LibraryName != "MaaCore" || lle.Path != "/opt/maa/bin/libMaaCore.so" ||
		lle.GOOS != "linux" || lle.GOARCH != "amd64" {
		t.Errorf("diagnostic fields mismatch: %+v", lle)
	}
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is failed to reach underlying loader cause")
	}
	var got *stubCause
	if !errors.As(wrapped, &got) || got != cause {
		t.Error("errors.As failed to reach underlying cause value")
	}
	for _, want := range []string{"MaaCore", "/opt/maa/bin/libMaaCore.so", "linux", "amd64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestSymbolLookupError(t *testing.T) {
	cause := &stubCause{msg: "lookup failed"}
	err := &SymbolLookupError{
		LibraryPath: "/opt/maa/bin/libMaaCore.so",
		Symbol:      "AsstCreateEx",
		Profile:     "linux-amd64-maacore-locked",
		Err:         cause,
	}
	wrapped := fmt.Errorf("load transaction: %w", err)

	var sle *SymbolLookupError
	if !errors.As(wrapped, &sle) {
		t.Fatal("errors.As failed to find *SymbolLookupError")
	}
	if sle.Symbol != "AsstCreateEx" || sle.LibraryPath != "/opt/maa/bin/libMaaCore.so" ||
		sle.Profile != "linux-amd64-maacore-locked" {
		t.Errorf("diagnostic fields mismatch: %+v", sle)
	}
	if !errors.Is(wrapped, cause) {
		t.Error("errors.Is failed to reach underlying lookup cause")
	}
	for _, want := range []string{"AsstCreateEx", "/opt/maa/bin/libMaaCore.so", "linux-amd64-maacore-locked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestFunctionRegistrationError(t *testing.T) {
	// 模拟注册边界 recover 到的 panic 值本身是 error 的情况:
	// 该 error 直接作为 Err 保留,错误链上必须仍可识别。
	panicErr := &stubCause{msg: "panic during registration"}
	err := &FunctionRegistrationError{
		Symbol:    "AsstSetTaskParam",
		Signature: "func(callback uintptr, customArg uintptr) uintptr",
		Err:       panicErr,
	}
	wrapped := fmt.Errorf("load transaction: %w", err)

	var fre *FunctionRegistrationError
	if !errors.As(wrapped, &fre) {
		t.Fatal("errors.As failed to find *FunctionRegistrationError")
	}
	if fre.Symbol != "AsstSetTaskParam" ||
		fre.Signature != "func(callback uintptr, customArg uintptr) uintptr" {
		t.Errorf("diagnostic fields mismatch: %+v", fre)
	}
	if !errors.Is(wrapped, panicErr) {
		t.Error("errors.Is failed to reach the retained panic error")
	}
	var cause *stubCause
	if !errors.As(wrapped, &cause) || cause != panicErr {
		t.Error("errors.As failed to reach the retained panic error value")
	}
	// 注册错误不得伪装成缺符号错误。
	var sle *SymbolLookupError
	if errors.As(wrapped, &sle) {
		t.Error("FunctionRegistrationError must not be identifiable as *SymbolLookupError")
	}
	for _, want := range []string{"AsstSetTaskParam", "func(callback uintptr, customArg uintptr) uintptr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestAggregateErrorTraversal(t *testing.T) {
	causeB := &stubCause{msg: "symbol table truncated"}
	symA := &SymbolLookupError{
		LibraryPath: "/opt/maa/bin/libMaaCore.so",
		Symbol:      "AsstCreateEx",
		Profile:     "linux-amd64-maacore-locked",
		Err:         &stubCause{msg: "not found"},
	}
	symB := &SymbolLookupError{
		LibraryPath: "/opt/maa/bin/libMaaCore.so",
		Symbol:      "AsstAppendTask",
		Profile:     "linux-amd64-maacore-locked",
		Err:         fmt.Errorf("dlsym: %w", causeB),
	}
	agg := AggregateError{symA, symB}
	// 再包一层,证明展开规则能穿透外层包装。
	err := fmt.Errorf("load transaction: %w", agg)

	// errors.As 提取聚合本体后,逐项 range 遍历,
	// 分别拿到两个符号名——全程不解析错误字符串。
	var got AggregateError
	if !errors.As(err, &got) {
		t.Fatal("errors.As failed to find AggregateError")
	}
	symbols := map[string]bool{}
	for _, member := range got {
		var sle *SymbolLookupError
		if errors.As(member, &sle) {
			symbols[sle.Symbol] = true
		}
	}
	if !symbols["AsstCreateEx"] || !symbols["AsstAppendTask"] {
		t.Errorf("range traversal missed symbols, got %v", symbols)
	}

	// 经 Unwrap 取出成员切片(errors.Is/errors.As 内部同规则),
	// 逐项 errors.As 也能分别提取每个 SymbolLookupError。
	members, ok := errors.Unwrap(err).(interface{ Unwrap() []error })
	if !ok {
		t.Fatal("wrapped error does not expose Unwrap() []error")
	}
	extracted := map[string]bool{}
	for _, member := range members.Unwrap() {
		var sle *SymbolLookupError
		if errors.As(member, &sle) {
			extracted[sle.Symbol] = true
		}
	}
	if len(extracted) != 2 || !extracted["AsstCreateEx"] || !extracted["AsstAppendTask"] {
		t.Errorf("Unwrap traversal extracted %v, want both symbols", extracted)
	}

	// errors.Is 穿透聚合,匹配其中一个成员包装的底层原因。
	if !errors.Is(err, causeB) {
		t.Error("errors.Is failed to reach cause wrapped inside aggregate member")
	}

	// 多项文本逐项编号且包含各项内容。
	text := agg.Error()
	for i, want := range []string{"[1]", "[2]", "AsstCreateEx", "AsstAppendTask"} {
		if !strings.Contains(text, want) {
			t.Errorf("AggregateError.Error() = %q, want it to contain %q (item %d)", text, want, i)
		}
	}
}

func TestAggregateErrorEmpty(t *testing.T) {
	var agg AggregateError
	if got := agg.Error(); strings.TrimSpace(got) == "" {
		t.Errorf("empty AggregateError.Error() = %q, want a non-empty placeholder", got)
	}
	// 空聚合对任意目标都不匹配,且不会 panic。
	if errors.Is(agg, ErrClosed) {
		t.Error("empty aggregate must not match any sentinel")
	}
}

func TestIDTypesAreDistinctInt32(t *testing.T) {
	taskType := reflect.TypeOf(TaskID(0))
	asyncType := reflect.TypeOf(AsyncID(0))
	msgType := reflect.TypeOf(MessageID(0))

	if taskType == asyncType || taskType == msgType || asyncType == msgType {
		t.Fatal("TaskID, AsyncID and MessageID must be distinct named types")
	}
	for _, tt := range []reflect.Type{taskType, asyncType, msgType} {
		if tt.Kind() != reflect.Int32 {
			t.Errorf("%s kind = %v, want int32", tt, tt.Kind())
		}
	}

	// 显式转换合法且保值;不同类型之间不能隐式混用由编译期保证,
	// 运行期以类型差异断言覆盖。
	tid := TaskID(42)
	if AsyncID(tid) != AsyncID(42) || MessageID(tid) != MessageID(42) {
		t.Error("explicit conversion between ID types should preserve the value")
	}
	if TaskID(0) != 0 || AsyncID(0) != 0 {
		t.Error("zero (documented failure value) must be representable")
	}
}
