/*
 * fixture.c — core-worker 异步完成、故障矩阵与生命周期验证用受控原生库
 * (asyncfixture)。
 *
 * 定位与边界:
 *   - 本夹具按锁定的 C 头文件 AsstCaller.h 与 AsstCallerExtra.h 独立
 *     手写全部必需导出与可选导出(AsstGetImageBgr、AsstGetMapLevelKey、
 *     AsstGetItemName、AsstAsyncAttachWindow)的真实签名,供真实
 *     core-worker 二进制经 maa-go 绑定动态加载,验收四类异步提交的
 *     真实回调完成、实例故障观察与跨方法组合;
 *   - 与既有夹具分工不同:imagefixture 验证跨进程大图像传输(且不导出
 *     实验符号),queryfixture 验证日志与实验查询(从不回调),本夹具
 *     专供异步完成时序、故障注入与生命周期组合,三者不混用、互不替代;
 *   - 不调用、不依赖任何真实 MaaCore;从不连接设备、从不截图真实
 *     屏幕、从不启动真实任务;全部完成消息由本夹具自己经 AsstApiCallback
 *     发出,内容确定;
 *   - 真实调用发生在 core-worker 子进程内(由监管客户端测试驱动);
 *     除测试触发的回调外,本夹具从不输出 stdout。
 *
 * == 已发表契约 ==
 *
 * 常量:
 *   - NullSize 哨兵:0xFFFFFFFFFFFFFFFF(锁定 profile 值,Open 必核验);
 *   - 版本:"v6.20.1-asyncfx";实例句柄:0x00FEED42;
 *   - AsyncCallInfo 消息编号 4(冻结自 AsstMsg.h);洪水消息编号 777
 *     (未知编号,绑定按未知消息原样交付);
 *   - 完成消息 details 固定携带大整数 "big":9007199254740993(2^53+1,
 *     float64 往返必失真,用于逐字节保真断言)与 "src":"asyncfx"。
 *
 * 异步提交(四类共享一个严格递增计数器,binding 要求 raw 返回值严格
 * 大于上一次已接受 ID 且在 1..MaxInt32):
 *   - AsstAsyncClick(handle,x,y,block):x == 0x4658("FX")为触发提交:
 *     先把自己压入待完成栈,随后按 LIFO 逐条发出全部待完成项(完成
 *     顺序与受理顺序相反,乱序可断言;自身完成在自身调用内同步发出,
 *     即「早到结果」),最后返回其 ID;x == 0x4658 且 y == -1 时其
 *     details.ret 为 false(原生失败语义);其余点击只压栈不完成;
 *   - AsstAsyncConnect:address 以 "asyncfx-fail" 开头时 ret=false;
 *     只压栈,由后续触发点击统一发出;
 *   - AsstAsyncScreencap / AsstAsyncAttachWindow:ret 恒 true,只压栈;
 *   - 完成 JSON(消息 4):{"what":"<Connect|Click|Screencap|
 *     AttachWindow>","async_call_id":<id>,"details":{"ret":<bool>,
 *     "cost":1,"big":9007199254740993,"src":"asyncfx"}};what 与提交
 *     方法一一对应,客户端按预期 what 校验。
 *
 * 故障与洪水触发(经 AsstLog 的 message 携带,level 任意):
 *   - "asyncfx-fire-unknown":发出一条从未提交的 async_call_id
 *     (0x7FFFFF00,高于全部已接受 ID,且发出时无 raw 窗口)——镜像
 *     「无法关联的完成」,绑定按协议故障记录首因;
 *   - "asyncfx-duplicate":重发最近一次已完成的 async_call_id——镜像
 *     「已完成 ID 的重复回调」,绑定按协议故障记录首因;
 *   - "asyncfx-badjson":发出一条非法 UTF-8/非法 JSON 的回调原文
 *     ({0xFF,0xFE,...})——镜像原生回调契约破坏,绑定保留原文前缀
 *     并记录协议故障,该原文仍按公开事件预算交付(由客户端边界拒绝);
 *   - "asyncfx-flood-<N>":连续发出 N 条消息编号 777 的洪水事件
 *     {"asyncfx":"flood","n":<k>,"big":9007199254740993}(k 从 1 起);
 *     用于验证两端有界队列与溢出纪律;
 *   - 其余 message:忽略(仅 void 返回)。
 *
 * 观察与图像:
 *   - AsstGetUUID 恒返回 "asyncfx-uuid"(标识查询;日志读回通道由
 *     queryfixture 承载,本夹具不复用);
 *   - AsstGetImage:NULL 返回 NullSize,足够返回 0(合法空图像);
 *   - AsstGetImageBgr(可选符号):16 字节确定图像 byte[i] = i*29+0xB5;
 *     NULL 或容量不足返回 NullSize;
 *   - AsstGetMapLevelKey:"asyncfx-hit" 返回带调用计数的四元组
 *     (thread_local 借用缓冲),其余(含 NULL)返回全 NULL struct;
 *   - AsstGetItemName:NULL/空串返回 "Unknown","asyncfx-item" 返回
 *     "asyncfx-item#N",其余返回空串;
 *   - AsstGetTasksList:NULL 或容量不足返回 NullSize,足够返回 0
 *     (空任务列表);
 *   - Running/Connected 恒返回 0;Stop/Start/BackToHome 恒返回 1;
 *     AppendTask 恒返回 7;SetTaskParams/LoadResource/SetUserDir/
 *     SetStaticOption/SetInstanceOption 非空参数恒返回 1;
 *   - CreateEx 保存 callback/custom_arg 并返回固定句柄;Destroy void;
 *     SetConnectionExtras/Log void。
 */

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* 类型逐字对应 AsstCaller.h;Linux 上 ASSTAPI/ASST_CALL 展开为空。 */
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
typedef struct AsstExtAPI* AsstHandle; /* AsstCaller.h:7 */

