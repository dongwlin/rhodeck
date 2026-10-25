//go:build linux

// 本文件是 MAA-11 第二部分的 loader 夹具验证:以独立 C 夹具
// (testdata/loaderfixture/fixture.c)经真实系统 loader 验证
// Loader.Open → Library.Lookup → ResolveAndRegister 的完整装载链路。
//
// 对应任务验收条件的落实:
//   - 夹具构建入口(MAA-11 步骤 4):buildLoaderFixture 以 cc/gcc 编译
//     夹具,经 t.Logf 记录夹具源码路径、编译参数、平台与工具链;无
//     C 编译器的环境计为 SKIP(跳过不是通过),夹具构建与装载验证
//     未执行,不冒充本平台通过;
//   - 完整夹具(TestM2LoaderFixtureCompleteLoadsInFreshSubprocess):
//     26 导出全部在场时,装载 → 解析注册成功发布 API 表,能力快照
//     Create/ImageBgr 置位、AsyncAttachWindow 在 Linux 恒关闭
//     (windowsOnly 符号不查找),未知符号查找得到 *SymbolLookupError;
//   - 缺失夹具(TestM2LoaderFixtureMissingRequiredSymbolsReportedTogether):
//     3 个必需符号缺失时,一次 ResolveAndRegister 调用收集全部失败,
//     AggregateError 恰含 3 项 *SymbolLookupError、顺序与符号清单
//     一致,且不发布任何 API 表(失败原子性)。
//
// 为何走子进程执行器(internal/ffitest):FFI 纪律要求装载也发生在
// 全新进程内并由父进程外部强制超时——dlopen 的原生副作用(库内构造
// 器、依赖映射、系统 loader 状态)不可撤回,失败后同进程状态不可信。
// 夹具全部导出是空函数且签名有意不对应真实 ABI,绝不可被调用,因此
// 用例只触达 Open/Lookup/ResolveAndRegister 三个原语:不触达任何 raw
// 调用包装(calls.go),也不走公开 Open——公开 Open 会对加载结果发起
// GetVersion 等真实 ABI 调用,对空函数夹具既无意义也越出本文件边界。
//
// 边界声明:本文件是「装载验证」,结果以
// MAA_M2_LOADER_FIXTURE_*_OK 标记单独记录,不进入 ABI 调用通过统计;
// 真实 ABI 的调用验证由 MAA-12 的独立 fixture 承担,两者不得混用。

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

	// loaderFixtureSOEnv 向子进程传递夹具 .so 的绝对路径;子进程先
	// 断言其为绝对路径,与 loader 只接受绝对路径的冻结策略一致。
	loaderFixtureSOEnv = "MAA_M2_LOADER_FIXTURE_SO"

	// loaderFixtureSrcRel 是夹具源码相对包目录的路径:go test 的工作
	// 目录是包源码目录,testdata/ 下的文件不参与 Go 构建。
	loaderFixtureSrcRel = "testdata/loaderfixture/fixture.c"

	// loaderFixtureSOName 是编译产物文件名。
	loaderFixtureSOName = "libloaderfixture.so"

	// completeOKMarker / missingOKMarker 是两种子进程全部内部断言通过
	// 后打印的结构化标记:父进程不信任子进程「退出 0」本身,只以
	// stdout 实际标记行及其字段为复验依据(纵深防御)。
	completeOKMarker = "MAA_M2_LOADER_FIXTURE_COMPLETE_OK"
	missingOKMarker  = "MAA_M2_LOADER_FIXTURE_MISSING_OK"

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

