/*
 * fixture.c — MAA-13 独立原生线程 callback fixture(callbackfixture)。
 *
 * 定位与边界:
 *   - 本夹具是 MAA-13 专属的 callback 线程验证夹具,全部导出以 cbfx_
 *     前缀命名,不属于 MaaCore 的 26 个 Asst 导出契约;与 MAA-11 的空
 *     导出 loader fixture(testdata/loaderfixture/fixture.c,签名有意
 *     不对应真实 ABI、绝不可被调用)和 MAA-12 的真实 ABI 夹具
 *     (testdata/abifixture/fixture.c)分目录、分统计维护,三者不得
 *     混用;
 *   - 回调一律经 C 函数指针 void (*)(int32_t msg, const char*
 *     details_json, void* custom_arg) 调用,类型与锁定的
 *     AsstCaller.h:53 AsstApiCallback 完全一致(Linux 上 ASST_CALL
 *     展开为空);
 *   - 原生线程一律以 pthread_create 启动、pthread_join 收束;导出
 *     返回时全部线程已 join,不遗留任何存活线程;
 *   - 本夹具不调用、不依赖任何真实 MaaCore;全部行为来自本文件的
 *     已发表契约,语义确定、可审计;
 *   - 真实调用发生在 Go 侧独立子进程中(超时由父进程外部强制),结果
 *     以 MAA_M2_CALLBACK_*_OK 标记单独报告,与静态 checker、
 *     MAA_M2_LOADER_FIXTURE_* 装载检查和 MAA_M2_ABI_FIXTURE_* 数值/
 *     缓冲区结果分开统计。
 *
 * == 已发表契约(全文) ==
 *
 * 回调调用契约(全部导出共用):
 *   - callback 每线程恰好调用一次,参数依次为该线程的 msg(int32_t,
 *     原样透传,允许任意负值)、details_json(私有堆副本的借用指针)、
 *     custom_arg((void*)(uintptr_t)token —— token 是调用方提供的整
 *     数,夹具只做「整数→指针宽度」的原样透传,从不解引用、从不把
 *     任何 Go 地址交给回调);
 *   - 每个线程启动前先把调用方提供的 details_json(必须是 NUL 结尾
 *     的 C 字符串)strdup 为私有堆副本;callback 返回后线程立即整体
 *     改写副本(改写模式见下),随后把 invoked 置 1 再退出 —— 「回调
 *     返回后原生立即改写缓冲」是 Core 复用 details 缓冲的镜像,Go
 *     侧在回调内复制的快照不得受影响;
 *   - invoked 在回调返回后置 1;join 之后由导出实现读取,未观察到
 *     invoked 即失败。
 *
 * 改写模式:
 *   - cbfx_call_on_thread / cbfx_call_on_threads:逐字节派生改写,
 *     第 i 字节写为 (uint8_t)(i*31+7) —— 递增派生模式,不是恒等写;
 *   - cbfx_call_then_overwrite:整块 memset 为调用方指定的 fill
 *     字节。
 *
 * 导出与返回值约定(返回 int;0 = 全部成功;置位 (1u<<i) = 线程 i
 * 创建失败或 join 后未观察到 invoked;-1 = 任一私有副本分配失败):
 *   - int cbfx_call_on_thread(cbfx_callback callback, int32_t msg,
 *         const char* details_json, uintptr_t token)
 *     启动 1 个线程(逐字节派生改写模式),join 后返回;
 *   - int cbfx_call_on_threads(cbfx_callback callback,
 *         int32_t msg0, const char* details0, uintptr_t token0,
 *         int32_t msg1, const char* details1, uintptr_t token1,
 *         int32_t msg2, const char* details2, uintptr_t token2)
 *     一次性并发启动 3 个线程(各带独立的 msg/details/token;3 线程
 *     全部创建完成前不等待任何一线程),全部 join 后返回;
 *   - int cbfx_call_then_overwrite(cbfx_callback callback, int32_t msg,
 *         const char* details_json, uintptr_t token, uint8_t fill)
 *     启动 1 个线程,专用于「调用后立即改写」验证:callback 返回后
 *     以 memset(copy, fill, len) 整块改写,join 后返回。
 */

#include <pthread.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

/* 回调函数指针类型:与 AsstCaller.h:53 的 AsstApiCallback 一致。 */
typedef void (*cbfx_callback)(int32_t msg, const char* details_json,
                              void* custom_arg);

/* 线程作业:一个实例对应一个线程。details 是本线程的私有堆副本,
 * 归作业所有,join 后由导出实现释放。 */
