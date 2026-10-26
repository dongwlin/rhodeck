/*
 * fixture.c — MAA-12 独立真实 ABI fixture(abifixture)。
 *
 * 定位与边界:
 *   - 本夹具按锁定的 C 头文件 AsstCaller.h(行号见各导出注释)独立
 *     手写 26 个 R/O 导出的真实签名;不由 Go 符号表生成,与 MAA-11
 *     的空导出 loader fixture(testdata/loaderfixture/fixture.c,签名
 *     有意不对应真实 ABI、绝不可被调用)分目录、分统计维护,两者
 *     不得混用;
 *   - Linux 上 ASSTAPI 与 ASST_CALL 均展开为空,导出直接写成普通
 *     C 函数;类型逐字取自头文件的 typedef;
 *   - 锁定头三处零参声明为空参 () 形式(AsstCaller.h:63、134、136,
 *     即 AsstCreate/AsstGetNullSize/AsstGetVersion);本夹具按 C 的
 *     规范零参原型形式写为 (void) —— 对函数定义两者等价,导出符号
 *     与调用约定不受影响;
 *   - 本夹具不调用、不依赖任何真实 MaaCore;全部行为来自本文件的
 *     已发表契约,语义确定、可审计;
 *   - 真实调用发生在 Go 侧独立子进程中(超时由父进程外部强制),
 *     结果以 MAA_M2_ABI_FIXTURE_*_OK 标记单独报告,与静态 checker
 *     和 MAA_M2_LOADER_FIXTURE_* 装载检查分开统计。
 *
 * == 已发表契约(全文) ==
 *
 * 常量:
 *   - NullSize 哨兵:0xFFFFFFFFFFFFFFFF,即 AsstGetNullSize 的返回;
 *     全部缓冲区函数的失败返回与其一致;
 *   - PNG 图像内容(16 字节):89 50 4E 47 0D 0A 1A 0A 00 00 00 0D
 *     49 48 44 52;
 *   - BGR 图像内容(12 字节):00 11 22 33 44 55 66 77 88 99 AA BB;
 *   - UUID 内容:"uuid-no-nul-1234"(16 字节);
 *   - 版本格式:"v6.20.1-abifixture#%d",计数从 1 开始,每次调用 +1;
 *   - 状态 dump 格式:"log=%u,extras=%s=%s,destroy=%llu"。
 *
 * uint8 往返:
 *   - AsstRunning:返回 (uint8_t)(uintptr_t)handle —— handle 取
 *     0/1/0xFF 得 0/1/255;
 *   - AsstConnected:恒返回 2 —— 非 0/1 的真值,验证「非零为真」的
 *     布尔解释不能依赖具体数值。
 *
 * int32 往返(小端):
 *   - AsstAppendTask:taskType == "echo-i32" 且 params 非 NULL 时,
 *     把 params 前 4 字节按小端装配成 int32_t 返回(符号位保留);
 *     其余情形返回 0。
 *
 * 参数位置与 4+ 参数:
 *   - AsstAsyncClick:block==0 返回 x,block==1 返回 y,其余 block
 *     返回 0 —— 用互相不同的负值证明第 2/3 槽位次序与符号位;
 *   - AsstAsyncConnect(5 参数;契约要求 3 个字符串参数均非 NULL):
 *     对 handle 低 32 位、三个 strlen 与 block 做确定性折叠(uint32
 *     异或/乘常量/rotate,各位置运算结构不同)后按 int32 返回;
 *   - AsstAsyncAttachWindow(6 参数形状;windowsOnly,非 Windows 平
 *     台不参与符号查找,夹具仍导出):对全部 6 个参数做旋转位数各
 *     不相同的 rotate-xor(9 个分量:handle、hwnd、3 个 method 的
 *     低/高 32 位、block),高 32 位独立参与,按 int32 返回。Go 侧
 *     以本地绑定调用它只证明 SysV ABI 下六参数形状传递正确,不代表
 *     Windows 调用约定运行通过。
 *
 * void 与字符串:
 *   - AsstDestroy / AsstSetConnectionExtras / AsstLog:无返回值,不
 *     提供成功码。Destroy 记录 g_last_destroyed(最后一次 handle);
 *     SetConnectionExtras 把 name/extras 有界复制进静态缓冲(上限
 *     64/256 字节,超长截断);Log 把 level/message 有界复制进静态
 *     缓冲(上限 32/256 字节)并使 g_log_count +1;
 *   - 状态读取通道:GetUUID(handle==0)返回状态 dump(见上格式,
 *     内容不含 NUL),buff==NULL 或容量不足返回 NullSize,足够则
 *     返回精确字节长度;
 *   - AsstGetVersion:每次调用把静态缓冲改写为版本格式(计数递增)
 *     并返回其借用指针 —— 调用方首次复制后,后续调用改写原生缓冲
 *     不影响已复制的 Go 内容;
 *   - 字符串内容相等比较(SetUserDir/LoadResource/SetStaticOption/
 *     SetInstanceOption/SetTaskParams):仅当字符串内容与契约值完全
 *     相等(且 key/ID 等整数参数同时取契约值,槽位错位即失败)时
 *     返回 1,否则返回 0 —— 证明 C 字符串输入内容真实到达:
 *       SetUserDir        path == "/abi-fixture-userdir"
 *       LoadResource      path == "/abi-fixture-resource"
 *       SetStaticOption   key == 1 且 value == "static-option-value"
 *       SetInstanceOption key == 6 且 value == "clienttype-value"
 *       SetTaskParams     id == 42 且 params == "{}"
 *
 * 缓冲区协议(GetImage/GetImageBgr/GetUUID/GetTasksList):
 *   - buff==NULL 或容量不足(含 0)→ NullSize;足够 → 拷贝全部内容
 *     并返回内容字节数(图像/UUID)或元素数(任务列表);
 *   - GetImage/GetImageBgr 的 handle==0 返回 0(合法空图像,与
 *     NullSize 可辨;此时不触碰缓冲区);
 *   - GetUUID(handle!=0)拷贝固定 UUID 且不写 NUL —— 调用方必须按
 *     返回长度读取,不能依赖尾部 NUL;
 *   - GetTasksList 固定内容 {12, -5, 7}(3 个 int32,含负数),容量
 *     与返回值的单位都是元素数,不做 ×4;
 *   - buffSize 按 uint64 处理:高 32 位非零的取值(如
 *     0x1_0000_0000+15)不得被截断成 32 位 —— 截断后该值变为 15,
 *     对 16 字节图像会误判容量不足,Go 侧以此区分截断与否。
 *
 * 未深测符号(MAA-12 未深测,仅保证签名在场与确定返回):
 *   - AsstCreate:恒返回 (AsstHandle)0x00C0FFEE(固定非零值,调用方
 *     不解引用);
 *   - AsstCreateEx:返回 0x00C0FFEE ^ (uintptr_t)callback ^
 *     ((uintptr_t)custom_arg << 1) —— callback 与 token 槽位错位即
 *     结果不同(token 往返);夹具从不调用 callback;
 *   - AsstStart / AsstStop / AsstBackToHome:恒返回 11 / 12 / 13;
 *   - AsstAsyncScreencap:返回 100 + block;
 *   - 字符串比较函数(SetUserDir/LoadResource/SetStaticOption/
 *     SetInstanceOption/SetTaskParams)同属未深测清单,其返回值另
 *     兼作字符串输入到达验证,契约如上。
 */

