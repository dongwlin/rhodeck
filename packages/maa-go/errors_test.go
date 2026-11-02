package maa

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.Len(t, sentinels, 11, "sentinel count = %d, want 11", len(sentinels))
	for i, a := range sentinels {
		require.Error(t, a.err, "%s is nil", a.name)
		for j, b := range sentinels {
			if i != j {
				assert.False(t, a.err == b.err, "%s and %s are the same error value", a.name, b.name)
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
	require.ErrorAs(t, wrapped, &lle, "errors.As failed to find *LibraryLoadError")
	assert.True(t, lle.LibraryName == "MaaCore" && lle.Path == "/opt/maa/bin/libMaaCore.so" &&
		lle.GOOS == "linux" && lle.GOARCH == "amd64", "diagnostic fields mismatch: %+v", lle)
	assert.ErrorIs(t, wrapped, cause, "errors.Is failed to reach underlying loader cause")
	var got *stubCause
	assert.True(t, errors.As(wrapped, &got) && got == cause, "errors.As failed to reach underlying cause value")
	for _, want := range []string{"MaaCore", "/opt/maa/bin/libMaaCore.so", "linux", "amd64"} {
		assert.Contains(t, err.Error(), want, "Error() = %q, want it to contain %q", err.Error(), want)
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
	require.ErrorAs(t, wrapped, &sle, "errors.As failed to find *SymbolLookupError")
	assert.True(t, sle.Symbol == "AsstCreateEx" && sle.LibraryPath == "/opt/maa/bin/libMaaCore.so" &&
		sle.Profile == "linux-amd64-maacore-locked", "diagnostic fields mismatch: %+v", sle)
	assert.ErrorIs(t, wrapped, cause, "errors.Is failed to reach underlying lookup cause")
	for _, want := range []string{"AsstCreateEx", "/opt/maa/bin/libMaaCore.so", "linux-amd64-maacore-locked"} {
		assert.Contains(t, err.Error(), want, "Error() = %q, want it to contain %q", err.Error(), want)
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
	require.ErrorAs(t, wrapped, &fre, "errors.As failed to find *FunctionRegistrationError")
	assert.True(t, fre.Symbol == "AsstSetTaskParam" &&
		fre.Signature == "func(callback uintptr, customArg uintptr) uintptr", "diagnostic fields mismatch: %+v", fre)
	assert.ErrorIs(t, wrapped, panicErr, "errors.Is failed to reach the retained panic error")
	var cause *stubCause
	assert.True(t, errors.As(wrapped, &cause) && cause == panicErr, "errors.As failed to reach the retained panic error value")
	// 注册错误不得伪装成缺符号错误。
	var sle *SymbolLookupError
	assert.NotErrorAs(t, wrapped, &sle, "FunctionRegistrationError must not be identifiable as *SymbolLookupError")
	for _, want := range []string{"AsstSetTaskParam", "func(callback uintptr, customArg uintptr) uintptr"} {
		assert.Contains(t, err.Error(), want, "Error() = %q, want it to contain %q", err.Error(), want)
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
	require.ErrorAs(t, err, &got, "errors.As failed to find AggregateError")
	symbols := map[string]bool{}
	for _, member := range got {
		var sle *SymbolLookupError
		if errors.As(member, &sle) {
			symbols[sle.Symbol] = true
		}
	}
	assert.True(t, symbols["AsstCreateEx"] && symbols["AsstAppendTask"], "range traversal missed symbols, got %v", symbols)

	// 经 Unwrap 取出成员切片(errors.Is/errors.As 内部同规则),
	// 逐项 errors.As 也能分别提取每个 SymbolLookupError。
	members, ok := errors.Unwrap(err).(interface{ Unwrap() []error })
	require.True(t, ok, "wrapped error does not expose Unwrap() []error")
	extracted := map[string]bool{}
	for _, member := range members.Unwrap() {
		var sle *SymbolLookupError
		if errors.As(member, &sle) {
			extracted[sle.Symbol] = true
		}
	}
	assert.True(t, len(extracted) == 2 && extracted["AsstCreateEx"] && extracted["AsstAppendTask"], "Unwrap traversal extracted %v, want both symbols", extracted)

	// errors.Is 穿透聚合,匹配其中一个成员包装的底层原因。
	assert.ErrorIs(t, err, causeB, "errors.Is failed to reach cause wrapped inside aggregate member")

	// 多项文本逐项编号且包含各项内容。
	text := agg.Error()
	for i, want := range []string{"[1]", "[2]", "AsstCreateEx", "AsstAppendTask"} {
		assert.Contains(t, text, want, "AggregateError.Error() = %q, want it to contain %q (item %d)", text, want, i)
	}
}

func TestAggregateErrorEmpty(t *testing.T) {
	var agg AggregateError
	got := agg.Error()
	assert.True(t, strings.TrimSpace(got) != "", "empty AggregateError.Error() = %q, want a non-empty placeholder", got)
	// 空聚合对任意目标都不匹配,且不会 panic。
	assert.NotErrorIs(t, agg, ErrClosed, "empty aggregate must not match any sentinel")
}
