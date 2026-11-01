// 本文件是共享复制辅助 cString 编码契约的用例:验证空串编码为单字节
// NUL 的空 C 字符串、非空串逐字节加 NUL,以及独立 buffer 所有权。

package maa

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCStringEncodesEmptyStringAsEmptyCString 验证:cString("") 返回
// 非 nil 首地址与恰为单字节 0x00 的底层 buffer——传空 C 字符串
// 而不是 nil(空字符串与空指针不默认等价;:
// ConnectConfig.Config、option value 允许空串并原样传空 C 字符串)。
func TestCStringEncodesEmptyStringAsEmptyCString(t *testing.T) {
	p, buf := cString("")
	require.NotNil(t, p, `cString("") must return a non-nil pointer (an empty C string, not nil)`)
	require.Equal(t, []byte{0x00}, buf, `cString("") underlying buffer = %#v, want exactly a single byte 0x00`, buf)
	assert.Equal(t, byte(0x00), *p, "*p = %#x, want 0x00 (the pointer targets the NUL at the buffer's first byte)", *p)
}

// TestCStringAppendsNULTerminator 验证:非空串编码为原文逐字节加
// NUL 终止的独立 buffer,返回的指针指向 buffer 首字节。
func TestCStringAppendsNULTerminator(t *testing.T) {
	p, buf := cString("abc")
	require.NotNil(t, p, "cString must return a non-nil pointer")
	got := string(buf)
	require.Equal(t, "abc\x00", got, `cString("abc") underlying buffer = %q, want "abc\x00"`, got)
	assert.Same(t, &buf[0], p, "the returned pointer must target the first byte of the underlying buffer")
}
