// 本文件是声明式符号表的 AST 静态自检:钉死 api.go、symbols.go 与
// callback_types.go 的声明形式,形式漂移即失败。

package native

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedAPIFields 是手写内联的独立期望表:字段名、完整签名、
// R/O 策略与适用平台,顺序即 api 字段与 symbolSpecs 条目的冻结
// 顺序,数据来源为的逐符号矩阵,不从被检查的 Go 表
// 生成。签名文本经 canonicalizeSigText 解析后走与被检文件相同的
// 规范化器,避免手写字符串因空格差异误判。
var expectedAPIFields = []struct {
	field    string
	sig      string
	policy   string
	platform string
}{
	{"SetUserDir", "func(path *byte) uint8", "required", "allPlatforms"},
	{"LoadResource", "func(path *byte) uint8", "required", "allPlatforms"},
	{"SetStaticOption", "func(key int32, value *byte) uint8", "required", "allPlatforms"},
	{"Create", "func() uintptr", "optional", "allPlatforms"},
	{"CreateEx", "func(callback uintptr, customArg uintptr) uintptr", "required", "allPlatforms"},
	{"Destroy", "func(handle uintptr)", "required", "allPlatforms"},
	{"SetInstanceOption", "func(handle uintptr, key int32, value *byte) uint8", "required", "allPlatforms"},
	{"AppendTask", "func(handle uintptr, taskType *byte, params *byte) int32", "required", "allPlatforms"},
	{"SetTaskParams", "func(handle uintptr, id int32, params *byte) uint8", "required", "allPlatforms"},
	{"Start", "func(handle uintptr) uint8", "required", "allPlatforms"},
	{"Stop", "func(handle uintptr) uint8", "required", "allPlatforms"},
	{"Running", "func(handle uintptr) uint8", "required", "allPlatforms"},
	{"Connected", "func(handle uintptr) uint8", "required", "allPlatforms"},
	{"BackToHome", "func(handle uintptr) uint8", "required", "allPlatforms"},
	{"AsyncConnect", "func(handle uintptr, adbPath *byte, address *byte, config *byte, block uint8) int32", "required", "allPlatforms"},
	{"SetConnectionExtras", "func(name *byte, extras *byte)", "required", "allPlatforms"},
	{"AsyncClick", "func(handle uintptr, x int32, y int32, block uint8) int32", "required", "allPlatforms"},
	{"AsyncScreencap", "func(handle uintptr, block uint8) int32", "required", "allPlatforms"},
	{"GetImage", "func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64", "required", "allPlatforms"},
	{"GetImageBgr", "func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64", "optional", "allPlatforms"},
	{"GetUUID", "func(handle uintptr, buff *byte, buffSize uint64) uint64", "required", "allPlatforms"},
	{"GetTasksList", "func(handle uintptr, buff *int32, buffSize uint64) uint64", "required", "allPlatforms"},
	{"GetNullSize", "func() uint64", "required", "allPlatforms"},
	{"GetVersion", "func() *byte", "required", "allPlatforms"},
	{"Log", "func(level *byte, message *byte)", "required", "allPlatforms"},
	{"AsyncAttachWindow", "func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32", "optional", "windowsOnly"},
	{"GetMapLevelKey", "func(key *byte) mapLevelKey", "optional", "allPlatforms"},
	{"GetItemName", "func(id *byte) *byte", "optional", "allPlatforms"},
}

// expectedExclusions 是手写内联的排除期望表:恰好 2 条 deprecated
// 项,名称集合与顺序、来源头均冻结自,不得新增或改绑。
var expectedExclusions = []struct {
	name   string
	header string
}{
	{"AsstConnect", "AsstCaller.h"},
	{"AsstAttachWindow", "AsstCaller.h"},
}

// expectedCallbackSigs 是手写内联的 callback 期望签名:Unix 无
// 返回值,Windows purego 路径返回 uintptr。
var expectedCallbackSigs = []struct {
	typeName string
	sig      string
}{
	{"unixCallback", "func(msg int32, detailsJSON *byte, customArg uintptr)"},
	{"windowsCallback", "func(msg int32, detailsJSON *byte, customArg uintptr) uintptr"},
}

// expectedCapabilityFields 是 optional 符号到 Capabilities 字段的
// 手写内联映射:键集合必须与期望表中 policy 为 optional 的条目
// (Asst + 字段名)一一对应,值是能力置位 switch 写入的字段名。
// 新增 optional 符号时,期望表、本表与 register.go 的 switch 三处
// 必须同步修改,任何一处遗漏都会在交叉校验中失败。
var expectedCapabilityFields = map[string]string{
	"AsstCreate":            "create",
	"AsstGetImageBgr":       "imageBgr",
	"AsstAsyncAttachWindow": "asyncAttachWindow",
	"AsstGetMapLevelKey":    "mapLevelKey",
	"AsstGetItemName":       "itemName",
}