#include <stdint.h>
#include <stdio.h>
#include <string.h>

/* 类型逐字对应 AsstCaller.h:7、41-53;Linux 上 ASSTAPI/ASST_CALL
 * 展开为空,callback 直接是普通函数指针类型。 */
typedef uint8_t AsstBool;                    /* AsstCaller.h:41 */
typedef uint64_t AsstSize;                   /* AsstCaller.h:42 */
typedef int32_t AsstId;                      /* AsstCaller.h:44 */
typedef AsstId AsstMsgId;                    /* AsstCaller.h:45 */
typedef AsstId AsstTaskId;                   /* AsstCaller.h:46 */
typedef AsstId AsstAsyncCallId;              /* AsstCaller.h:47 */
typedef int32_t AsstOptionKey;               /* AsstCaller.h:49 */
typedef AsstOptionKey AsstStaticOptionKey;   /* AsstCaller.h:50 */
typedef AsstOptionKey AsstInstanceOptionKey; /* AsstCaller.h:51 */

struct AsstExtAPI;
typedef struct AsstExtAPI* AsstHandle;       /* AsstCaller.h:7 */

typedef void (*AsstApiCallback)(AsstMsgId msg, const char* details_json,
                                void* custom_arg); /* AsstCaller.h:53 */

/* == 契约常量 == */

