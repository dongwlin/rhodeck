/*
 * fixture.c — core-worker Runtime 日志与实验查询验证用受控原生库
 * (queryfixture)。
 *
 * 定位与边界:
 *   - 本夹具按锁定的 C 头文件 AsstCaller.h 与 AsstCallerExtra.h 独立
 *     手写全部必需导出、两个实验导出(AsstGetMapLevelKey、
 *     AsstGetItemName)的真实签名,供真实 core-worker 二进制经 maa-go
 *     绑定动态加载,验收 log / map_level_key / item_name 三条命令的
 *     生产映射;
 *   - 与既有夹具分工不同:abifixture 验证绑定层 ABI 与缓冲区协议
 *     细节,imagefixture 验证跨进程大图像传输(且不导出实验符号,
 *     供能力缺席用例使用);本夹具专供日志与实验查询,三者不混用、
 *     互不替代;
 *   - 不调用、不依赖任何真实 MaaCore;除记录的日志与实验查询内容
 *     外,行为全部确定为宽松成功(非 NULL 即 1),不验证绑定层已覆盖
 *     的输入校验细节;
 *   - 真实调用发生在 core-worker 子进程内(由监管客户端测试驱动),
 *     本夹具从不调用 callback、从不输出 stdout。
 *
 * == 已发表契约 ==
 *
 * 常量:
 *   - NullSize 哨兵:0xFFFFFFFFFFFFFFFF(锁定 profile 值,Open 必核验);
 *   - 版本:"v6.20.1-queryfixture";
 *   - 日志读回分隔符:0x1F(ASCII unit separator,合法单字节 UTF-8)。
 *
 * 日志(AsstLog + AsstGetUUID 读回通道):
 *   - AsstLog 把 level/message 原样拷贝进静态缓冲(各 256/2048 字节,
 *     含 NUL 截断保护)并递增调用计数 g_log_count;这是测试读取
 *     "AsstLog 实际收到什么"的唯一事实来源;
 *   - AsstGetUUID 返回读回串 "<level>\x1f<message>\x1f<count>"(count
 *     为十进制调用计数):未记录时为 "\x1f\x1f0"。buff == NULL 或容量
 *     不足 → NullSize(驱动绑定扩容探测);足够 → 拷贝全部内容并返回
 *     字节数,不写 NUL。读回串总长受 UUID 缓冲上限(默认 4096)约束,
 *     测试输入必须留有余量。
 *
 * 实验查询(AsstCallerExtra.h,27-28/28):
 *   - AsstGetMapLevelKey:key == "queryfx-hit" 时按值返回四指针
 *     struct,四个成员写入 thread_local 缓冲,内容互不相同且带调用
 *     计数("queryfx-stage#N"/"queryfx-code#N"/"queryfx-level#N"/
 *     "queryfx-name#N",N 从 1 起每次调用 +1)——后续调用改写缓冲不
 *     影响调用方已复制的内容,字段错位与陈旧视图可辨;
 *     key == "queryfx-badutf8" 时 stage_id 指向非法 UTF-8 字节串
 *     {0xFF,0xFE,0}(镜像坏原生字符串,绑定按协议破坏拒绝);
 *     key == "queryfx-nonul" 时 stage_id 指向 8192 字节无 NUL 的
 *     'x' 串(超出绑定 4097 字节扫描窗,协议破坏);其余 key(含
 *     NULL)返回全 NULL struct(镜像 Core 的「未找到」语义);
 *   - AsstGetItemName:id 为 NULL 或空串返回 "Unknown"(静态字面
 *     量);id == "queryfx-item" 返回 thread_local 缓冲的借用指针,
 *     内容 "queryfx-item#N"(N 从 1 起每次调用 +1);id ==
 *     "queryfx-nil" 返回 NULL(镜像 C 契约破坏:恒非 NULL);
 *     id == "queryfx-badutf8" 返回非法 UTF-8 字节串;其余 id 返回
 *     空串(非 NULL,合法的「空名称」结果);
 *   - 两个符号是可选符号:符号是否在库内由夹具恒导出决定,能力门
 *     由 Go 侧能力快照表达,与本夹具无关;能力缺席用例由不导出实验
 *     符号的 imagefixture 承载。
 *
 * 其余导出(绑定层已由 abifixture 深测,这里只保证流程可走通):
 *   - SetUserDir/LoadResource/SetStaticOption/SetInstanceOption/
 *     SetTaskParams:非 NULL 参数恒返回 1;
 *   - CreateEx:返回固定非零句柄 0x00C0FFEE;从不调用 callback;
 *   - Destroy/SetConnectionExtras/Log:void;
 *   - Start/Stop/BackToHome:恒返回 1;Running/Connected:恒返回 0;
 *   - AppendTask:恒返回 7;AsyncConnect/AsyncClick/AsyncScreencap:
 *     恒返回固定非零 ID;
 *   - GetImage:容量不足返回 NullSize,足够返回 0(合法空图像);
 *   - GetTasksList:容量不足返回 NullSize,足够返回 0(空任务列表)。
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

#define QRYFX_NULL_SIZE 0xFFFFFFFFFFFFFFFFULL
#define QRYFX_CREATE_HANDLE 0x00C0FFEE
static const char QRYFX_VERSION[] = "v6.20.1-queryfixture";

/* == 日志记录与读回通道 == */