// staticFiles 是被静态自检的三个源文件,均不允许平台条件。
var staticFiles = []string{"api.go", "symbols.go", "callback_types.go"}

// parsePackageFile 以 ParseComments 解析包目录内名为 name 的源
// 文件。测试运行目录即包目录,以 os.Getwd 定位;解析失败立即
// Fatal,错误信息带完整路径。
func parsePackageFile(t *testing.T, name string) *ast.File {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err, "os.Getwd failed, cannot locate %s", name)
	path := filepath.Join(dir, name)
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	require.NoError(t, err, "failed to parse %s", path)
	return file
}

// isIdent 判断 expr 是否为名为 name 的标识符。
func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}

// declKeys 把顶层声明渲染为稳定标识序列:func <名>、type <名>、
// var <名>、import [别名 ]<路径>,const 块整体记为 const(<首项
// 名>)。序列按出现顺序,白名单比对逐位进行。
func declKeys(file *ast.File) []string {
	var keys []string
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			keys = append(keys, "func "+decl.Name.Name)
		case *ast.GenDecl:
			switch decl.Tok {
			case token.IMPORT:
				for _, spec := range decl.Specs {
					imp := spec.(*ast.ImportSpec)
					key := "import "
					if imp.Name != nil {
						key += imp.Name.Name + " "
					}
					keys = append(keys, key+imp.Path.Value)
				}
			case token.TYPE:
				for _, spec := range decl.Specs {
					keys = append(keys, "type "+spec.(*ast.TypeSpec).Name.Name)
				}
			case token.CONST:
				if len(decl.Specs) > 0 {
					first := decl.Specs[0].(*ast.ValueSpec).Names[0].Name
					keys = append(keys, "const("+first+")")
				}
			case token.VAR:
				for _, spec := range decl.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						keys = append(keys, "var "+name.Name)
					}
				}
			}
		}
	}
	return keys
}

// canonicalTypeExpr 把受限类型表达式渲染为规范文本:仅接受预声明
// 整型标识 uint8/int32/uint64/uintptr/byte、包内 mapLevelKey 同构名、
// 指针、unsafe.Pointer 选择器与函数类型;自定义别名、伪造 unsafe、
// 数组、map、变参等其他任何表达式一律 Fatal——raw 声明的类型白名单
// 由此钉死。
func canonicalTypeExpr(t *testing.T, expr ast.Expr) string {
	t.Helper()
	switch e := expr.(type) {
	case *ast.Ident:
		switch e.Name {
		case "uint8", "int32", "uint64", "uintptr", "byte":
			return e.Name
		case "mapLevelKey":
			return e.Name
		}
		require.FailNow(t, "type identifier %q is not in the whitelist (only uint8/int32/uint64/uintptr/byte, mapLevelKey, or unsafe.Pointer allowed)", e.Name)
	case *ast.StarExpr:
		return "*" + canonicalTypeExpr(t, e.X)
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok && x.Name == "unsafe" && e.Sel.Name == "Pointer" {
			return "unsafe.Pointer"
		}
		require.FailNow(t, "selector expression is not in the whitelist (only unsafe.Pointer allowed, and the unsafe import must not be renamed)")
	}
	require.FailNow(t, "type expression %T is not allowed", expr)
	return ""
}

// canonicalFuncType 把 *ast.FuncType 渲染为规范签名文本:每个参数
// 必须恰好具名一个名字,至多一个未具名结果;匿名参数、变参、具名
// 返回值、多返回值一律 Fatal。无结果渲染为没有结果列表,有结果
// 渲染为单个空格加结果类型。
func canonicalFuncType(t *testing.T, ft *ast.FuncType) string {
	t.Helper()
	parts := make([]string, 0, 6)
	if ft.Params != nil {
		for _, field := range ft.Params.List {
			if len(field.Names) != 1 {
				names := make([]string, 0, len(field.Names))
				for _, n := range field.Names {
					names = append(names, n.Name)
				}
				require.FailNow(t, "each parameter must have exactly one name (anonymous and multi-name parameters are forbidden), got %d: [%s]",
					len(field.Names), strings.Join(names, ", "))
			}
			parts = append(parts, field.Names[0].Name+" "+canonicalTypeExpr(t, field.Type))
		}
	}
	sig := "func(" + strings.Join(parts, ", ") + ")"
	switch {
	case ft.Results == nil || len(ft.Results.List) == 0:
		// C void 返回:无结果列表。
	case len(ft.Results.List) == 1 && len(ft.Results.List[0].Names) == 0:
		sig += " " + canonicalTypeExpr(t, ft.Results.List[0].Type)
	default:
		require.FailNow(t, "there must be exactly one unnamed result (named and multiple results are forbidden), got %d result fields", len(ft.Results.List))
	}
	return sig
}

