//go:build linux

// 本文件以独立手写 C 夹具验证真实 ABI:签名逐字对应锁定的 AsstCaller.h,
// 覆盖数值往返、参数位置、void 副作用与缓冲区协议。

package native

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/ffitest"
)

// 子进程模式、资源传递环境变量、路径与 stdout 成功标记。
const (
	// 四组子进程模式:每组一组真实 FFI,独立子进程。
	abiFixtureModeNumeric     = "abifixture-numeric"
	abiFixtureModeParams      = "abifixture-params"
	abiFixtureModeVoidStrings = "abifixture-void-strings"
	abiFixtureModeBuffers     = "abifixture-buffers"

	// abiFixtureSOEnv 向子进程传递夹具.so 的绝对路径;子进程先断言
	// 其为绝对路径,与 loader 只接受绝对路径的冻结策略一致。
	abiFixtureSOEnv = "MAA_ABI_FIXTURE_SO"

	// abiFixtureSrcRel 是夹具源码相对包目录的路径:夹具按任务规定
	// 独立放在 maa-go 包根的 testdata/abifixture/(与
	// internal/native/testdata/loaderfixture 分目录),go test 的工作
	// 目录是 internal/native 包源码目录,testdata/ 下的文件不参与 Go
	// 构建。
	abiFixtureSrcRel = "../../testdata/abifixture/fixture.c"

	// abiFixtureSOName 是编译产物文件名。
	abiFixtureSOName = "libabifixture.so"

	// 四个标记独立命名空间:与 loader fixture 的 MAA_LOADER_FIXTURE_*
	// 和静态 checker 结果明确分开,单独统计报告。
	numericOKMarker     = "MAA_ABI_FIXTURE_NUMERIC_OK"
	paramsOKMarker      = "MAA_ABI_FIXTURE_PARAMS_OK"
	voidStringsOKMarker = "MAA_ABI_FIXTURE_VOID_STRINGS_OK"
	buffersOKMarker     = "MAA_ABI_FIXTURE_BUFFERS_OK"

	// abiFixtureChildTimeout 是父进程强制的子进程 wall-clock 上限:
	// 真实调用毫秒级完成,2 分钟只防御环境异常导致的挂起。
	abiFixtureChildTimeout = 2 * time.Minute
)

// 夹具契约常量(与 fixture.c 文件头的已发表契约一一对应;两侧独立
// 维护,任何一侧改动都必须同步另一侧,否则子进程断言失败)。
const (
	// abiFixtureVersionFormat 是 AsstGetVersion 的版本格式,计数从 1 起。
	abiFixtureVersionFormat = "v6.20.1-abifixture#%d"

	// abiFixtureDumpFormat 是 GetUUID(handle==0)的状态 dump 格式:
	// log 计数、extras 的 name/json、最后一次 Destroy 的 handle。
	abiFixtureDumpFormat = "log=%d,extras=%s=%s,destroy=%d"

	// abiFixtureCreateHandle 是 AsstCreate 的固定非零返回值
	// (0x00C0FFEE),也是 AsstCreateEx 折叠的基值。
	abiFixtureCreateHandle = uintptr(0x00C0FFEE)

	// abiFixtureUUIDText 是固定 UUID 内容,16 字节,原生不写 NUL。
	abiFixtureUUIDText = "uuid-no-nul-1234"
)

// abiFixturePNG / abiFixtureBGR 是固定图像内容(16/12 字节)。
var (
	abiFixturePNG = [16]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52}
	abiFixtureBGR = [12]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xAA, 0xBB}
)

func init() {
	// 子进程与父进程是同一测试二进制:注册发生在 init(),先于
	// TestMain(loaderfixture_linux_test.go,包内唯一)的 Dispatch。
	// 重复注册会 panic,配置错误在启动期即暴露。
	ffitest.Register(abiFixtureModeNumeric, runABIFixtureNumericChild)
	ffitest.Register(abiFixtureModeParams, runABIFixtureParamsChild)
	ffitest.Register(abiFixtureModeVoidStrings, runABIFixtureVoidStringsChild)
	ffitest.Register(abiFixtureModeBuffers, runABIFixtureBuffersChild)
}