typedef struct {
    cbfx_callback callback;
    int32_t msg;
    uintptr_t token;
    char* details;
    size_t details_len;
    int overwrite_fill; /* 1 = memset(fill);0 = 逐字节派生模式 */
    uint8_t fill;
    pthread_t tid;
    int started; /* pthread_create 成功置 1 */
    int invoked; /* 回调返回后置 1 */
} cbfx_thread_job;

/* 填充一个作业:复制 details 为私有堆副本。复制失败返回 -1(此时
 * details 为 NULL,join 实现会统一释放)。 */
static int cbfx_fill_job(cbfx_thread_job* job, cbfx_callback callback,
                         int32_t msg, const char* details_json,
                         uintptr_t token, int overwrite_fill, uint8_t fill) {
    job->callback = callback;
    job->msg = msg;
    job->token = token;
    job->overwrite_fill = overwrite_fill;
    job->fill = fill;
    job->started = 0;
    job->invoked = 0;
    job->details_len = 0;
    if (details_json == NULL) {
        job->details = NULL;
        return -1;
    }
    job->details_len = strlen(details_json);
    job->details = strdup(details_json);
    return job->details == NULL ? -1 : 0;
}

/* 线程主例程:调用 callback → 置 invoked → 立即整体改写私有副本。 */
static void* cbfx_thread_main(void* raw) {
    cbfx_thread_job* job = (cbfx_thread_job*)raw;
    job->callback(job->msg, job->details, (void*)(uintptr_t)job->token);
    job->invoked = 1;
    if (job->overwrite_fill) {
        memset(job->details, (int)job->fill, job->details_len);
    } else {
        for (size_t i = 0; i < job->details_len; ++i) {
            job->details[i] = (char)(unsigned char)(i * 31u + 7u);
        }
    }
    return NULL;
}

/* 启动 count 个线程并全部 join;返回值契约见文件头。本函数接管全部
 * 作业的收束与释放责任:创建失败的线程跳过 join,其余全部 join;
 * join 后逐线程核对 invoked。 */
static int cbfx_spawn_join_all(cbfx_thread_job* jobs, int count) {
    int failures = 0;
    for (int i = 0; i < count; ++i) {
        if (jobs[i].details == NULL) {
            for (int j = 0; j < count; ++j) {
                free(jobs[j].details);
                jobs[j].details = NULL;
            }
            return -1;
        }
    }
    for (int i = 0; i < count; ++i) {
        if (pthread_create(&jobs[i].tid, NULL, cbfx_thread_main, &jobs[i]) != 0) {
            failures |= (int)(1u << i);
        } else {
            jobs[i].started = 1;
        }
    }
    for (int i = 0; i < count; ++i) {
        if (!jobs[i].started) {
            continue;
        }
        pthread_join(jobs[i].tid, NULL);
        if (!jobs[i].invoked) {
            failures |= (int)(1u << i);
        }
    }
    for (int i = 0; i < count; ++i) {
        free(jobs[i].details);
        jobs[i].details = NULL;
    }
    return failures;
}

/* 导出 1:单线程调用(逐字节派生改写模式)。 */
int cbfx_call_on_thread(cbfx_callback callback, int32_t msg,
                        const char* details_json, uintptr_t token) {
    cbfx_thread_job job;
    if (cbfx_fill_job(&job, callback, msg, details_json, token, 0, 0) != 0) {
        return -1;
    }
    return cbfx_spawn_join_all(&job, 1);
}

/* 导出 2:一次性并发启动 3 个线程,各带独立 msg/details/token。 */
int cbfx_call_on_threads(cbfx_callback callback,
                         int32_t msg0, const char* details0, uintptr_t token0,
                         int32_t msg1, const char* details1, uintptr_t token1,
                         int32_t msg2, const char* details2, uintptr_t token2) {
    cbfx_thread_job jobs[3];
    if (cbfx_fill_job(&jobs[0], callback, msg0, details0, token0, 0, 0) != 0 ||
        cbfx_fill_job(&jobs[1], callback, msg1, details1, token1, 0, 0) != 0 ||
        cbfx_fill_job(&jobs[2], callback, msg2, details2, token2, 0, 0) != 0) {
        for (int i = 0; i < 3; ++i) {
            free(jobs[i].details);
            jobs[i].details = NULL;
        }
        return -1;
    }
    return cbfx_spawn_join_all(jobs, 3);
}

/* 导出 3:「调用后立即改写」专用(fill 整块 memset)。 */
int cbfx_call_then_overwrite(cbfx_callback callback, int32_t msg,
                             const char* details_json, uintptr_t token,
                             uint8_t fill) {
    cbfx_thread_job job;
    if (cbfx_fill_job(&job, callback, msg, details_json, token, 1, fill) != 0) {
        return -1;
    }
    return cbfx_spawn_join_all(&job, 1);
}
