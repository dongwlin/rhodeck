// 本文件是共享复制辅助 cString 编码契约的用例:验证空串编码为单字节
// NUL 的空 C 字符串、非空串逐字节加 NUL,以及独立 buffer 所有权。

package maa

import (
	"testing"
)

// TestCStringEncodesEmptyStringAsEmptyCString 验证:cString("") 返回
// 非 nil 首地址与恰为单字节 0x00 的底层 buffer——传空 C 字符串
// 而不是 nil(空字符串与空指针不默认等价;:
// ConnectConfig.Config、option value 允许空串并原样传空 C 字符串)。
func TestCStringEncodesEmptyStringAsEmptyCString(t *testing.T) {
	p, buf := cString("")
	if p == nil {
		t.Fatal(`cString("") 必须返回非 nil 指针(空 C 字符串,不传 nil)`)
	}
	if len(buf) != 1 || buf[0] != 0x00 {
		t.Fatalf(`cString("") 的底层 buffer = %#v, want 恰为单字节 0x00`, buf)
	}
	if *p != 0x00 {
		t.Errorf("*p = %#x, want 0x00(指针指向 buffer 首字节的 NUL)", *p)
	}
}

// TestCStringAppendsNULTerminator 验证:非空串编码为原文逐字节加
// NUL 终止的独立 buffer,返回的指针指向 buffer 首字节。
func TestCStringAppendsNULTerminator(t *testing.T) {
	p, buf := cString("abc")
	if p == nil {
		t.Fatal("cString 必须返回非 nil 指针")
	}
	if got := string(buf); got != "abc\x00" {
		t.Fatalf(`cString("abc") 的底层 buffer = %q, want "abc\x00"`, got)
	}
	if p != &buf[0] {
		t.Errorf("返回的指针必须指向底层 buffer 首字节")
	}
}
