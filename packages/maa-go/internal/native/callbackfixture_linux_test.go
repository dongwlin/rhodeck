//go:build linux

// 本文件是 MAA-13 的独立原生线程 callback fixture 验证:以独立手写 C
// 夹具(testdata/callbackfixture/fixture.c,回调指针类型与锁定的
// AsstCaller.h:53 AsstApiCallback 一致)在 pthread 启动并 join 的原生
// 线程上调用生产 trampoline,验证 Unix 无返回 Go callback 的参数往
// 返、整数 token、details 即时复制与 panic 边界,为 M3 的实例注册表
// 与事件分发器提供可信 callback 层依据。
//
// 与既有检查的关系(分开报告,不得混用统计):
//   - 静态 API checker(MAA-09/10)只核对声明一致性;
//   - loader fixture(MAA_M2_LOADER_FIXTURE_*)只证明装载链路;
//   - 数值/缓冲区 fixture(MAA_M2_ABI_FIXTURE_*,MAA-12)只证明
//     Go→C 方向的真实调用;
//   - 本文件以 MAA_M2_CALLBACK_* 标记报告 C→Go 方向的原生线程回调
//     结果,命名空间独立、单独统计。
//
// 三组用例各自在独立子进程中执行(internal/ffitest,复用包内唯一
// TestMain 的分派与 abifixture 的 2 分钟外部强制超时常量;崩溃/超时
// /死锁判失败),子进程内 NewLoader().Open → Lookup cbfx 导出 →
// purego.RegisterFunc 本地绑定 → 安装测试 sink → 经原生线程触发回调
// → 断言,全部通过后打印标记行;父进程不信任「退出 0」,逐字段复验
// 标记行(纵深防御)。分组:
//   - roundtrip(MAA_M2_CALLBACK_ROUNDTRIP_OK):3 个并发原生线程各带
//     不同负 msg(含 MinInt32)、不同非零整数 token、不同 details,
//     回调参数逐项精确往返;trampoline 地址 1000 次重复获取保持同一
//     (每进程恰好一个,数量不随获取次数增长);
//   - snapshot(MAA_M2_CALLBACK_SNAPSHOT_OK):callback 返回后原生立即
//     整体改写 details 缓冲,Go 快照逐字节保留原始内容——未知字段
//     (futureField 嵌套对象/数组)与超大整数(>2^53)逐字符保留,
//     json.Number 抽查证明不经 float64 往返(生产复制是原始字节复
//     制,本身不做任何 JSON 解析);
//   - panic(MAA_M2_CALLBACK_PANIC_FAULT_OK):sink 处理器内 Go panic
//     被 native 边界 recover 为可见故障记录;C 线程存活并正常 join、
//     fixture 导出正常返回。不宣称 recover 原生崩溃(SIGSEGV 等)或
//     C++ 异常——那类失败由 ffitest 表现为可见的进程失败。
//
// token 纪律:全部用例的 custom_arg 都是本文件内选定的非零整数常量,
// 全程不把任何 Go 对象地址交给原生侧。
//
// 夹具构建入口 buildCallbackFixture 照 MAA-11/12 先例(cc/gcc 回退,
// 无编译器 Skipf——跳过不是通过),额外带 -pthread;源码路径、编译
// 器、工具链与命令行经 t.Logf 记录。夹具语义契约全文见 fixture.c
// 文件头,两侧独立维护,改动必须同步。

package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ebitengine/purego"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/ffitest"
)