// canonicalizeSigText 把手写内联的期望签名文本解析为 AST 后交给
// 同一规范化器:期望与被检文件走完全相同的渲染路径,逐字比对才有
// 意义。签名以 "package p\ntype _ = <sig>" 的唯一声明解析。
func canonicalizeSigText(t *testing.T, sig string) string {
	t.Helper()
	src := "package p\n\ntype _ = " + sig + "\n"
	file, err := parser.ParseFile(token.NewFileSet(), "expected.go", src, 0)
	require.NoError(t, err, "failed to parse the expected signature %q", sig)
	require.Len(t, file.Decls, 1, "the expected signature %q must parse into exactly one declaration", sig)
	ts, ok := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)
	require.True(t, ok, "the expected signature %q did not parse into a type declaration", sig)
	ft, ok := ts.Type.(*ast.FuncType)
	require.True(t, ok, "the expected signature %q is not a function type", sig)
	return canonicalFuncType(t, ft)
}

// TestStaticFilesHaveNoBuildConstraints 验证三个被检文件均无任何
// 形式的构建约束注释。
func TestStaticFilesHaveNoBuildConstraints(t *testing.T) {
	for _, name := range staticFiles {
		t.Run(name, func(t *testing.T) {
			file := parsePackageFile(t, name)
			for _, group := range file.Comments {
				for _, c := range group.List {
					isBuildConstraint := strings.HasPrefix(c.Text, "//go:build") || strings.HasPrefix(c.Text, "// +build")
					assert.False(t, isBuildConstraint, "%s contains a build-constraint comment %q; the design requires the three files to carry no platform conditions", name, c.Text)
				}
			}
		})
	}
}

// TestStaticTopLevelDeclsWhitelist 把三文件的顶层声明钉死为白名单:
// 序列必须与期望完全一致(按出现顺序),白名单之外的顶层声明即
// 失败;api.go 的 unsafe 导入不得有别名或重命名。
func TestStaticTopLevelDeclsWhitelist(t *testing.T) {
	cases := []struct {
		file string
		want []string
	}{
		{"api.go", []string{`import "unsafe"`, "type mapLevelKey", "type api"}},
		{"symbols.go", []string{
			"type symbolPolicy", "const(required)",
			"type symbolPlatform", "const(allPlatforms)",
			"type symbolSpec", "func symbolSpecs",
			"type exclusionSpec", "var exclusions",
		}},
		{"callback_types.go", []string{"type unixCallback", "type windowsCallback"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			file := parsePackageFile(t, tc.file)
			got := declKeys(file)
			for i := 0; i < len(got) || i < len(tc.want); i++ {
				switch {
				case i >= len(got):
					assert.Fail(t, "%s is missing whitelisted top-level declaration #%d %q (actual full list %v)", tc.file, i, tc.want[i], got)
					return
				case i >= len(tc.want):
					assert.Fail(t, "%s has a top-level declaration outside the whitelist %q (actual full list %v)", tc.file, got[i], got)
					return
				case got[i] != tc.want[i]:
					assert.Fail(t, "%s top-level declaration #%d = %q, want %q (got full list %v, want full list %v)",
						tc.file, i, got[i], tc.want[i], got, tc.want)
					return
				}
			}
		})
	}
	t.Run("api.go imports unsafe without alias", func(t *testing.T) {
		file := parsePackageFile(t, "api.go")
		require.Len(t, file.Imports, 1, "api.go import count = %d, want 1 (unsafe)", len(file.Imports))
		imp := file.Imports[0]
		path := strings.Trim(imp.Path.Value, `"`)
		assert.Equal(t, "unsafe", path, "api.go's sole import path = %q, want %q", path, "unsafe")
		if imp.Name != nil {
			assert.Nil(t, imp.Name, "api.go's unsafe import must not be aliased/renamed, got %q", imp.Name.Name)
		}
	})
}

// TestAPIFieldSignatures 逐字段核验 type api:字段数、字段名与
// 顺序对照期望表;每个字段必须是恰好一个具名的 func 类型,规范化
// 签名与期望表解析后的规范文本逐字一致。unsafe.Pointer 仅出现在
// GetImage/GetImageBgr 的 buff 参数由期望表本身钉死。
func TestAPIFieldSignatures(t *testing.T) {
	file := parsePackageFile(t, "api.go")
	var spec *ast.TypeSpec
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, s := range gd.Specs {
			if s.(*ast.TypeSpec).Name.Name == "api" {
				spec = s.(*ast.TypeSpec)
			}
		}
	}
	require.NotNil(t, spec, "type api not found in api.go")
	st, ok := spec.Type.(*ast.StructType)
	require.True(t, ok, "type api must be a struct, got %T", spec.Type)
	fields := st.Fields.List
	require.Len(t, fields, len(expectedAPIFields), "api field count = %d, want %d", len(fields), len(expectedAPIFields))
	for i, field := range fields {
		want := expectedAPIFields[i]
		require.Len(t, field.Names, 1, "field #%d must have exactly one name (embedded fields are forbidden), got %d", i, len(field.Names))
		gotName := field.Names[0].Name
		assert.Equal(t, want.field, gotName, "field #%d name = %q, want %q (field order must match the expectation table)", i, gotName, want.field)
		ft, ok := field.Type.(*ast.FuncType)
		require.True(t, ok, "the type of field %s must be a func type, got %T", gotName, field.Type)
		gotSig := canonicalFuncType(t, ft)
		wantSig := canonicalizeSigText(t, want.sig)
		assert.Equal(t, wantSig, gotSig, "field %s signature = %q, want %q", gotName, gotSig, wantSig)
	}
}