#define ABIFX_NULL_SIZE 0xFFFFFFFFFFFFFFFFULL
#define ABIFX_PNG_SIZE 16
#define ABIFX_BGR_SIZE 12
#define ABIFX_UUID_SIZE 16

static const unsigned char ABIFX_PNG[ABIFX_PNG_SIZE] = {
    0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
    0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52};
static const unsigned char ABIFX_BGR[ABIFX_BGR_SIZE] = {
    0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
    0x88, 0x99, 0xAA, 0xBB};
static const char* ABIFX_UUID_TEXT = "uuid-no-nul-1234"; /* 恰 16 字节 */

#define ABIFX_CREATE_HANDLE 0x00C0FFEE
#define ABIFX_ECHO_I32_TYPE "echo-i32"

/* == 有界静态状态 == */

static unsigned g_log_count = 0;
static char g_log_level[32];    /* 上限 31 字节 + NUL */
static char g_log_message[256]; /* 上限 255 字节 + NUL */
static char g_extras_name[64];  /* 上限 63 字节 + NUL */
static char g_extras_json[256]; /* 上限 255 字节 + NUL */
static uintptr_t g_last_destroyed = 0;
static int g_version_counter = 0;

static AsstSize abifx_null_size(void) { return ABIFX_NULL_SIZE; }

static uint32_t abifx_rotl32(uint32_t v, unsigned n) {
    return (v << n) | (v >> (32u - n));
}

/* 状态 dump:格式见文件头契约;内容不含 NUL,返回精确字节长度。 */
static AsstSize abifx_status_dump(char* buff, AsstSize buff_size) {
    char dump[512];
    int n = snprintf(dump, sizeof dump, "log=%u,extras=%s=%s,destroy=%llu",
                     g_log_count, g_extras_name, g_extras_json,
                     (unsigned long long)g_last_destroyed);
    if (n < 0 || (size_t)n >= sizeof dump) {
        return abifx_null_size();
    }
    if (buff == NULL || buff_size < (AsstSize)n) {
        return abifx_null_size();
    }
    memcpy(buff, dump, (size_t)n);
    return (AsstSize)n;
}

/* == 导出(26 个,顺序同 native 符号清单) == */

/*  1/26: AsstSetUserDir — AsstCaller.h:59;未深测,字符串内容相等比较 */
AsstBool AsstSetUserDir(const char* path) {
    return (AsstBool)(path != NULL && strcmp(path, "/abi-fixture-userdir") == 0);
}

/*  2/26: AsstLoadResource — AsstCaller.h:60;未深测,字符串内容相等比较 */
AsstBool AsstLoadResource(const char* path) {
    return (AsstBool)(path != NULL && strcmp(path, "/abi-fixture-resource") == 0);
}

/*  3/26: AsstSetStaticOption — AsstCaller.h:61;未深测,key+字符串内容 */
AsstBool AsstSetStaticOption(AsstStaticOptionKey key, const char* value) {
    return (AsstBool)(key == 1 && value != NULL &&
                      strcmp(value, "static-option-value") == 0);
}

/*  4/26: AsstCreate — AsstCaller.h:63;未深测,固定非零句柄值 */
AsstHandle AsstCreate(void) {
    return (AsstHandle)(uintptr_t)ABIFX_CREATE_HANDLE;
}

/*  5/26: AsstCreateEx — AsstCaller.h:64;未深测,callback/token 折叠
 * (token 往返);从不调用 callback */
AsstHandle AsstCreateEx(AsstApiCallback callback, void* custom_arg) {
    return (AsstHandle)(ABIFX_CREATE_HANDLE ^ (uintptr_t)callback ^
                        ((uintptr_t)custom_arg << 1));
}

/*  6/26: AsstDestroy — AsstCaller.h:65;void,记录最后一次 handle */
void AsstDestroy(AsstHandle handle) {
    g_last_destroyed = (uintptr_t)handle;
}

/*  7/26: AsstSetInstanceOption — AsstCaller.h:70-71;未深测,key+字符串 */
AsstBool AsstSetInstanceOption(AsstHandle handle, AsstInstanceOptionKey key,
                               const char* value) {
    (void)handle;
    return (AsstBool)(key == 6 && value != NULL &&
                      strcmp(value, "clienttype-value") == 0);
}