// buildABIFixture 编译独立 C ABI 夹具,返回.so 绝对路径。
//
// 编译器发现:依次 exec.LookPath 查找 cc、gcc;两者都缺失时 Skipf
// ——夹具构建受阻属于测试环境受限,计为 SKIP(跳过不是通过),真实
// ABI 调用验证未执行,不冒充本平台通过。C 编译器是本验证的测试环境
// 工具链需求,与生产构建无关:生产二进制 CGO_ENABLED=0、不经 cgo。
//
// 编译进 t.TempDir(),输出 libabifixture.so;源码路径、平台、编译器
// 路径、--version 首行与完整命令行全部经 t.Logf 记录。编译失败
// Fatalf 并附完整合并输出,不重试。
func buildABIFixture(t *testing.T) string {
	t.Helper()

	compiler := ""
	for _, name := range []string{"cc", "gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Skipf("abi fixture 受阻:未找到 C 编译器,夹具构建与真实 ABI 调用验证未执行,不计为本平台通过")
	}

	src, err := filepath.Abs(abiFixtureSrcRel)
	if err != nil {
		t.Fatalf("定位 abi fixture 源码 %s 失败: %v", abiFixtureSrcRel, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("abi fixture 源码 %s 不可访问(仓库基线被破坏,非环境受限): %v", src, err)
	}

	outPath := filepath.Join(t.TempDir(), abiFixtureSOName)
	args := []string{"-shared", "-fPIC", "-o", outPath, src}
	commandLine := compiler + " " + strings.Join(args, " ")
	t.Logf("abi fixture 构建:源码=%s", src)
	t.Logf("abi fixture 构建:平台=%s/%s", runtime.GOOS, runtime.GOARCH)
	t.Logf("abi fixture 构建:编译器=%s", compiler)
	t.Logf("abi fixture 构建:工具链=%s", compilerFirstLine(t, compiler))
	t.Logf("abi fixture 构建:命令行=%s", commandLine)

	cmd := exec.Command(compiler, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("编译 abi fixture 失败(命令行 %s): %v\n%s", commandLine, err, output)
	}
	return outPath
}

// == 数值折叠的 Go 镜像:与 fixture.c 的 C 实现逐项对应,互不从对方
// 生成;uint32 乘法在 Go 中回绕,与 C 无符号乘法一致。 ==

// abiFoldAsyncConnect 镜像 AsstAsyncConnect 的 5 参数确定性折叠:
// handle 低 32 位、三个 strlen 与 block,各位置运算结构不同(address
// 步经 rotate,避免与 config 步可交换,槽位对调必变)。
func abiFoldAsyncConnect(handle uintptr, adbPath, address, config string, block uint8) int32 {
	f := uint32(handle)
	f ^= 0x9E3779B9
	f = (f ^ uint32(len(adbPath))) * 16777619
	f = abiRotl32(f^uint32(len(address)), 7)
	f = (f ^ uint32(len(config))) * 2654435761
	f ^= uint32(block)
	return int32(f)
}

// abiRotl32 是 32 位循环左移,镜像 C 侧 abifx_rotl32。
func abiRotl32(v uint32, n uint) uint32 { return v<<n | v>>(32-n) }

// abiFoldAsyncAttachWindow 镜像 AsstAsyncAttachWindow 的六参数折叠:
// 9 个分量(handle、hwnd、3 个 method 的低/高 32 位、block)旋转位
// 数各不相同,高 32 位独立参与,槽位错位即结果不同。
func abiFoldAsyncAttachWindow(handle, hwnd uintptr, screencap, mouse, keyboard uint64, block uint8) int32 {
	acc := uint32(0x12345678)
	acc ^= abiRotl32(uint32(handle), 1)
	acc ^= abiRotl32(uint32(hwnd), 3)
	acc ^= abiRotl32(uint32(screencap), 5)
	acc ^= abiRotl32(uint32(screencap>>32), 7)
	acc ^= abiRotl32(uint32(mouse), 9)
	acc ^= abiRotl32(uint32(mouse>>32), 11)
	acc ^= abiRotl32(uint32(keyboard), 13)
	acc ^= abiRotl32(uint32(keyboard>>32), 15)
	acc ^= abiRotl32(uint32(block), 17)
	return int32(acc)
}

// == 子进程辅助 ==

// abiFixtureOpenChild 在子进程内装载夹具并完成解析注册:断言.so 路
// 径为绝对路径(NewLoader().Open → ResolveAndRegister),失败时向
// stderr 打印诊断并返回 ok=false(子进程将以非零退出码失败)。
func abiFixtureOpenChild(scope, so string) (lib *Library, table *APITable, ok bool) {
	if !filepath.IsAbs(so) {
		fmt.Fprintf(os.Stderr, "abi fixture %s child: %s = %q 不是绝对路径(loader 只接受绝对路径)\n",
			scope, abiFixtureSOEnv, so)
		return nil, nil, false
	}
	lib, err := NewLoader().Open(so)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abi fixture %s child: Open(%s) failed: %v\n", scope, so, err)
		return nil, nil, false
	}
	table, err = ResolveAndRegister(lib)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abi fixture %s child: ResolveAndRegister(%s) failed: %v\n", scope, so, err)
		return nil, nil, false
	}
	if table == nil {
		fmt.Fprintf(os.Stderr, "abi fixture %s child: ResolveAndRegister 成功但未发布 API 表\n", scope)
		return nil, nil, false
	}
	return lib, table, true
}

// abiCBytes 构造带 NUL 终止的 C 字符串缓冲区;返回的 *byte 指向的
// 数组在调用期间由该指针保持存活(raw 调用同步返回)。
func abiCBytes(s string) *byte {
	b := make([]byte, len(s)+1)
	copy(b, s)
	return &b[0]
}