// TestSymbolSpecsForm 核验 symbolSpecs 的冻结形式:唯一 FuncDecl;
// 单参数 table *api;函数体只有一条 return;返回 []symbolSpec 直接
// 字面量,恰好 26 个元素;每元素恰好 4 个 KeyValueExpr 且键名顺序
// 为 Name/Target/Policy/Platform;Name 是等于 "Asst"+字段名的字符
// 串字面量,Target 是 &table.<字段>,Policy/Platform 是引用策略/
// 平台常量的标识符且与期望表一致;函数体内不存在任何调用。
func TestSymbolSpecsForm(t *testing.T) {
	file := parsePackageFile(t, "symbols.go")
	var fn *ast.FuncDecl
	funcDecls := 0
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			funcDecls++
			if fd.Name.Name == "symbolSpecs" {
				fn = fd
			}
		}
	}
	require.NotNil(t, fn, "func symbolSpecs not found in symbols.go")
	assert.Equal(t, 1, funcDecls, "symbols.go FuncDecl count = %d, want 1 (helpers are forbidden)", funcDecls)
	require.Nil(t, fn.Recv, "symbolSpecs must not have a receiver")
	gotParams := 0
	if fn.Type.Params != nil {
		gotParams = len(fn.Type.Params.List)
	}
	require.Equal(t, 1, gotParams, "symbolSpecs must have exactly one parameter, got %d parameter fields", gotParams)
	param := fn.Type.Params.List[0]
	require.Len(t, param.Names, 1, "the symbolSpecs parameter must be named table, got %d names", len(param.Names))
	require.Equal(t, "table", param.Names[0].Name, "the symbolSpecs parameter must be named table, got %d names", len(param.Names))
	star, ok := param.Type.(*ast.StarExpr)
	require.True(t, ok, "the symbolSpecs parameter type must be *api, got %T", param.Type)
	require.True(t, isIdent(star.X, "api"), "the symbolSpecs parameter type must be *api, got %T", param.Type)
	gotStmts := 0
	if fn.Body != nil {
		gotStmts = len(fn.Body.List)
	}
	require.Equal(t, 1, gotStmts, "the symbolSpecs body must contain exactly one statement, got %d", gotStmts)
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	require.True(t, ok, "the sole statement of the symbolSpecs body must be a return, got %T", fn.Body.List[0])
	require.Len(t, ret.Results, 1, "the return must have exactly one result expression, got %d", len(ret.Results))
	comp, ok := ret.Results[0].(*ast.CompositeLit)
	require.True(t, ok, "the return result must be a composite literal, got %T", ret.Results[0])
	arr, ok := comp.Type.(*ast.ArrayType)
	require.True(t, ok, "the returned composite literal's type must be []symbolSpec, got %T", comp.Type)
	require.Nil(t, arr.Len, "the returned composite literal's type must be []symbolSpec, got %T", comp.Type)
	require.True(t, isIdent(arr.Elt, "symbolSpec"), "the returned composite literal's type must be []symbolSpec, got %T", comp.Type)
	require.Len(t, comp.Elts, len(expectedAPIFields), "symbolSpecs entry count = %d, want %d", len(comp.Elts), len(expectedAPIFields))
	specKeys := [4]string{"Name", "Target", "Policy", "Platform"}
	for i, elt := range comp.Elts {
		entry, ok := elt.(*ast.CompositeLit)
		require.True(t, ok, "entry #%d must be a composite literal, got %T", i, elt)
		assert.Nil(t, entry.Type, "entry #%d's type must be omitted (supplied by the outer []symbolSpec), got %T", i, entry.Type)
		require.Len(t, entry.Elts, len(specKeys), "entry #%d must have exactly %d KeyValueExprs, got %d", i, len(specKeys), len(entry.Elts))
		want := expectedAPIFields[i]
		symbol := "Asst" + want.field
		for k := range entry.Elts {
			kv, ok := entry.Elts[k].(*ast.KeyValueExpr)
			require.True(t, ok, "entry #%d element %d must be a KeyValueExpr, got %T", i, k, entry.Elts[k])
			key, ok := kv.Key.(*ast.Ident)
			require.True(t, ok, "entry #%d key %d must be a named key, got %T", i, k, kv.Key)
			require.Equal(t, specKeys[k], key.Name, "entry #%d key %d = %q, want %q (key order must be Name/Target/Policy/Platform)",
				i, k, key.Name, specKeys[k])
			switch key.Name {
			case "Name":
				lit, ok := kv.Value.(*ast.BasicLit)
				require.True(t, ok, "entry #%d (%s) Name must be a string literal, got %T", i, symbol, kv.Value)
				require.Equal(t, token.STRING, lit.Kind, "entry #%d (%s) Name must be a string literal, got %T", i, symbol, kv.Value)
				name, err := strconv.Unquote(lit.Value)
				require.NoError(t, err, "cannot unquote entry #%d's Name literal", i)
				assert.Equal(t, symbol, name, "entry #%d Name = %q, want %q", i, name, symbol)
			case "Target":
				unary, ok := kv.Value.(*ast.UnaryExpr)
				require.True(t, ok, "entry #%d (%s) Target must be &table.<field>, got %T", i, symbol, kv.Value)
				require.Equal(t, token.AND, unary.Op, "entry #%d (%s) Target must be &table.<field>, got %T", i, symbol, kv.Value)
				sel, ok := unary.X.(*ast.SelectorExpr)
				require.True(t, ok, "entry #%d (%s) Target must be &table.<field>, got %T", i, symbol, unary.X)
				require.True(t, isIdent(sel.X, "table"), "entry #%d (%s) Target base name must be table (indirect variable targets are forbidden), got %T", i, symbol, sel.X)
				assert.Equal(t, want.field, sel.Sel.Name, "entry #%d (%s) Target field = %q, want %q", i, symbol, sel.Sel.Name, want.field)
			case "Policy":
				id, ok := kv.Value.(*ast.Ident)
				require.True(t, ok, "entry #%d (%s) Policy must reference required/optional, got %T", i, symbol, kv.Value)
				require.True(t, id.Name == "required" || id.Name == "optional", "entry #%d (%s) Policy must reference required/optional, got %T", i, symbol, kv.Value)
				assert.Equal(t, want.policy, id.Name, "entry #%d (%s) Policy = %q, want %q", i, symbol, id.Name, want.policy)
			case "Platform":
				id, ok := kv.Value.(*ast.Ident)
				require.True(t, ok, "entry #%d (%s) Platform must reference allPlatforms/windowsOnly, got %T", i, symbol, kv.Value)
				require.True(t, id.Name == "allPlatforms" || id.Name == "windowsOnly", "entry #%d (%s) Platform must reference allPlatforms/windowsOnly, got %T", i, symbol, kv.Value)
				assert.Equal(t, want.platform, id.Name, "entry #%d (%s) Platform = %q, want %q", i, symbol, id.Name, want.platform)
			}
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		if _, ok := n.(*ast.CallExpr); ok {
			assert.Fail(t, "no function calls may appear inside symbolSpecs (helpers/append/concatenation/reflection)")
		}
		return true
	})
}