/*  8/26: AsstAppendTask — AsstCaller.h:81;echo-i32 小端 int32 往返 */
AsstTaskId AsstAppendTask(AsstHandle handle, const char* type, const char* params) {
    (void)handle;
    if (type != NULL && strcmp(type, ABIFX_ECHO_I32_TYPE) == 0 &&
        params != NULL) {
        /* 显式小端装配,不依赖机器字节序 */
        uint32_t u = (uint32_t)(uint8_t)params[0] |
                     ((uint32_t)(uint8_t)params[1] << 8) |
                     ((uint32_t)(uint8_t)params[2] << 16) |
                     ((uint32_t)(uint8_t)params[3] << 24);
        return (AsstTaskId)u;
    }
    return 0;
}

/*  9/26: AsstSetTaskParams — AsstCaller.h:82;未深测,id+字符串内容 */
AsstBool AsstSetTaskParams(AsstHandle handle, AsstTaskId id, const char* params) {
    (void)handle;
    return (AsstBool)(id == 42 && params != NULL && strcmp(params, "{}") == 0);
}

/* 10/26: AsstStart — AsstCaller.h:84;未深测,常量 11 */
AsstBool AsstStart(AsstHandle handle) {
    (void)handle;
    return (AsstBool)11;
}

/* 11/26: AsstStop — AsstCaller.h:85;未深测,常量 12 */
AsstBool AsstStop(AsstHandle handle) {
    (void)handle;
    return (AsstBool)12;
}

/* 12/26: AsstRunning — AsstCaller.h:86;uint8 往返:(uint8_t)handle */
AsstBool AsstRunning(AsstHandle handle) {
    return (AsstBool)(uintptr_t)handle;
}

/* 13/26: AsstConnected — AsstCaller.h:87;恒返回 2(非零为真) */
AsstBool AsstConnected(AsstHandle handle) {
    (void)handle;
    return (AsstBool)2;
}

/* 14/26: AsstBackToHome — AsstCaller.h:88;未深测,常量 13 */
AsstBool AsstBackToHome(AsstHandle handle) {
    (void)handle;
    return (AsstBool)13;
}

/* 15/26: AsstAsyncConnect — AsstCaller.h:91-96;5 参数确定性折叠
 * (handle 低 32 位、3 个 strlen、block;契约要求字符串均非 NULL;
 * address 步经 rotate,避免与 config 步可交换,槽位对调必变) */
AsstAsyncCallId AsstAsyncConnect(AsstHandle handle, const char* adb_path,
                                 const char* address, const char* config,
                                 AsstBool block) {
    uint32_t f = (uint32_t)(uintptr_t)handle;
    f ^= 0x9E3779B9u;
    f = (f ^ (uint32_t)strlen(adb_path)) * 16777619u;
    f = abifx_rotl32(f ^ (uint32_t)strlen(address), 7);
    f = (f ^ (uint32_t)strlen(config)) * 2654435761u;
    f ^= (uint32_t)block;
    return (AsstAsyncCallId)f;
}

/* 16/26: AsstSetConnectionExtras — AsstCaller.h:97;void,有界复制 */
void AsstSetConnectionExtras(const char* name, const char* extras) {
    snprintf(g_extras_name, sizeof g_extras_name, "%s", name != NULL ? name : "");
    snprintf(g_extras_json, sizeof g_extras_json, "%s", extras != NULL ? extras : "");
}

/* 17/26: AsstAsyncAttachWindow — AsstCaller.h:118-124;windowsOnly,
 * 非 Windows 不参与查找,夹具仍导出;6 参数形状的 rotate-xor 折叠,
 * 旋转位数按槽位区分,method 高 32 位独立参与。注意:Go 侧本地绑定
 * 调用只证明 SysV ABI 六参数形状传递,不代表 Windows 调用约定。 */
AsstAsyncCallId AsstAsyncAttachWindow(AsstHandle handle, void* hwnd,
                                      uint64_t screencap_method,
                                      uint64_t mouse_method,
                                      uint64_t keyboard_method,
                                      AsstBool block) {
    uint32_t acc = 0x12345678u;
    acc ^= abifx_rotl32((uint32_t)(uintptr_t)handle, 1);
    acc ^= abifx_rotl32((uint32_t)(uintptr_t)hwnd, 3);
    acc ^= abifx_rotl32((uint32_t)screencap_method, 5);
    acc ^= abifx_rotl32((uint32_t)(screencap_method >> 32), 7);
    acc ^= abifx_rotl32((uint32_t)mouse_method, 9);
    acc ^= abifx_rotl32((uint32_t)(mouse_method >> 32), 11);
    acc ^= abifx_rotl32((uint32_t)keyboard_method, 13);
    acc ^= abifx_rotl32((uint32_t)(keyboard_method >> 32), 15);
    acc ^= abifx_rotl32((uint32_t)block, 17);
    return (AsstAsyncCallId)acc;
}