// abiCString 有界读取借用 C 字符串(遇 NUL 停,至多 limit 字节),
// 复制为 Go 数据;不 free 原生内存、不外泄视图。
func abiCString(p *byte, limit int) string {
	if p == nil {
		return ""
	}
	out := make([]byte, 0, 64)
	for i := 0; i < limit; i++ {
		c := *(*byte)(unsafe.Add(unsafe.Pointer(p), i))
		if c == 0 {
			break
		}
		out = append(out, c)
	}
	return string(out)
}

// == 四组子进程入口 ==

// runABIFixtureNumericChild 验证数值往返:MaxUint64 哨兵、uint8 的
// 0/1/255、恒 2 的非零真值、小端负 int32 往返与未深测符号的确定返回。
func runABIFixtureNumericChild() int {
	_, table, ok := abiFixtureOpenChild("numeric", os.Getenv(abiFixtureSOEnv))
	if !ok {
		return 1
	}
	f := table.funcs()

	if got := f.GetNullSize(); got != math.MaxUint64 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: GetNullSize() = %#x, want MaxUint64\n", got)
		return 1
	}
	// uint8 往返:handle 0/1/0xFF → 0/1/255,无截断或符号错误。
	if got := f.Running(0); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Running(0) = %d, want 0\n", got)
		return 1
	}
	if got := f.Running(1); got != 1 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Running(1) = %d, want 1\n", got)
		return 1
	}
	if got := f.Running(0xFF); got != 255 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Running(0xFF) = %d, want 255\n", got)
		return 1
	}
	// 非 0/1 的真值:bool 解释必须是非零为真,不能依赖具体数值。
	if got := f.Connected(0); got != 2 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Connected(0) = %d, want 2\n", got)
		return 1
	}
	// 负 int32 小端往返:符号位与字节序同时验证。
	for _, want := range []int32{-1, math.MinInt32, 0x12345678} {
		var raw [4]byte
		binary.LittleEndian.PutUint32(raw[:], uint32(want))
		got := f.AppendTask(0x21, abiCBytes("echo-i32"), &raw[0])
		if got != want {
			fmt.Fprintf(os.Stderr, "abi fixture numeric child: AppendTask(echo-i32, %d) = %d, want %d\n", want, got, want)
			return 1
		}
	}
	if got := f.AppendTask(0x21, abiCBytes("other"), abiCBytes("x")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: AppendTask(other) = %d, want 0\n", got)
		return 1
	}
	// 未深测符号的确定返回(契约常量)。
	if got := f.Start(0x21); got != 11 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Start = %d, want 11\n", got)
		return 1
	}
	if got := f.Stop(0x21); got != 12 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Stop = %d, want 12\n", got)
		return 1
	}
	if got := f.BackToHome(0x21); got != 13 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: BackToHome = %d, want 13\n", got)
		return 1
	}
	if got := f.AsyncScreencap(0x21, 0); got != 100 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: AsyncScreencap(block=0) = %d, want 100\n", got)
		return 1
	}
	if got := f.AsyncScreencap(0x21, 1); got != 101 {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: AsyncScreencap(block=1) = %d, want 101\n", got)
		return 1
	}
	if got := f.Create(); got != abiFixtureCreateHandle {
		fmt.Fprintf(os.Stderr, "abi fixture numeric child: Create = %#x, want %#x\n", got, abiFixtureCreateHandle)
		return 1
	}

	fmt.Printf("%s nullsize=%d running0=%d running1=%d running255=%d connected=%d echo_min=%d screencap1=%d create=%d\n",
		numericOKMarker,
		uint64(math.MaxUint64), 0, 1, 255, 2,
		int64(math.MinInt32), 101, uint64(abiFixtureCreateHandle))
	return 0
}

