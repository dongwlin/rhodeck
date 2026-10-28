/*
 * fixture.c — core-worker 图像传输验证用受控原生库(imagefixture)。
 *
 * 定位与边界:
 *   - 本夹具按锁定的 C 头文件 AsstCaller.h 独立手写全部必需导出与
 *     两个图像可选项(AsstCreate、AsstGetImageBgr)的真实签名,供真实
 *     core-worker 二进制经 maa-go 绑定动态加载,验收 get_image_png/
 *     get_image_bgr 分块传输对大图像的逐字节保真;
 *   - 与 maa-go 的 abifixture(testdata/abifixture/fixture.c)分工不同:
 *     abifixture 验证绑定层 ABI 与缓冲区协议细节,图像固定为 16/12
 *     字节;本夹具的图像大小由环境变量决定,专供跨进程大数据传输
 *     验收,两者不混用、互不替代;
 *   - 不调用、不依赖任何真实 MaaCore;除图像内容外行为全部确定为
 *     宽松成功(非 NULL 即 1),不验证绑定层已覆盖的输入校验细节;
 *   - 真实调用发生在 core-worker 子进程内(由监管客户端测试驱动),
 *     本夹具从不调用 callback、从不输出 stdout。
 *
 * == 已发表契约 ==
 *
 * 常量:
 *   - NullSize 哨兵:0xFFFFFFFFFFFFFFFF(锁定 profile 值,Open 必核验);
 *   - 版本:"v6.20.1-imagefixture";
 *   - UUID 内容:"image-fixture-uuid";
 *   - PNG salt 0x50、BGR salt 0x47。
 *
 * 图像(AsstGetImage / AsstGetImageBgr):
 *   - 大小分别取环境变量 RHODECK_IMAGE_FIXTURE_PNG_BYTES /
 *     RHODECK_IMAGE_FIXTURE_BGR_BYTES 的十进制值,缺省或解析失败为
 *     0(合法空图像);每次 GetImage 调用重新读取(已缓存,子进程内
 *     环境不变,取值稳定);
 *   - buff == NULL 或容量不足 → NullSize(驱动绑定扩容探测);
 *   - 足够 → 逐字节填充确定性图案 buff[i] = (uint8_t)(i*131 + salt)
 *     并返回图像字节数;i 为 uint64 参与运算后截断,Go 侧测试以同一
 *     公式独立复现,任何字节分歧都可见;
 *   - 大小为 0 时在实际非空 buffer 上返回 0(合法空图像,与 NullSize
 *     可辨)。
 *
 * 其余导出(绑定层已由 abifixture 深测,这里只保证流程可走通):
 *   - SetUserDir/LoadResource/SetStaticOption/SetInstanceOption/
 *     SetTaskParams:非 NULL 参数恒返回 1;
 *   - Create/CreateEx:返回固定非零句柄 0x00C0FFEE;从不调用 callback;
 *   - Destroy/SetConnectionExtras/Log:void;
 *   - Start/Stop/BackToHome:恒返回 1(关闭路径的 Stop 拒绝会变成
 *     stop diagnostic,这里不制造);
 *   - Running/Connected:恒返回 0;
 *   - AppendTask:恒返回 7;AsyncConnect/AsyncClick/AsyncScreencap:
 *     恒返回固定非零 ID;
 *   - GetUUID:拷贝固定 UUID,不写 NUL,容量不足返回 NullSize;
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

#define IMGFX_NULL_SIZE 0xFFFFFFFFFFFFFFFFULL
#define IMGFX_CREATE_HANDLE 0x00C0FFEE
#define IMGFX_PNG_SALT 0x50
#define IMGFX_BGR_SALT 0x47
static const char IMGFX_VERSION[] = "v6.20.1-imagefixture";
static const char IMGFX_UUID[] = "image-fixture-uuid"; /* 18 字节 */

/* == 图像大小与环境变量 == */