/* 18/26: AsstAsyncClick — AsstCaller.h:127;槽位次序:block 选 x/y */
AsstAsyncCallId AsstAsyncClick(AsstHandle handle, int32_t x, int32_t y,
                               AsstBool block) {
    (void)handle;
    if (block == 0) {
        return x;
    }
    if (block == 1) {
        return y;
    }
    return 0;
}

/* 19/26: AsstAsyncScreencap — AsstCaller.h:128;未深测,100+block */
AsstAsyncCallId AsstAsyncScreencap(AsstHandle handle, AsstBool block) {
    (void)handle;
    return (AsstAsyncCallId)(100 + (int32_t)block);
}

/* 20/26: AsstGetImage — AsstCaller.h:130;16 字节 PNG,单位字节 */
AsstSize AsstGetImage(AsstHandle handle, void* buff, AsstSize buff_size) {
    if ((uintptr_t)handle == 0) {
        return 0; /* 合法空图像,与 NullSize 可辨 */
    }
    if (buff == NULL || buff_size < ABIFX_PNG_SIZE) {
        return abifx_null_size();
    }
    memcpy(buff, ABIFX_PNG, ABIFX_PNG_SIZE);
    return ABIFX_PNG_SIZE;
}

/* 21/26: AsstGetImageBgr — AsstCaller.h:131;12 字节 BGR,单位字节 */
AsstSize AsstGetImageBgr(AsstHandle handle, void* buff, AsstSize buff_size) {
    if ((uintptr_t)handle == 0) {
        return 0; /* 合法空图像,与 NullSize 可辨 */
    }
    if (buff == NULL || buff_size < ABIFX_BGR_SIZE) {
        return abifx_null_size();
    }
    memcpy(buff, ABIFX_BGR, ABIFX_BGR_SIZE);
    return ABIFX_BGR_SIZE;
}

/* 22/26: AsstGetUUID — AsstCaller.h:132;handle==0 为状态 dump 通道,
 * handle!=0 拷贝固定 UUID 且不写 NUL,单位字节 */
AsstSize AsstGetUUID(AsstHandle handle, char* buff, AsstSize buff_size) {
    if ((uintptr_t)handle == 0) {
        return abifx_status_dump(buff, buff_size);
    }
    if (buff == NULL || buff_size < ABIFX_UUID_SIZE) {
        return abifx_null_size();
    }
    memcpy(buff, ABIFX_UUID_TEXT, ABIFX_UUID_SIZE); /* 不写 NUL */
    return ABIFX_UUID_SIZE;
}

/* 23/26: AsstGetTasksList — AsstCaller.h:133;固定 {12,-5,7},容量与
 * 返回值单位均为元素数(不做 ×4) */
AsstSize AsstGetTasksList(AsstHandle handle, AsstTaskId* buff, AsstSize buff_size) {
    static const AsstTaskId kTasks[3] = {12, -5, 7};
    (void)handle;
    if (buff == NULL || buff_size < 3) {
        return abifx_null_size();
    }
    memcpy(buff, kTasks, sizeof kTasks);
    return 3;
}

/* 24/26: AsstGetNullSize — AsstCaller.h:134;失败哨兵 MaxUint64 */
AsstSize AsstGetNullSize(void) {
    return abifx_null_size();
}

/* 25/26: AsstGetVersion — AsstCaller.h:136;静态缓冲每次改写并递增
 * 计数,返回借用指针(调用方立即复制,不 free) */
const char* AsstGetVersion(void) {
    static char buf[64];
    ++g_version_counter;
    snprintf(buf, sizeof buf, "v6.20.1-abifixture#%d", g_version_counter);
    return buf;
}

/* 26/26: AsstLog — AsstCaller.h:137;void,有界复制并计数(非 printf
 * 变参,两参数均为 C 字符串) */
void AsstLog(const char* level, const char* message) {
    ++g_log_count;
    snprintf(g_log_level, sizeof g_log_level, "%s", level != NULL ? level : "");
    snprintf(g_log_message, sizeof g_log_message, "%s", message != NULL ? message : "");
}