// 子进程模式、资源传递环境变量、路径与 stdout 成功标记。
const (
	// 三组子进程模式:每组一组原生线程回调,独立子进程。
	callbackFixtureModeRoundtrip = "callbackfixture-roundtrip"
	callbackFixtureModeSnapshot  = "callbackfixture-snapshot"
	callbackFixtureModePanic     = "callbackfixture-panic"

	// callbackFixtureSOEnv 向子进程传递夹具 .so 的绝对路径;子进程先
	// 断言其为绝对路径,与 loader 只接受绝对路径的冻结策略一致。
	callbackFixtureSOEnv = "MAA_M2_CALLBACK_FIXTURE_SO"

	// callbackFixtureSrcRel 是夹具源码相对包目录的路径:夹具按任务
	// 规定独立放在 maa-go 包根的 testdata/callbackfixture/(与 MAA-11
	// 的 loaderfixture、MAA-12 的 abifixture 分目录),go test 的工作
	// 目录是 internal/native 包源码目录,testdata/ 下的文件不参与 Go
	// 构建。
	callbackFixtureSrcRel = "../../testdata/callbackfixture/fixture.c"

	// callbackFixtureSOName 是编译产物文件名。
	callbackFixtureSOName = "libcallbackfixture.so"

	// 三个标记独立命名空间:与 checker、MAA_M2_LOADER_FIXTURE_*、
	// MAA_M2_ABI_FIXTURE_* 明确分开、单独统计报告。
	roundtripOKMarker  = "MAA_M2_CALLBACK_ROUNDTRIP_OK"
	snapshotOKMarker   = "MAA_M2_CALLBACK_SNAPSHOT_OK"
	panicFaultOKMarker = "MAA_M2_CALLBACK_PANIC_FAULT_OK"
)

// 夹具契约常量(与 fixture.c 文件头的已发表契约一一对应;两侧独立
// 维护,任何一侧改动都必须同步另一侧,否则子进程断言失败)。
const (
	// roundtrip 组:三个互相不同的负 msg,含 MinInt32 一档(符号位
	// 全 1 的极端值)。
	cbFixtureMsgA = int32(-5)
	cbFixtureMsgB = int32(math.MinInt32)
	cbFixtureMsgC = int32(-77777)

	// roundtrip 组:三个互相不同的非零整数 token(本文件内选定的常
	// 量,绝不传 Go 地址)。
	cbFixtureTokenA = uintptr(0x1111)
	cbFixtureTokenB = uintptr(0x2222)
	cbFixtureTokenC = uintptr(0x3333)

	// roundtrip 组:三条互不相同的 details(含未知字段与超大整数)。
	cbFixtureDetailsA = `{"seq":0,"futureField":{"list":[1,"two",{"deep":true}]},"note":"线程A"}`
	cbFixtureDetailsB = `{"seq":1,"big":9007199254740993,"futureField":{"arr":[null,false]}}`
	cbFixtureDetailsC = `{"seq":2,"huge":12345678901234567890,"negBig":-9007199254740993}`

	// snapshot 组:未知字段(futureField 嵌套对象/数组)与超过 2^53
	// 的正/负大整数逐字符保留;json.Number 抽查以证明可精确解码。
	callbackSnapshotPayload = `{"futureField":{"nested":[1,2,{"deep":"快照"}]},"big":9007199254740993,"huge":12345678901234567890,"negBig":-9007199254740993}`

	cbFixtureSnapshotMsg   = int32(-42)
	cbFixtureSnapshotToken = uintptr(0xA5A5A5A5)
	cbFixtureSnapshotFill  = uint8(0x58) // 'X'

	// panic 组:负 msg、非零 token 与 panic 探针文本(故障诊断必须
	// 原样携带该值,证明 recover 到的就是处理器内的 panic)。
	cbFixturePanicMsg    = int32(-7)
	cbFixturePanicToken  = uintptr(0x77)
	cbFixturePanicProbe  = "maa-m2-callback-panic-probe"
	cbFixturePanicDetail = `{"probe":1}`

	// callbackTrampolineFetches 是 trampoline 稳定性的重复获取次数。
	callbackTrampolineFetches = 1000
)

func init() {
	// 子进程与父进程是同一测试二进制:注册发生在 init(),先于
	// TestMain(loaderfixture_linux_test.go,包内唯一)的 Dispatch。
	// 重复注册会 panic,配置错误在启动期即暴露。
	ffitest.Register(callbackFixtureModeRoundtrip, runCallbackFixtureRoundtripChild)
	ffitest.Register(callbackFixtureModeSnapshot, runCallbackFixtureSnapshotChild)
	ffitest.Register(callbackFixtureModePanic, runCallbackFixturePanicChild)
}