// TestExclusionsForm 核验 exclusions:唯一 var;直接 []exclusionSpec
// 字面量,恰好 4 个元素;每元素恰好 Name/Header/Reason 三个键且均
// 为非空字符串字面量;Name 集合与顺序同期望表且不重复,Header 与
// 期望表一致(两个来源头)。
func TestExclusionsForm(t *testing.T) {
	file := parsePackageFile(t, "symbols.go")
	var vdecl *ast.GenDecl
	varCount := 0
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			varCount++
			vdecl = gd
		}
	}
	require.Equal(t, 1, varCount, "symbols.go var declaration count = %d, want 1 (exclusions)", varCount)
	require.Len(t, vdecl.Specs, 1, "exclusions must be a single var declaration, got %d specs", len(vdecl.Specs))
	vs, ok := vdecl.Specs[0].(*ast.ValueSpec)
	require.True(t, ok, "the sole var must be named exclusions")
	require.Len(t, vs.Names, 1, "the sole var must be named exclusions")
	require.Equal(t, "exclusions", vs.Names[0].Name, "the sole var must be named exclusions")
	require.Len(t, vs.Values, 1, "exclusions must be a direct literal, got %d values", len(vs.Values))
	comp, ok := vs.Values[0].(*ast.CompositeLit)
	require.True(t, ok, "exclusions must be an []exclusionSpec composite literal, got %T", vs.Values[0])
	arr, ok := comp.Type.(*ast.ArrayType)
	require.True(t, ok, "exclusions' type must be []exclusionSpec, got %T", comp.Type)
	require.Nil(t, arr.Len, "exclusions' type must be []exclusionSpec, got %T", comp.Type)
	require.True(t, isIdent(arr.Elt, "exclusionSpec"), "exclusions' type must be []exclusionSpec, got %T", comp.Type)
	require.Len(t, comp.Elts, len(expectedExclusions), "exclusion entry count = %d, want %d", len(comp.Elts), len(expectedExclusions))
	entryKeys := [3]string{"Name", "Header", "Reason"}
	seen := map[string]bool{}
	for i, elt := range comp.Elts {
		entry, ok := elt.(*ast.CompositeLit)
		require.True(t, ok, "exclusion entry #%d must be a composite literal, got %T", i, elt)
		assert.Nil(t, entry.Type, "exclusion entry #%d's type must be omitted (supplied by the outer []exclusionSpec), got %T", i, entry.Type)
		require.Len(t, entry.Elts, len(entryKeys), "exclusion entry #%d must have exactly %d keys, got %d", i, len(entryKeys), len(entry.Elts))
		values := map[string]string{}
		for k := range entry.Elts {
			kv, ok := entry.Elts[k].(*ast.KeyValueExpr)
			require.True(t, ok, "exclusion entry #%d element %d must be a KeyValueExpr, got %T", i, k, entry.Elts[k])
			key, ok := kv.Key.(*ast.Ident)
			require.True(t, ok, "exclusion entry #%d key %d must be a named key, got %T", i, k, kv.Key)
			require.Equal(t, entryKeys[k], key.Name, "exclusion entry #%d key %d = %q, want %q (key order must be Name/Header/Reason)",
				i, k, key.Name, entryKeys[k])
			lit, ok := kv.Value.(*ast.BasicLit)
			require.True(t, ok, "exclusion entry #%d's %s must be a string literal, got %T", i, key.Name, kv.Value)
			require.Equal(t, token.STRING, lit.Kind, "exclusion entry #%d's %s must be a string literal, got %T", i, key.Name, kv.Value)
			text, err := strconv.Unquote(lit.Value)
			require.NoError(t, err, "cannot unquote exclusion entry #%d's %s literal", i, key.Name)
			assert.NotEmpty(t, text, "exclusion entry #%d's %s must not be empty", i, key.Name)
			values[key.Name] = text
		}
		want := expectedExclusions[i]
		assert.Equal(t, want.name, values["Name"], "exclusion entry #%d Name = %q, want %q (both the entry set and order are frozen)", i, values["Name"], want.name)
		assert.Equal(t, want.header, values["Header"], "exclusion entry %q Header = %q, want %q", values["Name"], values["Header"], want.header)
		assert.False(t, seen[values["Name"]], "duplicate exclusion entry Name: %q", values["Name"])
		seen[values["Name"]] = true
	}
}