typedef void (*AsstApiCallback)(AsstMsgId msg, const char* details_json,
                                void* custom_arg); /* AsstCaller.h:53 */

/* == 契约常量 == */

#define ASYFX_NULL_SIZE 0xFFFFFFFFFFFFFFFFULL
#define ASYFX_CREATE_HANDLE 0x00FEED42
#define ASYFX_MSG_ASYNC_CALL_INFO 4
#define ASYFX_MSG_FLOOD 777
#define ASYFX_TRIGGER_X 0x4658      /* "FX":触发点击 */
#define ASYFX_FAIL_Y (-1)           /* ret=false 的点击 */
#define ASYFX_FIRST_ID 0x1F001      /* 首个异步 ID(>0,异于既有夹具) */
#define ASYFX_UNKNOWN_ID 0x7FFFFF00 /* 从未提交的完成 ID */
#define ASYFX_BIG 9007199254740993ULL /* 2^53+1 */
static const char ASYFX_VERSION[] = "v6.20.1-asyncfx";
static const char ASYFX_UUID[] = "asyncfx-uuid";

/* == 回调与异步完成状态 == */

static AsstApiCallback g_callback = NULL;
static void* g_callback_arg = NULL;
static AsstAsyncCallId g_next_id = (AsstAsyncCallId)ASYFX_FIRST_ID;

/* 待完成栈:受理即压栈,触发时 LIFO 发出。容量按测试规模固定。 */
#define ASYFX_MAX_PENDING 64
struct AsyfxPending {
    AsstAsyncCallId id;
    const char* what;
    int ok;
};
static struct AsyfxPending g_pending[ASYFX_MAX_PENDING];
static int g_pending_count = 0;
static AsstAsyncCallId g_last_completed = 0;

/* == 完成发出(命令循环线程内同步调用;回调线程安全由绑定承担) == */

static void asyfx_fire(AsstMsgId msg, const char* json) {
    if (g_callback != NULL) {
        g_callback(msg, json, g_callback_arg);
    }
}

static void asyfx_fire_completion(AsstAsyncCallId id, const char* what, int ok) {
    char json[256];
    int n = snprintf(json, sizeof json,
                     "{\"what\":\"%s\",\"async_call_id\":%d,"
                     "\"details\":{\"ret\":%s,\"cost\":1,\"big\":%llu,"
                     "\"src\":\"asyncfx\"}}",
                     what, (int)id, ok ? "true" : "false",
                     (unsigned long long)ASYFX_BIG);
    if (n < 0 || (size_t)n >= sizeof json) {
        return; /* 契约内不可能:保持静默确定行为 */
    }
    g_last_completed = id;
    asyfx_fire(ASYFX_MSG_ASYNC_CALL_INFO, json);
}

/* LIFO 排空待完成栈:完成顺序与受理顺序相反。 */
static void asyfx_drain(void) {
    while (g_pending_count > 0) {
        struct AsyfxPending entry = g_pending[--g_pending_count];
        asyfx_fire_completion(entry.id, entry.what, entry.ok);
    }
}

static AsstAsyncCallId asyfx_push(const char* what, int ok) {
    AsstAsyncCallId id = g_next_id++;
    if (g_pending_count < ASYFX_MAX_PENDING) {
        g_pending[g_pending_count].id = id;
        g_pending[g_pending_count].what = what;
        g_pending[g_pending_count].ok = ok;
        ++g_pending_count;
    }
    return id;
}