// buildCallbackFixture 编译独立原生线程 callback 夹具,返回 .so 绝对
// 路径。编译器发现与失败语义照 MAA-11/12 先例:依次 exec.LookPath 查
// 找 cc、gcc;两者都缺失时 Skipf——夹具构建受阻属于测试环境受限,计
// 为 SKIP(跳过不是通过),真实回调验证未执行,不冒充本平台通过。
// 编译进 t.TempDir(),输出 libcallbackfixture.so,额外带 -pthread(线
// 程例程需要);源码路径、平台、编译器路径、--version 首行与完整命
// 令行全部经 t.Logf 记录。编译失败 Fatalf 并附完整合并输出,不重试。
func buildCallbackFixture(t *testing.T) string {
	t.Helper()

	compiler := ""
	for _, name := range []string{"cc", "gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Skipf("callback fixture 受阻:未找到 C 编译器,夹具构建与原生线程回调验证未执行,不计为本平台通过")
	}

	src, err := filepath.Abs(callbackFixtureSrcRel)
	if err != nil {
		t.Fatalf("定位 callback fixture 源码 %s 失败: %v", callbackFixtureSrcRel, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("callback fixture 源码 %s 不可访问(仓库基线被破坏,非环境受限): %v", src, err)
	}

	outPath := filepath.Join(t.TempDir(), callbackFixtureSOName)
	args := []string{"-shared", "-fPIC", "-pthread", "-o", outPath, src}
	commandLine := compiler + " " + strings.Join(args, " ")
	t.Logf("callback fixture 构建:源码=%s", src)
	t.Logf("callback fixture 构建:平台=%s/%s", runtime.GOOS, runtime.GOARCH)
	t.Logf("callback fixture 构建:编译器=%s", compiler)
	t.Logf("callback fixture 构建:工具链=%s", compilerFirstLine(t, compiler))
	t.Logf("callback fixture 构建:命令行=%s", commandLine)

	cmd := exec.Command(compiler, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("编译 callback fixture 失败(命令行 %s): %v\n%s", commandLine, err, output)
	}
	return outPath
}

// == 子进程辅助 ==

// cbFixtureOpenChild 在子进程内装载夹具:断言 .so 路径为绝对路径并
// 经生产 NewLoader().Open 打开,失败时向 stderr 打印诊断并返回
// ok=false(子进程将以非零退出码失败)。cbfx_* 导出不是 Asst 符号,
// 不做 ResolveAndRegister,由各用例 Lookup 后本地绑定。
func cbFixtureOpenChild(scope, so string) (lib *Library, ok bool) {
	if !filepath.IsAbs(so) {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: %s = %q 不是绝对路径(loader 只接受绝对路径)\n",
			scope, callbackFixtureSOEnv, so)
		return nil, false
	}
	lib, err := NewLoader().Open(so)
	if err != nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: Open(%s) failed: %v\n", scope, so, err)
		return nil, false
	}
	return lib, true
}

// callbackRecord 是测试 sink 捕获到的一次投递快照。
type callbackRecord struct {
	msg     int32
	details []byte
	token   uintptr
}

// callbackRecorder 安装捕获 sink 并返回:record 读取函数(互斥保护,
// 投递来自并发原生线程)与 restore(恢复 nil sink)。
func callbackRecorder() (record func() []callbackRecord, restore func()) {
	var mu sync.Mutex
	var got []callbackRecord
	var sink callbackSink = func(msg int32, details []byte, token uintptr) {
		// details 已是 nativeCallback 在返回前复制的 Go 拥有字节串;
		// 这里再复制一份是测试侧防御,不依赖生产快照的别名行为。
		cp := append([]byte(nil), details...)
		mu.Lock()
		defer mu.Unlock()
		got = append(got, callbackRecord{msg: msg, details: cp, token: token})
	}
	callbackSinkSlot.Store(&sink)
	return func() []callbackRecord {
		mu.Lock()
		defer mu.Unlock()
		return append([]callbackRecord(nil), got...)
	}, func() { callbackSinkSlot.Store(nil) }
}