// buildLoaderFixture 编译独立 C 夹具,返回 .so 绝对路径;missing 为
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
// (MAA-11 步骤 4:记录夹具源码、编译参数、平台和工具链)。编译
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
		t.Skipf("loader fixture 受阻:未找到 C 编译器,夹具构建与装载验证未执行,不计为本平台通过")
	}

	src, err := filepath.Abs(loaderFixtureSrcRel)
	if err != nil {
		t.Fatalf("定位夹具源码 %s 失败: %v", loaderFixtureSrcRel, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("夹具源码 %s 不可访问(仓库基线被破坏,非环境受限): %v", src, err)
	}

	outPath := filepath.Join(t.TempDir(), loaderFixtureSOName)
	args := []string{"-shared", "-fPIC"}
	if missing {
		args = append(args, "-DLOADER_FIXTURE_MISSING")
	}
	args = append(args, "-o", outPath, src)
	commandLine := compiler + " " + strings.Join(args, " ")

	variant := "complete(26 导出全部在场)"
	if missing {
		variant = "missing(-DLOADER_FIXTURE_MISSING,省略 3 个必需符号)"
	}
	t.Logf("loader fixture 构建:变体=%s", variant)
	t.Logf("loader fixture 构建:源码=%s", src)
	t.Logf("loader fixture 构建:平台=%s/%s", runtime.GOOS, runtime.GOARCH)
	t.Logf("loader fixture 构建:编译器=%s", compiler)
	t.Logf("loader fixture 构建:工具链=%s", compilerFirstLine(t, compiler))
	t.Logf("loader fixture 构建:命令行=%s", commandLine)

	cmd := exec.Command(compiler, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("编译 loader fixture 失败(命令行 %s): %v\n%s", commandLine, err, output)
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
		return fmt.Sprintf("%s --version 查询失败: %v", compiler, err)
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
		fmt.Fprintf(os.Stderr, "loader fixture complete child: %s = %q 不是绝对路径(loader 只接受绝对路径)\n", loaderFixtureSOEnv, so)
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
		fmt.Fprintln(os.Stderr, "loader fixture complete child: ResolveAndRegister 成功但未发布 API 表")
		return 1
	}

	caps := table.Capabilities()
	if !caps.Create() {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: Capabilities.Create() = false, want true(AsstCreate 在场)")
		return 1
	}
	if !caps.ImageBgr() {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: Capabilities.ImageBgr() = false, want true(AsstGetImageBgr 在场)")
		return 1
	}
	if caps.AsyncAttachWindow() {
		fmt.Fprintln(os.Stderr, "loader fixture complete child: Capabilities.AsyncAttachWindow() = true, want false(windowsOnly 符号在 Linux 不查找)")
		return 1
	}

	_, lookupErr := lib.Lookup(loaderFixtureProbeSymbol)
	if lookupErr == nil {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: Lookup(%s) 意外成功,必须失败\n", loaderFixtureProbeSymbol)
		return 1
	}
	var sle *SymbolLookupError
	if !errors.As(lookupErr, &sle) {
		fmt.Fprintf(os.Stderr, "loader fixture complete child: Lookup(%s) 的错误 %T 不是 *SymbolLookupError: %v\n",
			loaderFixtureProbeSymbol, lookupErr, lookupErr)
		return 1
	}

	fmt.Printf("%s symbols=%d caps=create=%d,imageBgr=%d,attach=%d\n",
		completeOKMarker, countApplicableSpecs(),
		flagBit(caps.Create()), flagBit(caps.ImageBgr()), flagBit(caps.AsyncAttachWindow()))
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
		fmt.Fprintf(os.Stderr, "loader fixture missing child: %s = %q 不是绝对路径(loader 只接受绝对路径)\n", loaderFixtureSOEnv, so)
		return 1
	}

	lib, err := NewLoader().Open(so)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: Open(%s) failed: %v(缺失发生在符号解析阶段,装载本身必须成功)\n", so, err)
		return 1
	}
	table, err := ResolveAndRegister(lib)
	if err == nil {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: ResolveAndRegister 意外成功,必需符号缺失必须整体失败\n")
		return 1
	}
	if table != nil {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: 失败事务不得发布 API 表, got %v\n", table)
		return 1
	}
	var agg AggregateError
	if !errors.As(err, &agg) {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: errors.As 未命中 AggregateError, got %T: %v\n", err, err)
		return 1
	}
	if len(agg) != len(expectedMissingFixtureSymbols) {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: 聚合错误 %d 项, want %d 项: %v\n",
			len(agg), len(expectedMissingFixtureSymbols), err)
		return 1
	}

	missing := make([]string, 0, len(agg))
	for i, item := range agg {
		var sle *SymbolLookupError
		if !errors.As(item, &sle) {
			fmt.Fprintf(os.Stderr, "loader fixture missing child: 聚合项 %d (%T) 不是 *SymbolLookupError: %v\n", i, item, item)
			return 1
		}
		if sle.LibraryPath != so {
			fmt.Fprintf(os.Stderr, "loader fixture missing child: 聚合项 %d LibraryPath = %q, want %q\n", i, sle.LibraryPath, so)
			return 1
		}
		if sle.Profile != abiProfile {
			fmt.Fprintf(os.Stderr, "loader fixture missing child: 聚合项 %d Profile = %q, want %q\n", i, sle.Profile, abiProfile)
			return 1
		}
		missing = append(missing, sle.Symbol)
	}
	if strings.Join(missing, ",") != strings.Join(expectedMissingFixtureSymbols, ",") {
		fmt.Fprintf(os.Stderr, "loader fixture missing child: 缺失符号集合/顺序 = [%s], want [%s](必须与 fixture.c 的缺失变体及声明顺序严格一致)\n",
			strings.Join(missing, ","), strings.Join(expectedMissingFixtureSymbols, ","))
		return 1
	}

	fmt.Printf("%s symbols=%s\n", missingOKMarker, strings.Join(missing, ","))
	return 0
}

