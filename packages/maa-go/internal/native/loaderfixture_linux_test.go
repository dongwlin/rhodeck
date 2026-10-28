//go:build linux

// 本文件以独立 C 夹具验证 loader 的完整装载链路:Load → Lookup →
// ResolveAndRegister,包括缺符号聚合报告与失败不发布半成品。

package native

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/ffitest"
)

// 子进程模式、资源传递环境变量与 stdout 成功标记。
const (
	// loaderFixtureModeComplete 是完整变体子进程模式:装载 → 解析注册
	// → 能力快照断言 → 未知符号查找失败断言,全部通过后打印标记退出 0。
	loaderFixtureModeComplete = "loaderfixture-complete"

	// loaderFixtureModeMissing 是缺失变体子进程模式:Open 成功而
	// ResolveAndRegister 以聚合错误整体失败,断言通过后打印标记退出 0。
	loaderFixtureModeMissing = "loaderfixture-missing"

	// loaderFixtureSOEnv 向子进程传递夹具.so 的绝对路径;子进程先
	// 断言其为绝对路径,与 loader 只接受绝对路径的冻结策略一致。
	loaderFixtureSOEnv = "MAA_LOADER_FIXTURE_SO"

	// loaderFixtureSrcRel 是夹具源码相对包目录的路径:go test 的工作
	// 目录是包源码目录,testdata/ 下的文件不参与 Go 构建。
	loaderFixtureSrcRel = "testdata/loaderfixture/fixture.c"

	// loaderFixtureSOName 是编译产物文件名。
	loaderFixtureSOName = "libloaderfixture.so"

	// completeOKMarker / missingOKMarker 是两种子进程全部内部断言通过
	// 后打印的结构化标记:父进程不信任子进程「退出 0」本身,只以
	// stdout 实际标记行及其字段为复验依据(纵深防御)。
	completeOKMarker = "MAA_LOADER_FIXTURE_COMPLETE_OK"
	missingOKMarker  = "MAA_LOADER_FIXTURE_MISSING_OK"

	// loaderFixtureProbeSymbol 是完整变体中的故意缺失符号:验证 Lookup
	// 的失败路径返回结构化 *SymbolLookupError。
	loaderFixtureProbeSymbol = "AsstNoSuchSymbolForFixtureProbe"

	// loaderFixtureChildTimeout 是父进程强制的子进程 wall-clock 上限:
	// 装载毫秒级完成,2 分钟只防御环境异常导致的挂起。
	loaderFixtureChildTimeout = 2 * time.Minute
)

// expectedMissingFixtureSymbols 是缺失变体必须被聚合报告的符号集合,
// 顺序即 symbolSpecs 声明顺序(位于 allPlatforms 清单(25 条)第 1、
// 18、25 条——首部、后部、尾部)。
// 该集合必须与 fixture.c 的 LOADER_FIXTURE_MISSING 变体严格一致:
// 任何一侧改动都必须同步另一侧,否则两用例之一必然失败。
var expectedMissingFixtureSymbols = []string{
	"AsstSetUserDir",
	"AsstAsyncScreencap",
	"AsstLog",
}

func init() {
	// 子进程与父进程是同一测试二进制:注册发生在 init(),先于
	// TestMain 的 Dispatch,模式对子进程可见。重复注册会 panic,
	// 配置错误在启动期即暴露。
	ffitest.Register(loaderFixtureModeComplete, runLoaderFixtureCompleteChild)
	ffitest.Register(loaderFixtureModeMissing, runLoaderFixtureMissingChild)
}