// checkNoCallbackFault 断言当前无任何边界故障记录(正常路径下
// nativeCallback 不留故障)。
func checkNoCallbackFault(scope string) bool {
	if f := lastCallbackFault.Load(); f != nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 意外的边界故障 kind=%s msg=%d token=%d detail=%q\n",
			scope, f.Kind, f.Msg, f.Token, f.Detail)
		return false
	}
	return true
}

// == 三组子进程入口 ==

// runCallbackFixtureRoundtripChild 验证参数往返:3 个并发原生线程各
// 带不同负 msg(含 MinInt32)、不同非零整数 token、不同 details,回调
// 参数逐项精确对应;trampoline 地址 1000 次重复获取保持同一。
func runCallbackFixtureRoundtripChild() int {
	const scope = "roundtrip"
	lib, ok := cbFixtureOpenChild(scope, os.Getenv(callbackFixtureSOEnv))
	if !ok {
		return 1
	}
	addr, err := lib.Lookup("cbfx_call_on_threads")
	if err != nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: Lookup(cbfx_call_on_threads) failed: %v\n", scope, err)
		return 1
	}
	var callOnThreads func(cb uintptr, msg0 int32, details0 *byte, token0 uintptr,
		msg1 int32, details1 *byte, token1 uintptr,
		msg2 int32, details2 *byte, token2 uintptr) int32
	purego.RegisterFunc(&callOnThreads, addr)

	// trampoline 稳定性:每进程恰好一个,重复获取返回同一地址,数量
	// 不随获取次数无界增长。
	tramp := callbackTrampoline()
	for i := 0; i < callbackTrampolineFetches; i++ {
		if again := callbackTrampoline(); again != tramp {
			fmt.Fprintf(os.Stderr, "callback fixture %s child: 第 %d 次获取 trampoline 地址变化: %#x -> %#x\n",
				scope, i, tramp, again)
			return 1
		}
	}

	record, restore := callbackRecorder()
	defer restore()

	// 一次性并发启动 3 个原生线程(各带独立 msg/details/token),全部
	// join 后返回;返回 0 = 三线程创建、调用、join 全部成功。
	rc := callOnThreads(tramp,
		cbFixtureMsgA, abiCBytes(cbFixtureDetailsA), cbFixtureTokenA,
		cbFixtureMsgB, abiCBytes(cbFixtureDetailsB), cbFixtureTokenB,
		cbFixtureMsgC, abiCBytes(cbFixtureDetailsC), cbFixtureTokenC)
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: cbfx_call_on_threads = %d, want 0(全部线程必须创建、调用并 join)\n", scope, rc)
		return 1
	}

	got := record()
	if len(got) != 3 {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 投递数 = %d, want 3\n", scope, len(got))
		return 1
	}
	byToken := make(map[uintptr]callbackRecord, len(got))
	for _, rec := range got {
		byToken[rec.token] = rec
	}
	for _, want := range []struct {
		msg     int32
		token   uintptr
		details string
	}{
		{cbFixtureMsgA, cbFixtureTokenA, cbFixtureDetailsA},
		{cbFixtureMsgB, cbFixtureTokenB, cbFixtureDetailsB},
		{cbFixtureMsgC, cbFixtureTokenC, cbFixtureDetailsC},
	} {
		rec, ok := byToken[want.token]
		if !ok {
			fmt.Fprintf(os.Stderr, "callback fixture %s child: 缺少 token=%d 的投递(实际 token 集合见上)\n", scope, uint64(want.token))
			return 1
		}
		if rec.msg != want.msg {
			fmt.Fprintf(os.Stderr, "callback fixture %s child: token=%d 的 msg = %d, want %d\n", scope, uint64(want.token), rec.msg, want.msg)
			return 1
		}
		if string(rec.details) != want.details {
			fmt.Fprintf(os.Stderr, "callback fixture %s child: token=%d 的 details 不符:\n got  %q\n want %q\n",
				scope, uint64(want.token), rec.details, want.details)
			return 1
		}
	}
	if !checkNoCallbackFault(scope) {
		return 1
	}

	fmt.Printf("%s threads=3 msgs=%d,%d,%d tokens=%d,%d,%d trampoline=stable fetches=%d deliveries=3 detailbytes=%d,%d,%d\n",
		roundtripOKMarker,
		int64(cbFixtureMsgA), int64(cbFixtureMsgB), int64(cbFixtureMsgC),
		uint64(cbFixtureTokenA), uint64(cbFixtureTokenB), uint64(cbFixtureTokenC),
		callbackTrampolineFetches,
		len(cbFixtureDetailsA), len(cbFixtureDetailsB), len(cbFixtureDetailsC))
	return 0
}