// runABIFixtureParamsChild 验证参数传递:x/y 槽位次序(互相不同的负
// 值)、5 参数折叠、CreateEx token 往返、六参数形状本地绑定调用。
func runABIFixtureParamsChild() int {
	lib, table, ok := abiFixtureOpenChild("params", os.Getenv(abiFixtureSOEnv))
	if !ok {
		return 1
	}
	f := table.funcs()

	// 4 参数槽位次序:block==0 返回 x、block==1 返回 y;两个互相不同
	// 的负值使 x/y 错位可辨,同时验证 int32 符号位。
	const (
		fx = int32(-11)
		fy = int32(-22)
	)
	if got := f.AsyncClick(0x22, fx, fy, 0); got != fx {
		fmt.Fprintf(os.Stderr, "abi fixture params child: AsyncClick(block=0) = %d, want x=%d(第 2 槽位)\n", got, fx)
		return 1
	}
	if got := f.AsyncClick(0x22, fx, fy, 1); got != fy {
		fmt.Fprintf(os.Stderr, "abi fixture params child: AsyncClick(block=1) = %d, want y=%d(第 3 槽位)\n", got, fy)
		return 1
	}

	// 5 参数折叠:handle 取高 32 位非零值;字符串长度进入折叠,对调
	// address/config(长度互换)必须得到不同结果 —— 参数位置敏感。
	const hConnect = uintptr(0x1122334455667788)
	adbPath, address, config := "/data/adb/abi", "127.0.0.1:5555", "Android"
	wantConnect := abiFoldAsyncConnect(hConnect, adbPath, address, config, 1)
	gotConnect := f.AsyncConnect(hConnect, abiCBytes(adbPath), abiCBytes(address), abiCBytes(config), 1)
	if gotConnect != wantConnect {
		fmt.Fprintf(os.Stderr, "abi fixture params child: AsyncConnect = %d, want 折叠 %d\n", gotConnect, wantConnect)
		return 1
	}
	wantConnect2 := abiFoldAsyncConnect(hConnect, adbPath, config, address, 1)
	gotConnect2 := f.AsyncConnect(hConnect, abiCBytes(adbPath), abiCBytes(config), abiCBytes(address), 1)
	if gotConnect2 != wantConnect2 {
		fmt.Fprintf(os.Stderr, "abi fixture params child: AsyncConnect(对调) = %d, want 折叠 %d\n", gotConnect2, wantConnect2)
		return 1
	}
	if gotConnect2 == gotConnect {
		fmt.Fprintf(os.Stderr, "abi fixture params child: 两次 AsyncConnect 折叠重合(%d),参数位置敏感性无法证明\n", gotConnect)
		return 1
	}

	// CreateEx token 往返:callback 与 token 槽位错位即结果不同。
	const (
		cb    = uintptr(0x7777)
		token = uintptr(0xDEADBEEF)
	)
	wantCreateEx := abiFixtureCreateHandle ^ cb ^ (token << 1)
	if got := f.CreateEx(cb, token); got != wantCreateEx {
		fmt.Fprintf(os.Stderr, "abi fixture params child: CreateEx = %#x, want %#x(callback/token 折叠)\n", got, wantCreateEx)
		return 1
	}

	// 六参数形状:AsstAsyncAttachWindow 是 windowsOnly 符号,
	// ResolveAndRegister 在 Linux 不注册;以 purego.RegisterFunc 对
	// lib.Lookup 到的地址做本地绑定后调用。这只证明 SysV ABI 下六参
	// 数形状(6 个整数寄存器槽,恰好占满)传递正确,不代表 Windows
	// 调用约定运行通过;不据此声明 Windows 能力。
	addrAttach, err := lib.Lookup("AsstAsyncAttachWindow")
	if err != nil {
		fmt.Fprintf(os.Stderr, "abi fixture params child: Lookup(AsstAsyncAttachWindow) failed: %v\n", err)
		return 1
	}
	var attach func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32
	purego.RegisterFunc(&attach, addrAttach)

	const (
		hAttach = uintptr(0xA)
		hwnd    = uintptr(0xB)
	)
	sc, mouse, kb := uint64(0x1_0000_0002), uint64(0x2_0000_0010), uint64(0x1_0000_0004)
	wantAttach := abiFoldAsyncAttachWindow(hAttach, hwnd, sc, mouse, kb, 1)
	gotAttach := attach(hAttach, hwnd, sc, mouse, kb, 1)
	if gotAttach != wantAttach {
		fmt.Fprintf(os.Stderr, "abi fixture params child: AsyncAttachWindow = %d, want 折叠 %d(六参数形状)\n", gotAttach, wantAttach)
		return 1
	}
	// 高 32 位参与:仅改 mouse 的高位字,结果必须不同。
	mouseHigh := uint64(0x3_0000_0010)
	wantAttachHigh := abiFoldAsyncAttachWindow(hAttach, hwnd, sc, mouseHigh, kb, 1)
	gotAttachHigh := attach(hAttach, hwnd, sc, mouseHigh, kb, 1)
	if gotAttachHigh != wantAttachHigh {
		fmt.Fprintf(os.Stderr, "abi fixture params child: AsyncAttachWindow(高位字) = %d, want 折叠 %d\n", gotAttachHigh, wantAttachHigh)
		return 1
	}
	if gotAttachHigh == gotAttach {
		fmt.Fprintf(os.Stderr, "abi fixture params child: 两次 AsyncAttachWindow 折叠重合(%d),高 32 位未参与,无法证明 64 位 method 传递\n", gotAttach)
		return 1
	}

	fmt.Printf("%s click0=%d click1=%d connect1=%d connect2=%d createex=%d attach1=%d attach2=%d\n",
		paramsOKMarker,
		int64(fx), int64(fy), int64(gotConnect), int64(gotConnect2),
		uint64(wantCreateEx), int64(gotAttach), int64(gotAttachHigh))
	return 0
}