// TestMain 先分派子进程模式(命中即 os.Exit,不进入测试框架),父
// 进程路径照常 m.Run()。本包不得有第二个 TestMain:本文件是包内唯一
// 的 TestMain,全部既有测试共用此入口,分派不改变其可见行为。
func TestMain(m *testing.M) {
	if code, handled := ffitest.Dispatch(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// buildLoaderFixture 编译独立 C 夹具,返回.so 绝对路径;missing 为
// true 时编译「多个必需导出缺失」变体。
//
// 编译器发现:依次 exec.LookPath 查找 cc、gcc;两者都缺失时 Skipf
// ——loader fixture 受阻属于测试环境受限,计为 SKIP(跳过不是通过),
// 夹具构建与装载验证未执行,不冒充本平台通过。C 编译器是本验证的
// 测试环境工具链需求,与生产构建无关:生产二进制 CGO_ENABLED=0、不
// 经 cgo,夹具构建不构成生产 cgo 依赖。
//
// 编译进 t.TempDir(),输出 libloaderfixture.so;missing 变体加
// -DLOADER_FIXTURE_MISSING(置于源文件参数之前)。所用编译器路径、
// 编译器 --version 首行、完整编译命令行与变体全部经 t.Logf 记录
// (步骤 4:记录夹具源码、编译参数、平台和工具链)。编译
// 失败 Fatalf 并附完整合并输出,不重试。
func buildLoaderFixture(t *testing.T, missing bool) string {
	t.Helper()

	compiler := ""
	for _, name := range []string{"cc", "gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Skipf("loader fixture blocked: no C compiler found; fixture build and load verification not executed, does not count as a pass on this platform")
	}

	src, err := filepath.Abs(loaderFixtureSrcRel)
	if err != nil {
		t.Fatalf("failed to locate fixture source %s: %v", loaderFixtureSrcRel, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture source %s is not accessible (repository baseline broken, not an environment limitation): %v", src, err)
	}

	outPath := filepath.Join(t.TempDir(), loaderFixtureSOName)
	args := []string{"-shared", "-fPIC"}
	if missing {
		args = append(args, "-DLOADER_FIXTURE_MISSING")
	}
	args = append(args, "-o", outPath, src)
	commandLine := compiler + " " + strings.Join(args, " ")

	variant := "complete(all 28 exports present)"
	if missing {
		variant = "missing(-DLOADER_FIXTURE_MISSING, omitting 3 required symbols)"
	}
	t.Logf("loader fixture build: variant=%s", variant)
	t.Logf("loader fixture build: source=%s", src)
	t.Logf("loader fixture build: platform=%s/%s", runtime.GOOS, runtime.GOARCH)
	t.Logf("loader fixture build: compiler=%s", compiler)
	t.Logf("loader fixture build: toolchain=%s", compilerFirstLine(t, compiler))
	t.Logf("loader fixture build: command line=%s", commandLine)

	cmd := exec.Command(compiler, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to compile the loader fixture (command line %s): %v\n%s", commandLine, err, output)
	}
	return outPath
}

// compilerFirstLine 返回编译器 --version 输出的首行,供工具链日志
// 记录;查询失败不阻塞用例(版本信息是诊断增强,不是验收内容),
// 记为占位文本。
func compilerFirstLine(t *testing.T, compiler string) string {
	t.Helper()
	out, err := exec.Command(compiler, "--version").Output()
	if err != nil {
		return fmt.Sprintf("%s --version query failed: %v", compiler, err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// runLoaderFixtureCompleteChild 是 loaderfixture-complete 子进程入口:
// 经生产 NewLoader().Open 装载完整夹具,ResolveAndRegister 解析并注册
// 全部适用符号后发布只读 API 表,断言能力快照与未知符号查找失败,
// 全部通过后打印结构化标记并返回 0。全程不触达任何 raw 调用包装:
// 夹具导出是空函数且签名有意不对应真实 ABI,绝不可被调用。
func runLoaderFixtureCompleteChild() int {
	so := os.Getenv(loaderFixtureSOEnv)
	if !filepath.IsAbs(so) {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: %s = %q is not an absolute path (the loader only accepts absolute paths)\n", loaderFixtureSOEnv, so)
		return 1
	}

	lib, err := NewLoader().Open(so)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: Open(%s) failed: %v\n", so, err)
		return 1
	}
	table, err := ResolveAndRegister(lib)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: ResolveAndRegister(%s) failed: %v\n", so, err)
		return 1
	}
	if table == nil {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: ResolveAndRegister succeeded but published no API table")
		return 1
	}

	caps := table.Capabilities()
	if !caps.Create() {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: Capabilities.Create() = false, want true (AsstCreate is present)")
		return 1
	}
	if !caps.ImageBgr() {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: Capabilities.ImageBgr() = false, want true (AsstGetImageBgr is present)")
		return 1
	}
	if caps.AsyncAttachWindow() {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: Capabilities.AsyncAttachWindow() = true, want false (the windowsOnly symbol is not looked up on Linux)")
		return 1
	}
	if !caps.MapLevelKey() || !caps.ItemName() {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: Capabilities.MapLevelKey()=%v ItemName()=%v, want true,true (the experimental exports are always present in the fixture)\n",
			caps.MapLevelKey(), caps.ItemName())
		return 1
	}

	_, lookupErr := lib.Lookup(loaderFixtureProbeSymbol)
	if lookupErr == nil {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: Lookup(%s) unexpectedly succeeded, it must fail\n", loaderFixtureProbeSymbol)
		return 1
	}
	var sle *SymbolLookupError
	if !errors.As(lookupErr, &sle) {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: Lookup(%s) error %T is not a *SymbolLookupError: %v\n",
			loaderFixtureProbeSymbol, lookupErr, lookupErr)
		return 1
	}

	fmt.Printf("%s symbols=%d caps=create=%d,imageBgr=%d,attach=%d,mapLevelKey=%d,itemName=%d\n",
		completeOKMarker, countApplicableSpecs(),
		flagBit(caps.Create()), flagBit(caps.ImageBgr()), flagBit(caps.AsyncAttachWindow()),
		flagBit(caps.MapLevelKey()), flagBit(caps.ItemName()))
	return 0
}

// runLoaderFixtureMissingChild 是 loaderfixture-missing 子进程入口:
// 装载缺失 3 个必需符号的夹具变体,断言 Open 成功而 ResolveAndRegister
// 整体失败——错误可 errors.As 命中 AggregateError、不发布 API 表,
// 聚合成员逐项为 *SymbolLookupError 且符号集合与顺序恰为
// expectedMissingFixtureSymbols(一次调用收集全部必需项失败的原子
// 性),每项携带库路径与 ABI 组合标识。全部通过后打印标记返回 0。
func runLoaderFixtureMissingChild() int {
	so := os.Getenv(loaderFixtureSOEnv)
	if !filepath.IsAbs(so) {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: %s = %q is not an absolute path (the loader only accepts absolute paths)\n", loaderFixtureSOEnv, so)
		return 1
	}

	lib, err := NewLoader().Open(so)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: Open(%s) failed: %v (missing symbols happen at the symbol resolution stage; the load itself must succeed)\n", so, err)
		return 1
	}
	table, err := ResolveAndRegister(lib)
	if err == nil {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: ResolveAndRegister unexpectedly succeeded; missing required symbols must fail as a whole\n")
		return 1
	}
	if table != nil {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: a failed transaction must not publish an API table, got %v\n", table)
		return 1
	}
	var agg AggregateError
	if !errors.As(err, &agg) {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: errors.As did not match AggregateError, got %T: %v\n", err, err)
		return 1
	}
	if len(agg) != len(expectedMissingFixtureSymbols) {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: aggregate error has %d items, want %d: %v\n",
			len(agg), len(expectedMissingFixtureSymbols), err)
		return 1
	}

	missing := make([]string, 0, len(agg))
	for i, item := range agg {
		var sle *SymbolLookupError
		if !errors.As(item, &sle) {
			fmt.Fprintf(os.Stderr, "loader fixture missing child: aggregate item %d (%T) is not a *SymbolLookupError: %v\n", i, item, item)
			return 1
		}
		if sle.LibraryPath != so {
			fmt.Fprintf(os.Stderr, "loader fixture missing child: aggregate item %d LibraryPath = %q, want %q\n", i, sle.LibraryPath, so)
			return 1
		}
		if sle.Profile != abiProfile {
			fmt.Fprintf(os.Stderr, "loader fixture missing child: aggregate item %d Profile = %q, want %q\n", i, sle.Profile, abiProfile)
			return 1
		}
		missing = append(missing, sle.Symbol)
	}
	if strings.Join(missing, ",") != strings.Join(expectedMissingFixtureSymbols, ",") {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: missing symbol set/order = [%s], want [%s] (must match fixture.c's missing variant and declaration order exactly)\n",
			strings.Join(missing, ","), strings.Join(expectedMissingFixtureSymbols, ","))
		return 1
	}

	fmt.Printf("%s symbols=%s\n", missingOKMarker, strings.Join(missing, ","))
	return 0
}