// TestPolicyPlatformConstBlocks 核验两个 const 块:symbolPolicy 块
// required(带 iota)在前、optional 在后;symbolPlatform 块
// allPlatforms 在前、windowsOnly 在后;次项均沿用首项的类型与
// iota,不显式赋值;两个类型的底层类型均为 uint8。
func TestPolicyPlatformConstBlocks(t *testing.T) {
	file := parsePackageFile(t, "symbols.go")
	var blocks []*ast.GenDecl
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
			blocks = append(blocks, gd)
		}
	}
	require.Len(t, blocks, 2, "symbols.go const block count = %d, want 2", len(blocks))
	checkEnumBlock(t, blocks[0], "symbolPolicy", "required", "optional")
	checkEnumBlock(t, blocks[1], "symbolPlatform", "allPlatforms", "windowsOnly")

	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, s := range gd.Specs {
			ts := s.(*ast.TypeSpec)
			if ts.Name.Name != "symbolPolicy" && ts.Name.Name != "symbolPlatform" {
				continue
			}
			assert.True(t, isIdent(ts.Type, "uint8"), "type %s's underlying type must be uint8, got %T", ts.Name.Name, ts.Type)
		}
	}
}

// checkEnumBlock 核验单个策略/平台 const 块:恰两个 spec;首项具名
// first、显式标注 blockType 且以 iota 起始;次项具名 second,不
// 显式赋值(沿用首项类型与 iota 递进)。
func checkEnumBlock(t *testing.T, block *ast.GenDecl, blockType, first, second string) {
	t.Helper()
	assert.True(t, block.Lparen.IsValid(), "const %s must be written as a parenthesized const block", blockType)
	require.Len(t, block.Specs, 2, "const %s block must have exactly 2 specs, got %d", blockType, len(block.Specs))
	firstSpec, ok := block.Specs[0].(*ast.ValueSpec)
	require.True(t, ok, "the first spec of the const %s block must be named %s", blockType, first)
	require.Len(t, firstSpec.Names, 1, "the first spec of the const %s block must be named %s", blockType, first)
	require.Equal(t, first, firstSpec.Names[0].Name, "the first spec of the const %s block must be named %s", blockType, first)
	assert.True(t, isIdent(firstSpec.Type, blockType), "the first spec of the const %s block must be explicitly typed %s, got %T", blockType, blockType, firstSpec.Type)
	assert.Len(t, firstSpec.Values, 1, "the first spec %s of the const %s block must start with iota", blockType, first)
	assert.True(t, isIdent(firstSpec.Values[0], "iota"), "the first spec %s of the const %s block must start with iota", blockType, first)
	secondSpec, ok := block.Specs[1].(*ast.ValueSpec)
	require.True(t, ok, "the second spec of the const %s block must be named %s", blockType, second)
	require.Len(t, secondSpec.Names, 1, "the second spec of the const %s block must be named %s", blockType, second)
	require.Equal(t, second, secondSpec.Names[0].Name, "the second spec of the const %s block must be named %s", blockType, second)
	assert.Nil(t, secondSpec.Type, "the second spec %s of the const %s block must inherit the first spec's type and iota; no explicit value assignment", blockType, second)
	assert.Empty(t, secondSpec.Values, "the second spec %s of the const %s block must inherit the first spec's type and iota; no explicit value assignment", blockType, second)
}