/* == 故障/洪水触发解析 == */

static int asyfx_starts_with(const char* s, const char* prefix) {
    size_t n = strlen(prefix);
    return s != NULL && strncmp(s, prefix, n) == 0;
}

/* == 坏回调原文常量:非法 UTF-8(0xFF 0xFE)且非法 JSON。 == */
static const char asyfx_bad_json[] = {'\xFF', '\xFE', '{', '"', 'a', 's', 'y',
                                      'n',     'c',    'f', 'x', '"', '\0'};

/* == 实验查询的 thread_local 存储(借用语义镜像 queryfixture)。 == */

static __thread char asyfx_stage[32];
static __thread char asyfx_code[32];
static __thread char asyfx_level[32];
static __thread char asyfx_name[32];
static __thread char asyfx_item[32];
static int g_level_counter = 0;
static int g_item_counter = 0;

/* == BGR 图像 == */

#define ASYFX_BGR_BYTES 16

static void asyfx_fill_bgr(unsigned char* buff) {
    for (int i = 0; i < ASYFX_BGR_BYTES; ++i) {
        buff[i] = (unsigned char)(i * 29 + 0xB5);
    }
}

/* == 导出(必需 25 项 + 可选项) == */

AsstBool AsstSetUserDir(const char* path) {
    return (AsstBool)(path != NULL);
}

AsstBool AsstLoadResource(const char* path) {
    return (AsstBool)(path != NULL);
}

AsstBool AsstSetStaticOption(AsstStaticOptionKey key, const char* value) {
    return (AsstBool)(key >= 0 && value != NULL);
}

AsstHandle AsstCreateEx(AsstApiCallback callback, void* custom_arg) {
    g_callback = callback;
    g_callback_arg = custom_arg;
    return (AsstHandle)(uintptr_t)ASYFX_CREATE_HANDLE;
}

void AsstDestroy(AsstHandle handle) {
    (void)handle;
}

AsstBool AsstSetInstanceOption(AsstHandle handle, AsstInstanceOptionKey key,
                               const char* value) {
    (void)handle;
    return (AsstBool)(key >= 0 && value != NULL);
}

AsstTaskId AsstAppendTask(AsstHandle handle, const char* type, const char* params) {
    (void)handle;
    (void)type;
    (void)params;
    return 7;
}

AsstBool AsstSetTaskParams(AsstHandle handle, AsstTaskId id, const char* params) {
    (void)handle;
    (void)id;
    return (AsstBool)(params != NULL);
}

AsstBool AsstStart(AsstHandle handle) {
    (void)handle;
    return (AsstBool)1;
}

AsstBool AsstStop(AsstHandle handle) {
    (void)handle;
    return (AsstBool)1;
}

AsstBool AsstRunning(AsstHandle handle) {
    (void)handle;
    return (AsstBool)0;
}

AsstBool AsstConnected(AsstHandle handle) {
    (void)handle;
    return (AsstBool)0;
}

AsstBool AsstBackToHome(AsstHandle handle) {
    (void)handle;
    return (AsstBool)1;
}

AsstAsyncCallId AsstAsyncConnect(AsstHandle handle, const char* adb_path,
                                 const char* address, const char* config,
                                 AsstBool block) {
    (void)handle;
    (void)adb_path;
    (void)config;
    (void)block;
    int ok = !(address != NULL && asyfx_starts_with(address, "asyncfx-fail"));
    return asyfx_push("Connect", ok);
}

void AsstSetConnectionExtras(const char* name, const char* extras) {
    (void)name;
    (void)extras;
}

AsstAsyncCallId AsstAsyncClick(AsstHandle handle, int32_t x, int32_t y,
                               AsstBool block) {
    (void)handle;
    (void)block;
    int trigger = (x == (int32_t)ASYFX_TRIGGER_X);
    AsstAsyncCallId id = asyfx_push("Click", y != (int32_t)ASYFX_FAIL_Y);
    if (trigger) {
        asyfx_drain(); /* 自身在栈内:LIFO 先完成自己(早到结果) */
    }
    return id;
}

AsstAsyncCallId AsstAsyncScreencap(AsstHandle handle, AsstBool block) {
    (void)handle;
    (void)block;
    return asyfx_push("Screencap", 1);
}

AsstAsyncCallId AsstAsyncAttachWindow(AsstHandle handle, void* hwnd,
                                      uint64_t screencap_method,
                                      uint64_t mouse_method,
                                      uint64_t keyboard_method,
                                      AsstBool block) {
    (void)handle;
    (void)hwnd;
    (void)screencap_method;
    (void)mouse_method;
    (void)keyboard_method;
    (void)block;
    return asyfx_push("AttachWindow", 1);
}