// runABIFixtureVoidStringsChild 验证 void 与字符串:void 副作用经
// dump 通道可观察、借用版本字符串复制后不受原生改写影响、字符串输
// 入内容真实到达。
func runABIFixtureVoidStringsChild() int {
	_, table, ok := abiFixtureOpenChild("void-strings", os.Getenv(abiFixtureSOEnv))
	if !ok {
		return 1
	}
	f := table.funcs()

	// 借用字符串:GetVersion 返回原生静态缓冲的借用指针。首次立即
	// 复制为 Go 数据;第二次调用会改写原生缓冲,已复制的 Go 内容必须
	// 不受影响(不 free、不依赖别名)。
	first := abiCString(f.GetVersion(), 128)
	wantFirst := fmt.Sprintf(abiFixtureVersionFormat, 1)
	if first != wantFirst {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: GetVersion 首次 = %q, want %q\n", first, wantFirst)
		return 1
	}
	second := abiCString(f.GetVersion(), 128)
	wantSecond := fmt.Sprintf(abiFixtureVersionFormat, 2)
	if second != wantSecond {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: GetVersion 二次 = %q, want %q(原生缓冲已被改写)\n", second, wantSecond)
		return 1
	}
	if first != wantFirst {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: 首次副本 %q 在原生改写后变为非期望值,借用内容被错误共享\n", first)
		return 1
	}

	// 状态 dump 读取通道:GetUUID(handle==0),内容不含 NUL,返回
	// 精确长度。
	dumpBuf := make([]byte, 256)
	readDump := func() (string, bool) {
		n := f.GetUUID(0, &dumpBuf[0], uint64(len(dumpBuf)))
		if n == math.MaxUint64 || n > uint64(len(dumpBuf)) {
			fmt.Fprintf(os.Stderr, "abi fixture void-strings child: dump 读取失败 n=%d\n", n)
			return "", false
		}
		return string(dumpBuf[:n]), true
	}
	// 初始状态:未 Log、未 extras、未 Destroy。
	gotDump, ok := readDump()
	if !ok || gotDump != fmt.Sprintf(abiFixtureDumpFormat, 0, "", "", 0) {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: 初始 dump = %q, want %q\n",
			gotDump, fmt.Sprintf(abiFixtureDumpFormat, 0, "", "", 0))
		return 1
	}

	// void 副作用:Destroy / SetConnectionExtras / Log 均无返回值,
	// 不读取任何成功码;效果只能经 dump 通道观察。
	f.Destroy(0x77AA55)
	f.SetConnectionExtras(abiCBytes("abifx"), abiCBytes(`{"k":1}`))
	f.Log(abiCBytes("debug"), abiCBytes("abi-log-msg-1"))
	f.Log(abiCBytes("info"), abiCBytes("abi-log-msg-2"))
	gotDump, ok = readDump()
	if !ok || gotDump != fmt.Sprintf(abiFixtureDumpFormat, 2, "abifx", `{"k":1}`, 0x77AA55) {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: 副作用 dump = %q, want %q\n",
			gotDump, fmt.Sprintf(abiFixtureDumpFormat, 2, "abifx", `{"k":1}`, 0x77AA55))
		return 1
	}
	// 再次 Destroy:dump 记录最后一次 handle。
	f.Destroy(0xDEAD)
	gotDump, ok = readDump()
	if !ok || gotDump != fmt.Sprintf(abiFixtureDumpFormat, 2, "abifx", `{"k":1}`, 0xDEAD) {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: 二次 dump = %q, want %q\n",
			gotDump, fmt.Sprintf(abiFixtureDumpFormat, 2, "abifx", `{"k":1}`, 0xDEAD))
		return 1
	}

	// 字符串输入内容到达:内容相等比较,整数参数错位即失败。
	if got := f.SetUserDir(abiCBytes("/abi-fixture-userdir")); got != 1 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetUserDir(契约值) = %d, want 1\n", got)
		return 1
	}
	if got := f.SetUserDir(abiCBytes("/wrong")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetUserDir(错误值) = %d, want 0\n", got)
		return 1
	}
	if got := f.LoadResource(abiCBytes("/abi-fixture-resource")); got != 1 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: LoadResource(契约值) = %d, want 1\n", got)
		return 1
	}
	if got := f.LoadResource(abiCBytes("/wrong")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: LoadResource(错误值) = %d, want 0\n", got)
		return 1
	}
	if got := f.SetStaticOption(1, abiCBytes("static-option-value")); got != 1 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetStaticOption(key=1, 契约值) = %d, want 1\n", got)
		return 1
	}
	if got := f.SetStaticOption(2, abiCBytes("static-option-value")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetStaticOption(key=2, 契约值) = %d, want 0(key 槽位可辨)\n", got)
		return 1
	}
	if got := f.SetStaticOption(1, abiCBytes("wrong")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetStaticOption(key=1, 错误值) = %d, want 0\n", got)
		return 1
	}
	if got := f.SetInstanceOption(0x33, 6, abiCBytes("clienttype-value")); got != 1 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetInstanceOption(key=6, 契约值) = %d, want 1\n", got)
		return 1
	}
	if got := f.SetInstanceOption(0x33, 5, abiCBytes("clienttype-value")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetInstanceOption(key=5, 契约值) = %d, want 0\n", got)
		return 1
	}
	if got := f.SetTaskParams(0x33, 42, abiCBytes("{}")); got != 1 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetTaskParams(id=42) = %d, want 1\n", got)
		return 1
	}
	if got := f.SetTaskParams(0x33, 43, abiCBytes("{}")); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture void-strings child: SetTaskParams(id=43) = %d, want 0(id 槽位可辨)\n", got)
		return 1
	}

	fmt.Printf("%s copy=%s rewrite=%s logcount=%d destroy=%d\n",
		voidStringsOKMarker, first, second, 2, 0xDEAD)
	return 0
}

