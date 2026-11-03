package main

import (
	"fmt"
	"strings"
)

const (
	CatInput     = "input"
	CatCSyntax   = "c-syntax"
	CatGoSyntax  = "go-syntax"
	CatRelation  = "relation"
	CatPolicy    = "policy"
	CatSignature = "signature"
)

type Diagnostic struct {
	Category string
	File     string
	Line     int
	Symbol   string
	Expected string
	Actual   string
	Message  string
}

func (d Diagnostic) Error() string { return d.String() }

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