// runCallbackFixtureSnapshotChild 验证 details 即时复制:callback 返
// 回后原生立即整体改写缓冲,Go 快照逐字节保留原文;未知字段与超大整
// 数逐字符保留,json.Number 抽查证明不经 float64 往返。
func runCallbackFixtureSnapshotChild() int {
	const scope = "snapshot"
	lib, ok := cbFixtureOpenChild(scope, os.Getenv(callbackFixtureSOEnv))
	if !ok {
		return 1
	}
	addr, err := lib.Lookup("cbfx_call_then_overwrite")
	if err != nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: Lookup(cbfx_call_then_overwrite) failed: %v\n", scope, err)
		return 1
	}
	var callThenOverwrite func(cb uintptr, msg int32, details *byte, token uintptr, fill uint8) int32
	purego.RegisterFunc(&callThenOverwrite, addr)

	record, restore := callbackRecorder()
	defer restore()

	rc := callThenOverwrite(callbackTrampoline(),
		cbFixtureSnapshotMsg, abiCBytes(callbackSnapshotPayload), cbFixtureSnapshotToken, cbFixtureSnapshotFill)
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: cbfx_call_then_overwrite = %d, want 0\n", scope, rc)
		return 1
	}

	got := record()
	if len(got) != 1 {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 投递数 = %d, want 1\n", scope, len(got))
		return 1
	}
	rec := got[0]
	if rec.msg != cbFixtureSnapshotMsg || rec.token != cbFixtureSnapshotToken {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: msg = %d, want %d; token = %d, want %d\n",
			scope, rec.msg, cbFixtureSnapshotMsg, uint64(rec.token), uint64(cbFixtureSnapshotToken))
		return 1
	}
	// 逐字节比较:原生在 callback 返回后已把缓冲整体改写为 fill,快照
	// 仍必须是原文——证明复制发生在返回之前。
	if !bytes.Equal(rec.details, []byte(callbackSnapshotPayload)) {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 快照与原文逐字节不符(返回后改写影响了快照):\n got  %q\n want %q\n",
			scope, rec.details, callbackSnapshotPayload)
		return 1
	}

	// json.Number 抽查:快照可经 UseNumber 精确解码,超过 2^53 的整数
	// 不经 float64 往返;生产复制本身是原始字节复制,不做任何解析。
	dec := json.NewDecoder(bytes.NewReader(rec.details))
	dec.UseNumber()
	var parsed map[string]any
	if err := dec.Decode(&parsed); err != nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 快照 JSON 解码失败: %v\n", scope, err)
		return 1
	}
	future, ok := parsed["futureField"].(map[string]any)
	if !ok {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 未知字段 futureField 丢失或形状不符: %v\n", scope, parsed["futureField"])
		return 1
	}
	if _, ok := future["nested"].([]any); !ok {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: futureField.nested 丢失或不是数组: %v\n", scope, future["nested"])
		return 1
	}
	for _, check := range []struct{ key, want string }{
		{"big", "9007199254740993"},
		{"huge", "12345678901234567890"},
		{"negBig", "-9007199254740993"},
	} {
		num, ok := parsed[check.key].(json.Number)
		if !ok {
			fmt.Fprintf(os.Stderr, "callback fixture %s child: 字段 %q 不是 json.Number: %v\n", scope, check.key, parsed[check.key])
			return 1
		}
		if num.String() != check.want {
			fmt.Fprintf(os.Stderr, "callback fixture %s child: 字段 %q = %s, want %s(大整数被破坏,疑似 float64 往返)\n",
				scope, check.key, num.String(), check.want)
			return 1
		}
	}
	if !checkNoCallbackFault(scope) {
		return 1
	}

	fmt.Printf("%s bytes=%d msg=%d token=%d fill=%d big=9007199254740993 huge=12345678901234567890 negBig=-9007199254740993 future=1\n",
		snapshotOKMarker,
		len(callbackSnapshotPayload), int64(cbFixtureSnapshotMsg),
		uint64(cbFixtureSnapshotToken), int64(cbFixtureSnapshotFill))
	return 0
}