// TestLoaderFixtureCompleteLoadsInFreshSubprocess 验收完整夹具装载:
// 新子进程中 NewLoader().Open → ResolveAndRegister 全部成功并发布
// API 表。父进程断言子进程退出 0、标记行在场,并独立复验两处:
//   - symbols 值等于父进程侧由 symbolSpecs(new(api)) 现算的适用符号
//     条数(Linux 上即 allPlatforms 条数 25)。子进程标记行与父进程
//     复验出自同一测试二进制内的同一现算函数(countApplicableSpecs),
//     该比对只防标记行失真或过期硬编码,防不了夹具↔symbolSpecs 的
//     漂移;夹具↔符号清单的最终同步以子进程 ResolveAndRegister 的
//     成败为裁决(缺必需导出即整体失败),加本用例硬编码复验的
//     caps=1,1,0;
//   - caps 字段恰为 create=1,imageBgr=1,attach=0。
//
// 无编译器时在构建入口处 skip(不构建夹具、不启动子进程);子进程
// 失败(非零退出/信号/超时)一律 Fatalf 附完整诊断,不重试。
func TestLoaderFixtureCompleteLoadsInFreshSubprocess(t *testing.T) {
	soPath := buildLoaderFixture(t, false)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     loaderFixtureModeComplete,
		Timeout:  loaderFixtureChildTimeout,
		ExtraEnv: map[string]string{loaderFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("the complete child failed (non-zero exit/signal/timeout are all treated as failure):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, completeOKMarker)
	if !ok {
		t.Fatalf("the complete child exited 0 but stdout is missing the %s marker line:\n%s", completeOKMarker, out.Diagnostics())
	}

	gotSymbols, err := parseMarkerSymbolsCount(line)
	if err != nil {
		t.Fatalf("failed to parse the complete marker line %q: %v\n%s", line, err, out.Diagnostics())
	}
	wantSymbols := countApplicableSpecs()
	if gotSymbols != wantSymbols {
		t.Fatalf("complete marker line symbols = %d, want %d (the marker line disagrees with the freshly computed symbol list count; guards against marker distortion):\n%s",
			gotSymbols, wantSymbols, out.Diagnostics())
	}

	create, imageBgr, attach, mapLevelKey, itemName, err := parseMarkerCaps(line)
	if err != nil {
		t.Fatalf("failed to parse caps of the complete marker line %q: %v\n%s", line, err, out.Diagnostics())
	}
	if create != 1 || imageBgr != 1 || attach != 0 || mapLevelKey != 1 || itemName != 1 {
		t.Fatalf("complete marker line caps = create=%d,imageBgr=%d,attach=%d,mapLevelKey=%d,itemName=%d, want 1,1,0,1,1:\n%s",
			create, imageBgr, attach, mapLevelKey, itemName, out.Diagnostics())
	}

	t.Logf("the complete child's load verification passed (separate marker, not counted in ABI call pass statistics):%s", line)
}

// TestLoaderFixtureMissingRequiredSymbolsReportedTogether 验收缺失
// 夹具的聚合报告:缺失 3 个必需符号时,一次 ResolveAndRegister 调用
// 收集全部失败并整体拒绝发布。父进程断言子进程退出 0(其内部断言
// 全部通过)、标记行在场,并独立复验标记行 symbols 列表与顺序恰为
// expectedMissingFixtureSymbols——与子进程内断言互相独立(纵深防御)。
func TestLoaderFixtureMissingRequiredSymbolsReportedTogether(t *testing.T) {
	soPath := buildLoaderFixture(t, true)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     loaderFixtureModeMissing,
		Timeout:  loaderFixtureChildTimeout,
		ExtraEnv: map[string]string{loaderFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("the missing child failed (its internal assertions did not all pass):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, missingOKMarker)
	if !ok {
		t.Fatalf("the missing child exited 0 but stdout is missing the %s marker line:\n%s", missingOKMarker, out.Diagnostics())
	}

	got := parseMarkerSymbolsList(line)
	if strings.Join(got, ",") != strings.Join(expectedMissingFixtureSymbols, ",") {
		t.Fatalf("missing marker line symbols = [%s], want [%s]:\n%s",
			strings.Join(got, ","), strings.Join(expectedMissingFixtureSymbols, ","), out.Diagnostics())
	}
	t.Logf("the missing child's aggregate report verification passed (3 items collected in one call, order matches the symbol list):%s", line)
}

// countApplicableSpecs 现算当前平台适用的符号条数:遍历
// symbolSpecs(new(api)),Linux 上即 allPlatforms 条数(25)——唯一
// 的 windowsOnly 条目 AsstAsyncAttachWindow 不适用。子进程侧的标记
// 打印与父进程侧的独立复验共用本函数;夹具与符号清单的最终同步以
// ResolveAndRegister 的成败为裁决,本计数只钉住标记行与清单一致。
func countApplicableSpecs() int {
	n := 0
	for _, spec := range symbolSpecs(new(api)) {
		if specApplicable(spec) {
			n++
		}
	}
	return n
}

// flagBit 把布尔能力位渲染为标记行使用的 0/1。
func flagBit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// markerLine 从 stdout 定位以 marker 开头(后随空格)的首个标记行。
func markerLine(stdout, marker string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, marker+" ") {
			return line, true
		}
	}
	return "", false
}

// parseMarkerSymbolsCount 解析 complete 标记行的 symbols=<n> 字段。
func parseMarkerSymbolsCount(line string) (int, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "symbols=") {
		return 0, fmt.Errorf("the marker line lacks the symbols=<n> field")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(fields[1], "symbols="))
	if err != nil {
		return 0, fmt.Errorf("the symbols value is not a decimal integer: %w", err)
	}
	return n, nil
}

// parseMarkerSymbolsList 解析 missing 标记行的 symbols=a,b,c 字段,
// 返回逐项符号名(保持子进程报告的顺序)。
func parseMarkerSymbolsList(line string) []string {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "symbols=") {
		return nil
	}
	return strings.Split(strings.TrimPrefix(fields[1], "symbols="), ",")
}

// parseMarkerCaps 解析 complete 标记行的 caps=key=value,... 字段,
// 返回 create/imageBgr/attach 三个能力位。
func parseMarkerCaps(line string) (create, imageBgr, attach, mapLevelKey, itemName int, err error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "caps=") {
		return 0, 0, 0, 0, 0, fmt.Errorf("the marker line lacks the caps=... field")
	}
	for _, kv := range strings.Split(strings.TrimPrefix(fields[2], "caps="), ",") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return 0, 0, 0, 0, 0, fmt.Errorf("caps component %q lacks the key=value form", kv)
		}
		n, convErr := strconv.Atoi(value)
		if convErr != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("the value of caps component %q is not a decimal integer: %w", kv, convErr)
		}
		switch key {
		case "create":
			create = n
		case "imageBgr":
			imageBgr = n
		case "attach":
			attach = n
		case "mapLevelKey":
			mapLevelKey = n
		case "itemName":
			itemName = n
		default:
			return 0, 0, 0, 0, 0, fmt.Errorf("caps contains unknown key %q", key)
		}
	}
	return create, imageBgr, attach, mapLevelKey, itemName, nil
}
