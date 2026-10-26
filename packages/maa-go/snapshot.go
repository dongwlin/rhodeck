// 本文件是公开层共享的输入校验与复制辅助:与 native 无关,供
// Runtime 配置(Open)、实例配置与连接/任务输入以及后续 raw 调用
// 前的 C 字符串准备共用,不维护两套校验或快照逻辑。
//
// 单位、默认值与校验语义由 MAA-03 冻结(绑定设计 §5.1);快照
// 所有权边界见 §6、§6.1:调用者到方法返回前不得并发修改输入,
// 返回后可修改原对象而不影响已冻结快照,输出副本可长期持有、修改。
// 校验失败一律返回带字段路径、包装 native 哨兵(ErrInvalidInput)
// 的错误,发生在触碰任何生命周期门或原生调用之前。

package maa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// addChecked 返回 a+b 与「是否未溢出」:预算累计必须用检查溢出的
// 加法,溢出时不产出伪造的部分和。
func addChecked(a, b uint64) (uint64, bool) {
	sum := a + b
	if sum < a {
		return 0, false
	}
	return sum, true
}

// validateStringField 校验单个字符串输入字段:合法 UTF-8、无内嵌
// NUL,且长度不超过单字符串预算;allowEmpty 决定是否允许空串——
// 路径与 extras name 必须非空,option value 允许空串(空值编码为
// 单个 NUL 字节的空 C 字符串,不传 nil)。field 是错误文本中的字段
// 路径(如 LibraryPath、StaticOptions[0].Value)。
func validateStringField(field, value string, max uint64, allowEmpty bool) error {
	switch {
	case value == "" && !allowEmpty:
		return fmt.Errorf("maa: %s must not be empty: %w", field, native.ErrInvalidInput)
	case !utf8.ValidString(value):
		return fmt.Errorf("maa: %s is not valid UTF-8: %w", field, native.ErrInvalidInput)
	case strings.IndexByte(value, '\x00') >= 0:
		return fmt.Errorf("maa: %s contains an embedded NUL byte: %w", field, native.ErrInvalidInput)
	case uint64(len(value)) > max:
		return fmt.Errorf("maa: %s length %d exceeds Limits.InputStringBytes %d: %w", field, len(value), max, native.ErrInvalidInput)
	}
	return nil
}

// validatePathField 校验单个路径输入字段:在非空字符串校验之上追加
// 绝对路径要求。路径不做 shell 展开、不做存在性检查——文件是否
// 存在、原生是否接受,留给加载/调用阶段失败。
func validatePathField(field, value string, max uint64) error {
	if err := validateStringField(field, value, max, false); err != nil {
		return err
	}
	if !filepath.IsAbs(value) {
		return fmt.Errorf("maa: %s %q is not an absolute path: %w", field, value, native.ErrInvalidInput)
	}
	return nil
}

// validateJSONObject 校验一份必须为「非空、完整 JSON object」的
// 原始 JSON 输入(连接 extras、任务 params 等共用;绑定设计 §5.1):
//  1. 实际 NUL 字节(0x00)直接拒绝;字符串内的 \u0000 转义合法、
//     原样保留,不解码成 C 字符串输入;
//  2. 全文必须是合法 UTF-8;
//  3. 长度不超过单份 JSON 预算;
//  4. 用 json.Decoder 确认恰好一个完整 JSON object:先看首 token,
//     再走 token 计深,nil/null/数组/标量/空白/截断都拒绝,对象
//     之后的尾随第二个值也拒绝;`{}` 允许。
//
// 原文透传:全文只读,不重编码、不经 map[string]any 往返,超过
// 2^53 的整数按原文字节保留;对 object 只做语法和上述输入安全
// 校验,不丢弃未知字段。
func validateJSONObject(field string, raw json.RawMessage, max uint64) error {
	switch {
	case bytes.IndexByte(raw, 0x00) >= 0:
		return fmt.Errorf("maa: %s contains a literal NUL byte: %w", field, native.ErrInvalidInput)
	case !utf8.Valid(raw):
		return fmt.Errorf("maa: %s is not valid UTF-8: %w", field, native.ErrInvalidInput)
	case uint64(len(raw)) > max:
		return fmt.Errorf("maa: %s length %d exceeds Limits.InputJSONBytes %d: %w", field, len(raw), max, native.ErrInvalidInput)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 只做形状检查,不取任何值;UseNumber 使数字保持为 json.Number,
	// 不经 float64 往返。
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("maa: %s is not valid JSON (%v): %w", field, err, native.ErrInvalidInput)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("maa: %s must be a JSON object, got %v: %w", field, tok, native.ErrInvalidInput)
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("maa: %s is not a complete JSON object (%v): %w", field, err, native.ErrInvalidInput)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("maa: %s has trailing content after the JSON object: %w", field, native.ErrInvalidInput)
	}
	return nil
}

// cString 把 Go 字符串编码为带 NUL 终止的独立 Go buffer,返回首
// 地址与底层 buffer。编码即拷贝:输入是已通过校验(非空、合法
// UTF-8、无内嵌 NUL)的快照内容;buffer 在 raw 调用期间经
// runtime.KeepAlive 保活,调用期间不得被并发修改。空串编码为单个
// NUL 字节,传空 C 字符串而不是 nil。
func cString(s string) (*byte, []byte) {
	buf := make([]byte, len(s)+1)
	copy(buf, s)
	return &buf[0], buf
}