/* 解析一次并缓存;缺省或非法取 0(合法空图像)。 */
static size_t imgfx_env_size(const char* name) {
    static size_t png_size, bgr_size;
    static int ready = 0;
    if (!ready) {
        ready = 1;
        const char* png = getenv("RHODECK_IMAGE_FIXTURE_PNG_BYTES");
        const char* bgr = getenv("RHODECK_IMAGE_FIXTURE_BGR_BYTES");
        if (png != NULL) {
            png_size = (size_t)strtoull(png, NULL, 10);
        }
        if (bgr != NULL) {
            bgr_size = (size_t)strtoull(bgr, NULL, 10);
        }
    }
    return strcmp(name, "png") == 0 ? png_size : bgr_size;
}

/* 确定性图案:buff[i] = (uint8_t)(i*131 + salt);i 按 uint64 参与运算,
 * 与 Go 侧测试的独立复现公式逐字对应。 */
static void imgfx_fill(unsigned char* buff, size_t n, unsigned char salt) {
    for (size_t i = 0; i < n; ++i) {
        buff[i] = (unsigned char)((uint64_t)i * 131u + (uint64_t)salt);
    }
}

static AsstSize imgfx_get_image(void* buff, AsstSize buff_size, size_t n,
                                unsigned char salt) {
    if (buff == NULL || buff_size < (AsstSize)n) {
        return IMGFX_NULL_SIZE;
    }
    imgfx_fill((unsigned char*)buff, n, salt);
    return (AsstSize)n;
}

/* == 导出(必需 23 项 + 可选 Create/GetImageBgr) == */

AsstBool AsstSetUserDir(const char* path) {
    return (AsstBool)(path != NULL);
}

AsstBool AsstLoadResource(const char* path) {
    return (AsstBool)(path != NULL);
}

AsstBool AsstSetStaticOption(AsstStaticOptionKey key, const char* value) {
    return (AsstBool)(key >= 0 && value != NULL);
}

AsstHandle AsstCreate(void) {
    return (AsstHandle)(uintptr_t)IMGFX_CREATE_HANDLE;
}

AsstHandle AsstCreateEx(AsstApiCallback callback, void* custom_arg) {
    (void)callback;
    (void)custom_arg;
    /* 从不调用 callback;固定非零句柄即可。 */
    return (AsstHandle)(uintptr_t)IMGFX_CREATE_HANDLE;
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

/* AsstGetImage — AsstCaller.h:130;大小由环境变量决定,单位字节。 */
AsstSize AsstGetImage(AsstHandle handle, void* buff, AsstSize buff_size) {
    (void)handle;
    return imgfx_get_image(buff, buff_size, imgfx_env_size("png"), IMGFX_PNG_SALT);
}

/* AsstGetImageBgr — AsstCaller.h:131;可选项,恒导出(能力门由 Go 侧
 * 能力快照表达)。 */
AsstSize AsstGetImageBgr(AsstHandle handle, void* buff, AsstSize buff_size) {
    (void)handle;
    return imgfx_get_image(buff, buff_size, imgfx_env_size("bgr"), IMGFX_BGR_SALT);
}

AsstSize AsstGetUUID(AsstHandle handle, char* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL || buff_size < sizeof IMGFX_UUID - 1) {
        return IMGFX_NULL_SIZE;
    }
    memcpy(buff, IMGFX_UUID, sizeof IMGFX_UUID - 1); /* 不写 NUL */
    return sizeof IMGFX_UUID - 1;
}

AsstSize AsstGetTasksList(AsstHandle handle, AsstTaskId* buff, AsstSize buff_size) {
    (void)handle;
    if (buff == NULL || buff_size < 1) {
        return IMGFX_NULL_SIZE;
    }
    return 0; /* 空任务列表 */
}

AsstSize AsstGetNullSize(void) {
    return IMGFX_NULL_SIZE;
}

const char* AsstGetVersion(void) {
    return IMGFX_VERSION;
}

void AsstLog(const char* level, const char* message) {
    (void)level;
    (void)message;
}