static char qryfx_log_level[256];
static char qryfx_log_message[2048];
static int g_log_count = 0;

/* == 实验查询的 thread_local 存储:每次命中调用整体改写,镜像 Core
 * 的借出语义(只在本次调用与同线程的下一次调用之间有效)。 == */

static __thread char qryfx_level_stage[32];
static __thread char qryfx_level_code[32];
static __thread char qryfx_level_level[32];
static __thread char qryfx_level_name[32];
static __thread char qryfx_item_name[64];
static int g_level_counter = 0;
static int g_item_counter = 0;

/* 坏原生字符串的常量证据:非法 UTF-8 与无 NUL 超长串。 */
static const char qryfx_bad_utf8[] = {'\xFF', '\xFE', '\0'};
static char qryfx_no_nul[8192]; /* 构造期填满 'x',全程无 NUL,见下 */

__attribute__((constructor)) static void qryfx_init(void) {
    memset(qryfx_no_nul, 'x', sizeof qryfx_no_nul);
}

/* == 导出(必需 23 项 + 实验可选项 2 项) == */

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
    (void)callback;
    (void)custom_arg;
    /* 从不调用 callback;固定非零句柄即可。 */
    return (AsstHandle)(uintptr_t)QRYFX_CREATE_HANDLE;
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
    (void)address;
    (void)config;
    return (AsstAsyncCallId)(0x1111 + (int32_t)block);
}

void AsstSetConnectionExtras(const char* name, const char* extras) {
    (void)name;
    (void)extras;
}

AsstAsyncCallId AsstAsyncClick(AsstHandle handle, int32_t x, int32_t y,
                               AsstBool block) {
    (void)handle;
    (void)x;
    (void)y;
    return (AsstAsyncCallId)(0x2222 + (int32_t)block);
}

AsstAsyncCallId AsstAsyncScreencap(AsstHandle handle, AsstBool block) {
    (void)handle;
    return (AsstAsyncCallId)(0x3333 + (int32_t)block);
}

/* AsstGetImage — 恒定空图像:容量不足返回 NullSize,足够返回 0。 */
AsstSize AsstGetImage(AsstHandle handle, void* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL) {
        return QRYFX_NULL_SIZE;
    }
    return 0;
}

AsstSize AsstGetUUID(AsstHandle handle, char* buff, AsstSize buff_size) {
    (void)handle;
    char readback[2304]; /* <level>\x1f<message>\x1f<count> 的上界 */
    int n = snprintf(readback, sizeof readback, "%s\x1f%s\x1f%d",
                     qryfx_log_level, qryfx_log_message, g_log_count);
    if (n < 0 || (AsstSize)n >= sizeof readback) {
        return QRYFX_NULL_SIZE; /* 记录内容本身超界:契约破坏,不截断 */
    }
    if (buff == NULL || buff_size < (AsstSize)n) {
        return QRYFX_NULL_SIZE;
    }
    memcpy(buff, readback, (size_t)n); /* 不写 NUL:按返回长度读取 */
    return (AsstSize)n;
}