// runCallbackFixturePanicChild 验证 panic 边界:sink 处理器内的 Go
// panic 被 native 边界 recover 为可见故障记录;C 线程存活并正常
// join、fixture 导出正常返回。不宣称 recover 原生崩溃或 C++ 异常。
func runCallbackFixturePanicChild() int {
	const scope = "panic"
	lib, ok := cbFixtureOpenChild(scope, os.Getenv(callbackFixtureSOEnv))
	if !ok {
		return 1
	}
	addr, err := lib.Lookup("cbfx_call_on_thread")
	if err != nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: Lookup(cbfx_call_on_thread) failed: %v\n", scope, err)
		return 1
	}
	var callOnThread func(cb uintptr, msg int32, details *byte, token uintptr) int32
	purego.RegisterFunc(&callOnThread, addr)

	var invoked atomic.Bool
	var sink callbackSink = func(msg int32, details []byte, token uintptr) {
		invoked.Store(true)
		panic(cbFixturePanicProbe)
	}
	callbackSinkSlot.Store(&sink)
	defer callbackSinkSlot.Store(nil)

	// 处理器 panic 被 nativeCallback 顶部的 defer/recover 拦截并转为
	// 故障记录后,回调正常返回:C 线程继续置 invoked、改写缓冲、退出,
	// join 成功,导出返回 0。
	rc := callOnThread(callbackTrampoline(),
		cbFixturePanicMsg, abiCBytes(cbFixturePanicDetail), cbFixturePanicToken)
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: cbfx_call_on_thread = %d, want 0(panic 被 recover 后 C 线程必须存活并正常 join)\n", scope, rc)
		return 1
	}
	if !invoked.Load() {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: sink 处理器未被调用\n", scope)
		return 1
	}
	f := lastCallbackFault.Load()
	if f == nil {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 处理器 panic 后未记录任何边界故障\n", scope)
		return 1
	}
	if f.Kind != callbackFaultPanic {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 故障 kind = %s, want %s\n", scope, f.Kind, callbackFaultPanic)
		return 1
	}
	if f.Msg != cbFixturePanicMsg || f.Token != cbFixturePanicToken {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 故障 msg = %d, want %d; token = %d, want %d\n",
			scope, f.Msg, cbFixturePanicMsg, uint64(f.Token), uint64(cbFixturePanicToken))
		return 1
	}
	if !strings.Contains(f.Detail, cbFixturePanicProbe) {
		fmt.Fprintf(os.Stderr, "callback fixture %s child: 故障诊断 %q 未携带 panic 值 %q\n", scope, f.Detail, cbFixturePanicProbe)
		return 1
	}

	fmt.Printf("%s msg=%d token=%d export=0 recovered=1\n",
		panicFaultOKMarker, int64(cbFixturePanicMsg), uint64(cbFixturePanicToken))
	return 0
}

// == 父进程用例:每组独立子进程,复验标记行与关键字段 ==