// runABIFixtureBuffersChild 验证缓冲区协议:NULL/不足/恰好/超容量、
// 合法零与 NullSize 可辨、uint64 高位不截断、无 NUL UUID 按长度读
// 取、任务列表按 int32 元素数。
func runABIFixtureBuffersChild() int {
	_, table, ok := abiFixtureOpenChild("buffers", os.Getenv(abiFixtureSOEnv))
	if !ok {
		return 1
	}
	f := table.funcs()

	nullsize := f.GetNullSize()
	if nullsize != math.MaxUint64 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetNullSize() = %#x, want MaxUint64\n", nullsize)
		return 1
	}

	// GetImage:16 字节 PNG,单位字节。
	const hImg = uintptr(0x1234)
	img := make([]byte, 16)
	// 容量不足(15/0)与 NULL buffer → NullSize;0 不是容量查询协议。
	if got := f.GetImage(hImg, unsafe.Pointer(&img[0]), 15); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=15) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetImage(hImg, unsafe.Pointer(&img[0]), 0); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=0) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetImage(hImg, nil, 16); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(nil) = %#x, want NullSize\n", got)
		return 1
	}
	// 恰好足够:拷贝并返回字节数,内容逐字节一致。
	if got := f.GetImage(hImg, unsafe.Pointer(&img[0]), 16); got != 16 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=16) = %d, want 16\n", got)
		return 1
	}
	if !bytes.Equal(img, abiFixturePNG[:]) {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage 内容不符: %x\n", img)
		return 1
	}
	// 超容量:仍返回实际字节数,只写前 16 字节。
	big := make([]byte, 32)
	if got := f.GetImage(hImg, unsafe.Pointer(&big[0]), 32); got != 16 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=32) = %d, want 16\n", got)
		return 1
	}
	if !bytes.Equal(big[:16], abiFixturePNG[:]) {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=32) 前 16 字节不符: %x\n", big[:16])
		return 1
	}
	// 合法零:空图像 handle 返回 0,与 NullSize 可辨。
	if got := f.GetImage(0, unsafe.Pointer(&img[0]), 16); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(handle=0) = %#x, want 0(合法零)\n", got)
		return 1
	}
	// uint64 高位:buffSize=2^32+15 必须成功 —— 若被截断成 32 位则
	// 变为 15,会误判容量不足返回 NullSize。
	const highJustAbove = uint64(1)<<32 + 15
	if got := f.GetImage(hImg, unsafe.Pointer(&img[0]), highJustAbove); got != 16 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=2^32+15) = %#x, want 16(高 32 位不得截断)\n", got)
		return 1
	}
	// MaxUint64 容量同样按 uint64 处理(夹具只写固定 16 字节,安全)。
	if got := f.GetImage(hImg, unsafe.Pointer(&img[0]), math.MaxUint64); got != 16 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImage(size=MaxUint64) = %#x, want 16\n", got)
		return 1
	}

	// GetImageBgr:12 字节,同样协议。
	const hBgr = uintptr(0x1235)
	bgr := make([]byte, 12)
	if got := f.GetImageBgr(hBgr, unsafe.Pointer(&bgr[0]), 11); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImageBgr(size=11) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetImageBgr(hBgr, nil, 12); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImageBgr(nil) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetImageBgr(hBgr, unsafe.Pointer(&bgr[0]), 12); got != 12 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImageBgr(size=12) = %d, want 12\n", got)
		return 1
	}
	if !bytes.Equal(bgr, abiFixtureBGR[:]) {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImageBgr 内容不符: %x\n", bgr)
		return 1
	}
	if got := f.GetImageBgr(0, unsafe.Pointer(&bgr[0]), 12); got != 0 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetImageBgr(handle=0) = %#x, want 0(合法零)\n", got)
		return 1
	}

	// GetUUID:固定 16 字节,原生不写 NUL —— 预填充毒值,断言尾部
	// 字节未被触碰,证明必须按返回长度读取而非寻找 NUL。
	const hUUID = uintptr(0x5678)
	ub := make([]byte, 24)
	for i := range ub {
		ub[i] = 0xAA
	}
	if got := f.GetUUID(hUUID, &ub[0], 15); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID(size=15) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetUUID(hUUID, nil, 16); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID(nil) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetUUID(hUUID, &ub[0], 16); got != 16 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID(size=16) = %d, want 16\n", got)
		return 1
	}
	if string(ub[:16]) != abiFixtureUUIDText {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID 内容不符: %q\n", ub[:16])
		return 1
	}
	if ub[16] != 0xAA {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID 尾部字节被改写为 %#x(原生写了 NUL 或越界)\n", ub[16])
		return 1
	}
	// 更大容量同样返回精确长度,尾部毒值不动。
	ub2 := make([]byte, 24)
	for i := range ub2 {
		ub2[i] = 0xAA
	}
	if got := f.GetUUID(hUUID, &ub2[0], 24); got != 16 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID(size=24) = %d, want 16\n", got)
		return 1
	}
	if string(ub2[:16]) != abiFixtureUUIDText || ub2[16] != 0xAA {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetUUID(size=24) 内容或尾部毒值不符\n")
		return 1
	}

	// GetTasksList:容量与返回值单位均为 int32 元素数 —— 传 3(而非
	// 字节数 12)必须成功;传 2 不足;负元素保留。
	const hTasks = uintptr(0x9ABC)
	two := make([]int32, 2)
	if got := f.GetTasksList(hTasks, &two[0], 2); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetTasksList(容量 2) = %#x, want NullSize\n", got)
		return 1
	}
	if got := f.GetTasksList(hTasks, nil, 3); got != nullsize {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetTasksList(nil) = %#x, want NullSize\n", got)
		return 1
	}
	three := make([]int32, 3)
	if got := f.GetTasksList(hTasks, &three[0], 3); got != 3 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetTasksList(容量 3) = %d, want 3(单位是元素数)\n", got)
		return 1
	}
	if three[0] != 12 || three[1] != -5 || three[2] != 7 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetTasksList 内容 = %v, want [12 -5 7]\n", three)
		return 1
	}
	// 容量 12(字节误读值)同样成功且仍返回 3 个元素。
	twelve := make([]int32, 12)
	if got := f.GetTasksList(hTasks, &twelve[0], 12); got != 3 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetTasksList(容量 12) = %d, want 3\n", got)
		return 1
	}
	if twelve[0] != 12 || twelve[1] != -5 || twelve[2] != 7 {
		fmt.Fprintf(os.Stderr, "abi fixture buffers child: GetTasksList(容量 12) 内容 = %v, want [12 -5 7]\n", twelve)
		return 1
	}

	fmt.Printf("%s nullsize=%d image=16 bgr=12 uuid=16 empty=0 high16=16 tasks=12,-5,7\n",
		buffersOKMarker, uint64(nullsize))
	return 0
}