// TestM2LoaderFixtureCompleteLoadsInFreshSubprocess 验收完整夹具装载:
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
func TestM2LoaderFixtureCompleteLoadsInFreshSubprocess(t *testing.T) {
	soPath := buildLoaderFixture(t, false)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     loaderFixtureModeComplete,
		Timeout:  loaderFixtureChildTimeout,
		ExtraEnv: map[string]string{loaderFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("complete 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, completeOKMarker)
	if !ok {
		t.Fatalf("complete 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", completeOKMarker, out.Diagnostics())
	}

	gotSymbols, err := parseMarkerSymbolsCount(line)
	if err != nil {
		t.Fatalf("complete 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	wantSymbols := countApplicableSpecs()
	if gotSymbols != wantSymbols {
		t.Fatalf("complete 标记行 symbols = %d, want %d(标记行与符号清单现算条数不符,防标记行失真):\n%s",
			gotSymbols, wantSymbols, out.Diagnostics())
	}

	create, imageBgr, attach, err := parseMarkerCaps(line)
	if err != nil {
		t.Fatalf("complete 标记行 %q 的 caps 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	if create != 1 || imageBgr != 1 || attach != 0 {
		t.Fatalf("complete 标记行 caps = create=%d,imageBgr=%d,attach=%d, want create=1,imageBgr=1,attach=0:\n%s",
			create, imageBgr, attach, out.Diagnostics())
	}

	t.Logf("complete 子进程装载验证通过(单独标记,不计入 ABI 调用通过统计):%s", line)
}

// TestM2LoaderFixtureMissingRequiredSymbolsReportedTogether 验收缺失
// 夹具的聚合报告:缺失 3 个必需符号时,一次 ResolveAndRegister 调用
// 收集全部失败并整体拒绝发布。父进程断言子进程退出 0(其内部断言
// 全部通过)、标记行在场,并独立复验标记行 symbols 列表与顺序恰为
// expectedMissingFixtureSymbols——与子进程内断言互相独立(纵深防御)。
func TestM2LoaderFixtureMissingRequiredSymbolsReportedTogether(t *testing.T) {
	soPath := buildLoaderFixture(t, true)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     loaderFixtureModeMissing,
		Timeout:  loaderFixtureChildTimeout,
		ExtraEnv: map[string]string{loaderFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("missing 子进程失败(其内部断言未全部通过):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, missingOKMarker)
	if !ok {
		t.Fatalf("missing 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", missingOKMarker, out.Diagnostics())
	}

	got := parseMarkerSymbolsList(line)
	if strings.Join(got, ",") != strings.Join(expectedMissingFixtureSymbols, ",") {
		t.Fatalf("missing 标记行 symbols = [%s], want [%s]:\n%s",
			strings.Join(got, ","), strings.Join(expectedMissingFixtureSymbols, ","), out.Diagnostics())
	}
	t.Logf("missing 子进程聚合报告验证通过(3 项一次收集,顺序与符号清单一致):%s", line)
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
		return 0, fmt.Errorf("标记行缺少 symbols=<n> 字段")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(fields[1], "symbols="))
	if err != nil {
		return 0, fmt.Errorf("symbols 值不是十进制整数: %w", err)
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
func parseMarkerCaps(line string) (create, imageBgr, attach int, err error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "caps=") {
		return 0, 0, 0, fmt.Errorf("标记行缺少 caps=... 字段")
	}
	for _, kv := range strings.Split(strings.TrimPrefix(fields[2], "caps="), ",") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return 0, 0, 0, fmt.Errorf("caps 分量 %q 缺少 key=value 形态", kv)
		}
		n, convErr := strconv.Atoi(value)
		if convErr != nil {
			return 0, 0, 0, fmt.Errorf("caps 分量 %q 的值不是十进制整数: %w", kv, convErr)
		}
		switch key {
		case "create":
			create = n
		case "imageBgr":
			imageBgr = n
		case "attach":
			attach = n
		default:
			return 0, 0, 0, fmt.Errorf("caps 含未知键 %q", key)
		}
	}
	return create, imageBgr, attach, nil
}