// TestM2CallbackNativeThreadRoundtrip 验证独立原生线程进入 Unix 无返
// 回 Go callback:负消息 ID(含 MinInt32)、details 全文与非零整数
// token 精确对应,多线程多 token 多消息;trampoline 地址 1000 次重复
// 获取保持同一。父进程独立复验标记行关键字段(纵深防御)。
func TestM2CallbackNativeThreadRoundtrip(t *testing.T) {
	soPath := buildCallbackFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     callbackFixtureModeRoundtrip,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{callbackFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("roundtrip 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, roundtripOKMarker)
	if !ok {
		t.Fatalf("roundtrip 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", roundtripOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, roundtripOKMarker)
	if err != nil {
		t.Fatalf("roundtrip 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	diag := out.Diagnostics()
	abiCheckField(t, fields, "threads", "3", diag)
	abiCheckField(t, fields, "msgs",
		fmt.Sprintf("%d,%d,%d", cbFixtureMsgA, cbFixtureMsgB, cbFixtureMsgC), diag)
	abiCheckField(t, fields, "tokens",
		fmt.Sprintf("%d,%d,%d", uint64(cbFixtureTokenA), uint64(cbFixtureTokenB), uint64(cbFixtureTokenC)), diag)
	abiCheckField(t, fields, "trampoline", "stable", diag)
	abiCheckField(t, fields, "fetches", strconv.Itoa(callbackTrampolineFetches), diag)
	abiCheckField(t, fields, "deliveries", "3", diag)
	abiCheckField(t, fields, "detailbytes",
		fmt.Sprintf("%d,%d,%d", len(cbFixtureDetailsA), len(cbFixtureDetailsB), len(cbFixtureDetailsC)), diag)

	t.Logf("roundtrip 子进程原生线程回调验证通过(3 线程、负 msg 含 MinInt32、整数 token 往返、trampoline 稳定):%s", line)
}

// TestM2CallbackSnapshotBeforeNativeRewrite 验证 details 即时复制:
// 原生在 callback 返回后整体改写缓冲,Go 快照保留原文、未知字段与超
// 大整数逐字符保留。父进程独立复验标记行关键字段。
func TestM2CallbackSnapshotBeforeNativeRewrite(t *testing.T) {
	soPath := buildCallbackFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     callbackFixtureModeSnapshot,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{callbackFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("snapshot 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, snapshotOKMarker)
	if !ok {
		t.Fatalf("snapshot 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", snapshotOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, snapshotOKMarker)
	if err != nil {
		t.Fatalf("snapshot 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	diag := out.Diagnostics()
	abiCheckField(t, fields, "bytes", strconv.Itoa(len(callbackSnapshotPayload)), diag)
	abiCheckField(t, fields, "msg", strconv.FormatInt(int64(cbFixtureSnapshotMsg), 10), diag)
	abiCheckField(t, fields, "token", strconv.FormatUint(uint64(cbFixtureSnapshotToken), 10), diag)
	abiCheckField(t, fields, "fill", strconv.Itoa(int(cbFixtureSnapshotFill)), diag)
	abiCheckField(t, fields, "big", "9007199254740993", diag)
	abiCheckField(t, fields, "huge", "12345678901234567890", diag)
	abiCheckField(t, fields, "negBig", "-9007199254740993", diag)
	abiCheckField(t, fields, "future", "1", diag)

	t.Logf("snapshot 子进程即时复制验证通过(返回后改写不影响快照、未知字段与大整数逐字符保留):%s", line)
}

// TestM2CallbackPanicContainedAtBoundary 验证 panic 边界:callback 内
// Go panic 不越过 native 边界,C 线程存活并正常 join、fixture 导出
// 正常返回,故障记录内容可见。不宣称 recover 原生崩溃或 C++ 异常。
func TestM2CallbackPanicContainedAtBoundary(t *testing.T) {
	soPath := buildCallbackFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     callbackFixtureModePanic,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{callbackFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("panic 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, panicFaultOKMarker)
	if !ok {
		t.Fatalf("panic 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", panicFaultOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, panicFaultOKMarker)
	if err != nil {
		t.Fatalf("panic 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	diag := out.Diagnostics()
	abiCheckField(t, fields, "msg", strconv.FormatInt(int64(cbFixturePanicMsg), 10), diag)
	abiCheckField(t, fields, "token", strconv.FormatUint(uint64(cbFixturePanicToken), 10), diag)
	abiCheckField(t, fields, "export", "0", diag)
	abiCheckField(t, fields, "recovered", "1", diag)

	t.Logf("panic 子进程边界验证通过(Go panic recover 为可见故障、C 线程 join、导出正常返回):%s", line)
}