// == 父进程用例:每组独立子进程,复验标记行与关键字段 ==

// TestABIFixtureNumeric 验证数值往返组:子进程全部断言通过后打印
// 标记,父进程独立复验关键字段(纵深防御)。
func TestABIFixtureNumeric(t *testing.T) {
	soPath := buildABIFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     abiFixtureModeNumeric,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{abiFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("numeric 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, numericOKMarker)
	if !ok {
		t.Fatalf("numeric 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", numericOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, numericOKMarker)
	if err != nil {
		t.Fatalf("numeric 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	diag := out.Diagnostics()
	abiCheckField(t, fields, "nullsize", strconv.FormatUint(math.MaxUint64, 10), diag)
	abiCheckField(t, fields, "running0", "0", diag)
	abiCheckField(t, fields, "running1", "1", diag)
	abiCheckField(t, fields, "running255", "255", diag)
	abiCheckField(t, fields, "connected", "2", diag)
	abiCheckField(t, fields, "echo_min", strconv.FormatInt(math.MinInt32, 10), diag)
	abiCheckField(t, fields, "screencap1", "101", diag)
	abiCheckField(t, fields, "create", strconv.FormatUint(uint64(abiFixtureCreateHandle), 10), diag)

	t.Logf("numeric 子进程真实 ABI 验证通过(uint8 0/1/255、负 int32、MaxUint64 往返):%s", line)
}

// TestABIFixtureParams 验证参数传递组:x/y 槽位、5 参数折叠、
// token 往返、六参数形状。父进程用镜像函数独立现算期望值复验。
func TestABIFixtureParams(t *testing.T) {
	soPath := buildABIFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     abiFixtureModeParams,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{abiFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("params 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, paramsOKMarker)
	if !ok {
		t.Fatalf("params 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", paramsOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, paramsOKMarker)
	if err != nil {
		t.Fatalf("params 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}

	// 与子进程独立重算全部折叠期望值(纵深防御)。
	const (
		hConnect = uintptr(0x1122334455667788)
		cb       = uintptr(0x7777)
		token    = uintptr(0xDEADBEEF)
		hAttach  = uintptr(0xA)
		hwnd     = uintptr(0xB)
	)
	const hAttachBlock = uint8(1)
	sc, mouse, kb := uint64(0x1_0000_0002), uint64(0x2_0000_0010), uint64(0x1_0000_0004)
	mouseHigh := uint64(0x3_0000_0010)
	diag := out.Diagnostics()
	abiCheckField(t, fields, "click0", strconv.FormatInt(int64(-11), 10), diag)
	abiCheckField(t, fields, "click1", strconv.FormatInt(int64(-22), 10), diag)
	abiCheckField(t, fields, "connect1",
		strconv.FormatInt(int64(abiFoldAsyncConnect(hConnect, "/data/adb/abi", "127.0.0.1:5555", "Android", 1)), 10), diag)
	abiCheckField(t, fields, "connect2",
		strconv.FormatInt(int64(abiFoldAsyncConnect(hConnect, "/data/adb/abi", "Android", "127.0.0.1:5555", 1)), 10), diag)
	abiCheckField(t, fields, "createex",
		strconv.FormatUint(uint64(abiFixtureCreateHandle^cb^(token<<1)), 10), diag)
	abiCheckField(t, fields, "attach1",
		strconv.FormatInt(int64(abiFoldAsyncAttachWindow(hAttach, hwnd, sc, mouse, kb, hAttachBlock)), 10), diag)
	abiCheckField(t, fields, "attach2",
		strconv.FormatInt(int64(abiFoldAsyncAttachWindow(hAttach, hwnd, sc, mouseHigh, kb, hAttachBlock)), 10), diag)

	t.Logf("params 子进程真实 ABI 验证通过(4 参数槽位、5 参数折叠、token 往返、六参数形状 SysV 本地绑定):%s", line)
}

// TestABIFixtureVoidStrings 验证 void 与字符串组:副作用 dump、借
// 用字符串复制与改写、字符串输入到达。父进程复验标记关键字段。
func TestABIFixtureVoidStrings(t *testing.T) {
	soPath := buildABIFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     abiFixtureModeVoidStrings,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{abiFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("void-strings 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, voidStringsOKMarker)
	if !ok {
		t.Fatalf("void-strings 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", voidStringsOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, voidStringsOKMarker)
	if err != nil {
		t.Fatalf("void-strings 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	diag := out.Diagnostics()
	abiCheckField(t, fields, "copy", fmt.Sprintf(abiFixtureVersionFormat, 1), diag)
	abiCheckField(t, fields, "rewrite", fmt.Sprintf(abiFixtureVersionFormat, 2), diag)
	abiCheckField(t, fields, "logcount", "2", diag)
	abiCheckField(t, fields, "destroy", strconv.FormatInt(0xDEAD, 10), diag)

	t.Logf("void-strings 子进程真实 ABI 验证通过(void 副作用、借用改写、字符串输入):%s", line)
}

// TestABIFixtureBuffers 验证缓冲区协议组:NULL/不足/恰好/超容量、
// 合法零、哨兵、高位 buffSize、无 NUL、负元素。父进程复验标记关键
// 字段。
func TestABIFixtureBuffers(t *testing.T) {
	soPath := buildABIFixture(t)

	out := ffitest.Run(t, ffitest.Options{
		Mode:     abiFixtureModeBuffers,
		Timeout:  abiFixtureChildTimeout,
		ExtraEnv: map[string]string{abiFixtureSOEnv: soPath},
	})
	if out.Err != nil {
		t.Fatalf("buffers 子进程失败(非零退出/信号/超时一律视为失败):\n%s", out.Diagnostics())
	}
	line, ok := markerLine(out.Stdout, buffersOKMarker)
	if !ok {
		t.Fatalf("buffers 子进程退出码 0,但 stdout 缺少 %s 标记行:\n%s", buffersOKMarker, out.Diagnostics())
	}
	fields, err := parseABIMarkerFields(line, buffersOKMarker)
	if err != nil {
		t.Fatalf("buffers 标记行 %q 解析失败: %v\n%s", line, err, out.Diagnostics())
	}
	diag := out.Diagnostics()
	abiCheckField(t, fields, "nullsize", strconv.FormatUint(math.MaxUint64, 10), diag)
	abiCheckField(t, fields, "image", "16", diag)
	abiCheckField(t, fields, "bgr", "12", diag)
	abiCheckField(t, fields, "uuid", "16", diag)
	abiCheckField(t, fields, "empty", "0", diag)
	abiCheckField(t, fields, "high16", "16", diag)
	abiCheckField(t, fields, "tasks", "12,-5,7", diag)

	t.Logf("buffers 子进程真实 ABI 验证通过(缓冲区协议、合法零/哨兵可辨、uint64 高位、无 NUL UUID、元素数单位):%s", line)
}

// == 标记行解析辅助 ==

// parseABIMarkerFields 解析标记行 key=value 字段为映射;值可含逗号
// 与点号,不含空白。
func parseABIMarkerFields(line, marker string) (map[string]string, error) {
	rest := strings.TrimPrefix(line, marker+" ")
	fields := make(map[string]string)
	for _, kv := range strings.Fields(rest) {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("标记字段 %q 缺少 key=value 形态", kv)
		}
		fields[key] = value
	}
	return fields, nil
}

// abiCheckField 复验标记行单个字段的期望值;缺失或不符即 Fatalf,
// 附子进程完整诊断。
func abiCheckField(t *testing.T, fields map[string]string, key, want, diagnostics string) {
	t.Helper()
	got, ok := fields[key]
	if !ok {
		t.Fatalf("abi fixture 标记行缺少字段 %s:\n%s", key, diagnostics)
	}
	if got != want {
		t.Fatalf("abi fixture 标记行字段 %s = %q, want %q:\n%s", key, got, want, diagnostics)
	}
}