AsstSize AsstGetTasksList(AsstHandle handle, AsstTaskId* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL || buff_size < 1) {
        return QRYFX_NULL_SIZE;
    }
    return 0; /* 空任务列表 */
}

AsstSize AsstGetNullSize(void) {
    return QRYFX_NULL_SIZE;
}

const char* AsstGetVersion(void) {
    return QRYFX_VERSION;
}

/* AsstLog — 原样记录输入并计数;读回通道见 AsstGetUUID。 */
void AsstLog(const char* level, const char* message) {
    snprintf(qryfx_log_level, sizeof qryfx_log_level, "%s",
             level != NULL ? level : "");
    snprintf(qryfx_log_message, sizeof qryfx_log_message, "%s",
             message != NULL ? message : "");
    ++g_log_count;
}

/* == 27-28/28: AsstCallerExtra.h 实验导出(类型与成员顺序逐字对应
 * AsstCallerExtra.h:13-19、32、41) == */

struct AsstMapLevelKey {
    const char* stage_id;   /* AsstCallerExtra.h:15 */
    const char* code;       /* AsstCallerExtra.h:16 */
    const char* level_id;   /* AsstCallerExtra.h:17 */
    const char* name;       /* AsstCallerExtra.h:18 */
};

/* 27/28: AsstGetMapLevelKey — 命中返回带调用计数的四元组(按值返回,
 * 32 字节 → 内存返回 ABI),坏字符串镜像键返回协议破坏证据,其余
 * (含 NULL key)返回全 NULL struct。 */
struct AsstMapLevelKey AsstGetMapLevelKey(const char* key) {
    struct AsstMapLevelKey none = {NULL, NULL, NULL, NULL};
    if (key == NULL) {
        return none;
    }
    if (strcmp(key, "queryfx-badutf8") == 0) {
        struct AsstMapLevelKey bad = {qryfx_bad_utf8, "", "", ""};
        return bad;
    }
    if (strcmp(key, "queryfx-nonul") == 0) {
        struct AsstMapLevelKey bad = {qryfx_no_nul, "", "", ""};
        return bad;
    }
    if (strcmp(key, "queryfx-hit") != 0) {
        return none;
    }
    ++g_level_counter;
    snprintf(qryfx_level_stage, sizeof qryfx_level_stage, "queryfx-stage#%d", g_level_counter);
    snprintf(qryfx_level_code, sizeof qryfx_level_code, "queryfx-code#%d", g_level_counter);
    snprintf(qryfx_level_level, sizeof qryfx_level_level, "queryfx-level#%d", g_level_counter);
    snprintf(qryfx_level_name, sizeof qryfx_level_name, "queryfx-name#%d", g_level_counter);
    struct AsstMapLevelKey hit = {qryfx_level_stage, qryfx_level_code,
                                  qryfx_level_level, qryfx_level_name};
    return hit;
}

/* 28/28: AsstGetItemName — NULL/空串 → "Unknown"(恒非 NULL),已知
 * 物品返回带调用计数的借用指针,契约破坏镜像 id 返回 NULL 或非法
 * UTF-8,其余返回空串(非 NULL)。 */
const char* AsstGetItemName(const char* id) {
    if (id == NULL || id[0] == '\0') {
        return "Unknown";
    }
    if (strcmp(id, "queryfx-nil") == 0) {
        return NULL;
    }
    if (strcmp(id, "queryfx-badutf8") == 0) {
        return qryfx_bad_utf8;
    }
    if (strcmp(id, "queryfx-item") != 0) {
        return "";
    }
    ++g_item_counter;
    snprintf(qryfx_item_name, sizeof qryfx_item_name, "queryfx-item#%d", g_item_counter);
    return qryfx_item_name;
}