/* AsstGetImage — 恒定空图像:NULL 返回 NullSize,足够返回 0。 */
AsstSize AsstGetImage(AsstHandle handle, void* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL) {
        return ASYFX_NULL_SIZE;
    }
    return 0;
}

/* AsstGetImageBgr — 可选符号:16 字节确定图像。 */
AsstSize AsstGetImageBgr(AsstHandle handle, void* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL || buff_size < (AsstSize)ASYFX_BGR_BYTES) {
        return ASYFX_NULL_SIZE;
    }
    asyfx_fill_bgr((unsigned char*)buff);
    return (AsstSize)ASYFX_BGR_BYTES;
}

AsstSize AsstGetUUID(AsstHandle handle, char* buff, AsstSize buff_size) {
    (void)handle;
    size_t n = sizeof ASYFX_UUID - 1;
    if (buff == NULL || buff_size < (AsstSize)n) {
        return ASYFX_NULL_SIZE;
    }
    memcpy(buff, ASYFX_UUID, n); /* 不写 NUL:按返回长度读取 */
    return (AsstSize)n;
}

AsstSize AsstGetTasksList(AsstHandle handle, AsstTaskId* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL || buff_size < 1) {
        return ASYFX_NULL_SIZE;
    }
    return 0; /* 空任务列表 */
}

AsstSize AsstGetNullSize(void) {
    return ASYFX_NULL_SIZE;
}

const char* AsstGetVersion(void) {
    return ASYFX_VERSION;
}

/* AsstLog — 测试触发通道;契约见文件头。 */
void AsstLog(const char* level, const char* message) {
    (void)level;
    if (message == NULL) {
        return;
    }
    if (strcmp(message, "asyncfx-fire-unknown") == 0) {
        asyfx_fire_completion((AsstAsyncCallId)ASYFX_UNKNOWN_ID, "Click", 1);
        return;
    }
    if (strcmp(message, "asyncfx-duplicate") == 0) {
        if (g_last_completed != 0) {
            asyfx_fire_completion(g_last_completed, "Click", 1);
        }
        return;
    }
    if (strcmp(message, "asyncfx-badjson") == 0) {
        asyfx_fire(ASYFX_MSG_ASYNC_CALL_INFO, asyfx_bad_json);
        return;
    }
    if (asyfx_starts_with(message, "asyncfx-flood-")) {
        long n = strtol(message + sizeof("asyncfx-flood-") - 1, NULL, 10);
        for (long k = 1; k <= n; ++k) {
            char json[128];
            int w = snprintf(json, sizeof json,
                             "{\"asyncfx\":\"flood\",\"n\":%ld,\"big\":%llu}",
                             k, (unsigned long long)ASYFX_BIG);
            if (w < 0 || (size_t)w >= sizeof json) {
                return;
            }
            asyfx_fire((AsstMsgId)ASYFX_MSG_FLOOD, json);
        }
        return;
    }
}

/* == AsstCallerExtra.h 实验导出 == */

struct AsstMapLevelKey {
    const char* stage_id; /* AsstCallerExtra.h:15 */
    const char* code;     /* AsstCallerExtra.h:16 */
    const char* level_id; /* AsstCallerExtra.h:17 */
    const char* name;     /* AsstCallerExtra.h:18 */
};

struct AsstMapLevelKey AsstGetMapLevelKey(const char* key) {
    struct AsstMapLevelKey none = {NULL, NULL, NULL, NULL};
    if (key == NULL || strcmp(key, "asyncfx-hit") != 0) {
        return none;
    }
    ++g_level_counter;
    snprintf(asyfx_stage, sizeof asyfx_stage, "asyncfx-stage#%d", g_level_counter);
    snprintf(asyfx_code, sizeof asyfx_code, "asyncfx-code#%d", g_level_counter);
    snprintf(asyfx_level, sizeof asyfx_level, "asyncfx-level#%d", g_level_counter);
    snprintf(asyfx_name, sizeof asyfx_name, "asyncfx-name#%d", g_level_counter);
    struct AsstMapLevelKey hit = {asyfx_stage, asyfx_code, asyfx_level, asyfx_name};
    return hit;
}

const char* AsstGetItemName(const char* id) {
    if (id == NULL || id[0] == '\0') {
        return "Unknown";
    }
    if (strcmp(id, "asyncfx-item") != 0) {
        return "";
    }
    ++g_item_counter;
    snprintf(asyfx_item, sizeof asyfx_item, "asyncfx-item#%d", g_item_counter);
    return asyfx_item;
}
