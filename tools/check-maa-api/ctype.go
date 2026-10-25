// ctype.go 定义规范化 C 类型模型:保留宽度、符号位、const、指针
// 层级与 pointee 语义,不以机器字长合并。至少区分 void、uint8、
// int32、uint64、char、const char*、char*、void*、指向具名 struct
// 的指针(opaque handle)、int32*(任务列表)与函数指针类型。经已知
// 别名展开时在 Via 保留使用处的原名(如 AsstBool→uint8、
// AsstTaskId→int32)。

package main

import "strings"

// CTypeKind 区分 void、标量、指针、函数指针与具名结构体。
type CTypeKind int

const (
	CKindVoid CTypeKind = iota
	CKindScalar
	CKindPointer
	CKindFuncPtr
	CKindStruct
)

// CType 是规范化类型节点:
//   - CKindVoid:无载荷;
//   - CKindScalar:Name/Bits/Signed;
//   - CKindPointer:Pointee;
//   - CKindFuncPtr:Sig;
//   - CKindStruct:Tag。
//
// Const 记录该层的 const 限定;对指针而言 const 属于 pointee 层。
// Via 记录使用处的外层别名原名,直接拼写为空。
type CType struct {
	Kind    CTypeKind
	Name    string // 标量规范名:uint8/int32/uint64/char
	Bits    int    // 标量位宽:8/32/64
	Signed  bool   // 标量符号位;char 单独命名,不与 uint8 合并
	Const   bool   // 该层的 const 限定
	Pointee *CType
	Sig     *CFuncSig
	Tag     string // 结构体 tag
	Via     string // 外层别名原名
}

// CFuncSig 是函数指针签名;CallConv 为展开后的调用约定。
type CFuncSig struct {
	Ret      CType
	Params   []CParam
	CallConv string
}

// CParam 是具名参数:类型加名称,禁止匿名参数。
type CParam struct {
	Name string
	Type CType
}

// Equal 深比较两个规范化类型(含 Via 与指针层级)。
func (t CType) Equal(o CType) bool {
	if t.Kind != o.Kind || t.Name != o.Name || t.Bits != o.Bits ||
		t.Signed != o.Signed || t.Const != o.Const || t.Tag != o.Tag || t.Via != o.Via {
		return false
	}
	if (t.Pointee == nil) != (o.Pointee == nil) {
		return false
	}
	if t.Pointee != nil && !t.Pointee.Equal(*o.Pointee) {
		return false
	}
	if (t.Sig == nil) != (o.Sig == nil) {
		return false
	}
	if t.Sig != nil && !t.Sig.Equal(*o.Sig) {
		return false
	}
	return true
}

// Equal 深比较两个函数指针签名。
func (s CFuncSig) Equal(o CFuncSig) bool {
	if !s.Ret.Equal(o.Ret) || s.CallConv != o.CallConv || len(s.Params) != len(o.Params) {
		return false
	}
	for i := range s.Params {
		if !s.Params[i].Equal(o.Params[i]) {
			return false
		}
	}
	return true
}

// Equal 比较具名参数(名称与类型都必须一致)。
func (p CParam) Equal(o CParam) bool { return p.Name == o.Name && p.Type.Equal(o.Type) }

// String 输出可读拼写,用于诊断与测试断言。
func (t CType) String() string {
	switch t.Kind {
	case CKindVoid:
		return "void"
	case CKindScalar:
		s := t.Name
		if t.Const {
			s = "const " + s
		}
		return s
	case CKindStruct:
		return "struct " + t.Tag
	case CKindPointer:
		return t.Pointee.String() + "*"
	case CKindFuncPtr:
		var b strings.Builder
		b.WriteString(t.Sig.Ret.String())
		b.WriteString("(")
		if t.Sig.CallConv != "" {
			b.WriteString(t.Sig.CallConv)
			b.WriteString(" ")
		}
		b.WriteString("*)(")
		for i, p := range t.Sig.Params {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(p.Type.String())
			b.WriteString(" ")
			b.WriteString(p.Name)
		}
		b.WriteString(")")
		return b.String()
	}
	return "?"
}

// scalarCType 把 <stdint.h> 的固定映射与 char 构造为规范化标量;
// 只认 uint8_t/int32_t/uint64_t 与 char,不使用 host 的 int/long/
// size_t 代换。char 的符号位是实现定义,此处以单独命名保持其与
// uint8 的语义区分。
func scalarCType(spell string) (CType, bool) {
	switch spell {
	case "uint8_t":
		return CType{Kind: CKindScalar, Name: "uint8", Bits: 8}, true
	case "int32_t":
		return CType{Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true}, true
	case "uint64_t":
		return CType{Kind: CKindScalar, Name: "uint64", Bits: 64}, true
	case "char":
		return CType{Kind: CKindScalar, Name: "char", Bits: 8}, true
	}
	return CType{}, false
}
