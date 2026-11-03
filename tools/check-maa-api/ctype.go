package main

import "strings"

type CTypeKind int

const (
	CKindVoid CTypeKind = iota
	CKindScalar
	CKindPointer
	CKindFuncPtr
	CKindStruct
)

type CType struct {
	Kind    CTypeKind
	Name    string
	Bits    int
	Signed  bool
	Const   bool
	Pointee *CType
	Sig     *CFuncSig
	Tag     string
	Via     string
}

type CFuncSig struct {
	Ret      CType
	Params   []CParam
	CallConv string
}

type CParam struct {
	Name string
	Type CType
}

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

func (p CParam) Equal(o CParam) bool { return p.Name == o.Name && p.Type.Equal(o.Type) }

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