// TestCallbackTypeSignatures 核验两个 callback 类型:直接是 func
// 类型,规范化签名与手写内联期望逐字一致——Unix 无返回值,Windows
// purego 路径返回 uintptr。
func TestCallbackTypeSignatures(t *testing.T) {
	file := parsePackageFile(t, "callback_types.go")
	specs := map[string]*ast.TypeSpec{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, s := range gd.Specs {
			ts := s.(*ast.TypeSpec)
			specs[ts.Name.Name] = ts
		}
	}
	for _, want := range expectedCallbackSigs {
		t.Run(want.typeName, func(t *testing.T) {
			ts, ok := specs[want.typeName]
			require.True(t, ok, "type %s not found in callback_types.go", want.typeName)
			ft, ok := ts.Type.(*ast.FuncType)
			require.True(t, ok, "type %s must directly be a func type, got %T", want.typeName, ts.Type)
			got := canonicalFuncType(t, ft)
			expect := canonicalizeSigText(t, want.sig)
			assert.Equal(t, expect, got, "type %s signature = %q, want %q", want.typeName, got, expect)
		})
	}
}

// TestCapabilitySwitchMatchesOptionalSpecs 把 register.go 中能力置位
// switch 与 optional 符号清单钉死为静态交叉校验。若未来新增 optional
// 符号而漏改置位逻辑,符号会照常注册但能力恒关、行为用例全部通过,
// 本测试以「两处独立期望的集合相等」堵住该漂移。断言:
//
//   - expectedCapabilityFields 的键集合与期望表中 optional 条目
//     ("Asst"+字段名)完全相等;
//   - register.go 恰有一个对 spec.Name 的 switch,case 集合与该映射
//     完全相等,无 default 分支;
//   - 每个 case 的函数体恰为 caps.<字段> = true 一条赋值,写入字段
//     与映射一致。
func TestCapabilitySwitchMatchesOptionalSpecs(t *testing.T) {
	optional := map[string]bool{}
	for _, want := range expectedAPIFields {
		if want.policy == "optional" {
			optional["Asst"+want.field] = true
		}
	}
	require.Len(t, optional, len(expectedCapabilityFields), "capability field expectation map has %d entries, unequal to the expectation table's %d optional entries", len(expectedCapabilityFields), len(optional))
	for name := range expectedCapabilityFields {
		assert.Contains(t, optional, name, "%q in the capability field map is not an optional entry of the expectation table", name)
	}
	for name := range optional {
		assert.Contains(t, expectedCapabilityFields, name, "optional entry %q is missing from the capability field map", name)
	}

	file := parsePackageFile(t, "register.go")
	var switches []*ast.SwitchStmt
	ast.Inspect(file, func(n ast.Node) bool {
		if sw, ok := n.(*ast.SwitchStmt); ok {
			if sel, ok := sw.Tag.(*ast.SelectorExpr); ok && isIdent(sel.X, "spec") && sel.Sel.Name == "Name" {
				switches = append(switches, sw)
			}
		}
		return true
	})
	require.Len(t, switches, 1, "register.go should have exactly one capability-setting switch over spec.Name, got %d", len(switches))

	got := map[string]string{}
	for _, stmt := range switches[0].Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		require.True(t, ok, "a non-case statement %T appears in the switch body", stmt)
		if clause.List == nil {
			require.FailNow(t, "the capability-setting switch must not have a default branch; the case set should equal the optional list exactly")
		}
		require.Len(t, clause.List, 1, "each case must have exactly one symbol literal, got %d", len(clause.List))
		lit, ok := clause.List[0].(*ast.BasicLit)
		require.True(t, ok, "the case value must be a string literal, got %T", clause.List[0])
		require.Equal(t, token.STRING, lit.Kind, "the case value must be a string literal, got %T", clause.List[0])
		symbol, err := strconv.Unquote(lit.Value)
		require.NoError(t, err, "case value %s is not a valid string literal", lit.Value)
		assert.NotContains(t, got, symbol, "case %s appears more than once", symbol)
		require.Len(t, clause.Body, 1, "the body of case %s must be exactly one assignment, got %d", symbol, len(clause.Body))
		assign, ok := clause.Body[0].(*ast.AssignStmt)
		require.True(t, ok, "the body of case %s must be the single assignment caps.<field> = true", symbol)
		require.Len(t, assign.Lhs, 1, "the body of case %s must be the single assignment caps.<field> = true", symbol)
		require.Len(t, assign.Rhs, 1, "the body of case %s must be the single assignment caps.<field> = true", symbol)
		require.True(t, isIdent(assign.Rhs[0], "true"), "the body of case %s must be the single assignment caps.<field> = true", symbol)
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		require.True(t, ok, "the assignment target of case %s must be caps.<field>", symbol)
		require.True(t, isIdent(sel.X, "caps"), "the assignment target of case %s must be caps.<field>", symbol)
		got[symbol] = sel.Sel.Name
	}
	for symbol, field := range expectedCapabilityFields {
		actual, ok := got[symbol]
		if !assert.True(t, ok, "the capability-setting switch is missing a case for optional symbol %q", symbol) {
			continue
		}
		assert.Equal(t, field, actual, "case %q sets field = %q, want %q", symbol, actual, field)
	}
	for symbol := range got {
		assert.Contains(t, expectedCapabilityFields, symbol, "the capability-setting switch contains symbol %q outside the optional list", symbol)
	}
}

