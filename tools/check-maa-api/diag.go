// diag.go 定义 checker 的统一诊断模型:输入校验、C 语法、Go 声明
// 提取、C↔Go 关联与锁定策略期望比对失败都以 Diagnostic 结构表达,
// 类别用字符串常量导出,后续阶段按同一模式扩展。任何失败最终以非
// 零退出码结束,失败路径不得输出 PASS。

package main

import (
	"fmt"
	"strings"
)

// 诊断类别:input(命令参数/清单/文件/摘要/目标/profile/文件选择)、
// c-syntax(C 语法/宏/条件/typedef)、go-syntax(Go 解析/声明形式/
// 类型词表/遮蔽)、relation(C↔Go 一对一关联失败)、policy(锁定
// profile 独立 R/O 策略期望比对失败)、signature(宽度级 C↔Go 逐
// 参数语义比对与 callback 平台适配失败)。
const (
	CatInput     = "input"
	CatCSyntax   = "c-syntax"
	CatGoSyntax  = "go-syntax"
	CatRelation  = "relation"
	CatPolicy    = "policy"
	CatSignature = "signature"
)

// Diagnostic 是单条失败诊断:Category/File/Line 定位来源,Symbol 指出
// 符号或字段,Expected/Actual 给出期望与实际,Message 补充说明。文件级
// 问题(清单、摘要、缺失文件)允许 Line 为 0。
type Diagnostic struct {
	Category string
	File     string
	Line     int
	Symbol   string
	Expected string
	Actual   string
	Message  string
}

// Error 实现 error 接口,便于把单条诊断当作错误传递。
func (d Diagnostic) Error() string { return d.String() }

// String 输出单行格式,必须含类别、文件:行号、符号与期望/实际;
// profile 与目标信息由命令入口在诊断之外统一输出。
func (d Diagnostic) String() string {
	var b strings.Builder
	b.WriteString("check-maa-api: ")
	b.WriteString(d.Category)
	if d.File != "" {
		b.WriteString(": ")
		b.WriteString(d.File)
		if d.Line > 0 {
			fmt.Fprintf(&b, ":%d", d.Line)
		}
	}
	if d.Symbol != "" {
		b.WriteString(": ")
		b.WriteString(d.Symbol)
	}
	if d.Expected != "" || d.Actual != "" {
		fmt.Fprintf(&b, ": expected %s, actual %s", quoteEmpty(d.Expected), quoteEmpty(d.Actual))
	}
	if d.Message != "" {
		b.WriteString(": ")
		b.WriteString(d.Message)
	}
	return b.String()
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}