// TestNativePackageExportsNoRawFunctions 把「raw 函数只能作为未导出
// 类型的未导出/具名字段存在于包内;公开层不出现可复制的 raw 函数
// 变量」钉死为静态断言。扫描包目录全部非 _test.go 文件(go/parser,
// 测试运行目录即包目录),断言:
//
//   - 无导出的包级 var/const 其显式类型为函数类型;
//   - 无导出的函数或方法其结果类型为函数类型;
//   - 无导出结构体类型的导出字段为函数类型;
//   - 特别断言 APITable 与 Capabilities 没有任何导出字段——能力
//     只能经方法读取。
func TestNativePackageExportsNoRawFunctions(t *testing.T) {
	dir, err := os.Getwd()
	require.NoError(t, err, "os.Getwd failed, cannot locate the package directory")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "failed to read the package directory %s", dir)
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	require.NotEmpty(t, names, "no non-test Go source files found in the package directory")

	seenStructs := map[string]bool{}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			file := parsePackageFile(t, name)
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					switch d.Tok {
					case token.VAR, token.CONST:
						for _, spec := range d.Specs {
							vs, ok := spec.(*ast.ValueSpec)
							if !ok || vs.Type == nil {
								continue
							}
							if _, isFunc := vs.Type.(*ast.FuncType); !isFunc {
								continue
							}
							kind := "var"
							if d.Tok == token.CONST {
								kind = "const"
							}
							for _, id := range vs.Names {
								assert.False(t, ast.IsExported(id.Name), "exported package-level %s %q has an explicit func type; copyable raw function variables must not appear in the public layer", kind, id.Name)
							}
						}
					case token.TYPE:
						for _, spec := range d.Specs {
							ts := spec.(*ast.TypeSpec)
							st, ok := ts.Type.(*ast.StructType)
							if !ok {
								continue
							}
							exportedType := ast.IsExported(ts.Name.Name)
							for _, field := range st.Fields.List {
								if _, isFunc := field.Type.(*ast.FuncType); !isFunc || len(field.Names) == 0 {
									continue
								}
								for _, id := range field.Names {
									assert.False(t, exportedType && ast.IsExported(id.Name), "exported field %q of exported struct %s has a func type; copyable raw functions must not be exported to the public layer", id.Name, ts.Name.Name)
								}
							}
							if ts.Name.Name == "APITable" || ts.Name.Name == "Capabilities" {
								seenStructs[ts.Name.Name] = true
								for _, field := range st.Fields.List {
									for _, id := range field.Names {
										assert.False(t, ast.IsExported(id.Name), "exported struct %s must not have any exported fields, got %q", ts.Name.Name, id.Name)
									}
								}
							}
						}
					}
				case *ast.FuncDecl:
					if !ast.IsExported(d.Name.Name) || d.Type.Results == nil {
						continue
					}
					for _, result := range d.Type.Results.List {
						_, isFunc := result.Type.(*ast.FuncType)
						assert.False(t, isFunc, "exported function/method %q has a func result type; copyable raw functions must not be exported to the public layer", d.Name.Name)
					}
				}
			}
		})
	}

	assert.True(t, seenStructs["APITable"] && seenStructs["Capabilities"], "both read-only publication types APITable and Capabilities must exist in the package, found %v", seenStructs)
}
