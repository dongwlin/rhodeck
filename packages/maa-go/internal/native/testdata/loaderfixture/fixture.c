/*
 * loaderfixture.c — 独立装载夹具(loader fixture)。
 *
 * 契约与边界:
 *   - 本夹具只证明 dlopen 装载、dlsym 符号解析与两阶段注册事务
 *     (解析→注册→发布),不证明任何 ABI 兼容性;
 *   - 全部导出是空函数(函数体为空),签名有意不对应真实 ABI
 *     (统一写成 void 名字(void)),绝不可被调用;
 *   - 真实 ABI fixture 位于 testdata/abifixture/fixture.c,两者不得混用;
 *   - 装载验证结果在 Go 侧单独标记
 *     (MAA_LOADER_FIXTURE_COMPLETE_OK / MAA_LOADER_FIXTURE_MISSING_OK),
 *     不计入 ABI 调用通过统计。
 *
 * 导出清单:28 个导出逐一对应 native 包 symbolSpecs 的 28 条,顺序
 * 一致,并逐条注明 required/optional 与平台属性。
 *
 * 变体控制:
 *   - 默认(完整变体):28 个导出全部在场;
 *   - -DLOADER_FIXTURE_MISSING(缺失变体):省略 3 个 required
 *     allPlatforms 符号 AsstSetUserDir、AsstAsyncScreencap、AsstLog
 *     (位于 allPlatforms 清单(27 条)第 1、18、25 条——首部、后部、
 *     尾部,便于验收聚合诊断的顺序);
 *   - 缺失集合必须与 Go 测试(loaderfixture_linux_test.go 的
 *     expectedMissingFixtureSymbols)严格一致:任何一侧改动都必须
 *     同步另一侧。
 */

/*  1/28: AsstSetUserDir — required, allPlatforms;缺失变体省略 */
#ifdef LOADER_FIXTURE_MISSING
/* 缺失变体:本必需符号不在场 */
#else
void AsstSetUserDir(void) {}
#endif

/*  2/28: AsstLoadResource — required, allPlatforms */
void AsstLoadResource(void) {}

/*  3/28: AsstSetStaticOption — required, allPlatforms */
void AsstSetStaticOption(void) {}

/*  4/28: AsstCreate — optional, allPlatforms */
void AsstCreate(void) {}

/*  5/28: AsstCreateEx — required, allPlatforms */
void AsstCreateEx(void) {}

/*  6/28: AsstDestroy — required, allPlatforms */
void AsstDestroy(void) {}

/*  7/28: AsstSetInstanceOption — required, allPlatforms */
void AsstSetInstanceOption(void) {}

/*  8/28: AsstAppendTask — required, allPlatforms */
void AsstAppendTask(void) {}

/*  9/28: AsstSetTaskParams — required, allPlatforms */
void AsstSetTaskParams(void) {}

/* 10/28: AsstStart — required, allPlatforms */
void AsstStart(void) {}

/* 11/28: AsstStop — required, allPlatforms */
void AsstStop(void) {}

/* 12/28: AsstRunning — required, allPlatforms */
void AsstRunning(void) {}

/* 13/28: AsstConnected — required, allPlatforms */
void AsstConnected(void) {}

/* 14/28: AsstBackToHome — required, allPlatforms */
void AsstBackToHome(void) {}

/* 15/28: AsstAsyncConnect — required, allPlatforms */
void AsstAsyncConnect(void) {}

/* 16/28: AsstSetConnectionExtras — required, allPlatforms */
void AsstSetConnectionExtras(void) {}

/* 17/28: AsstAsyncClick — required, allPlatforms */
void AsstAsyncClick(void) {}

/* 18/28: AsstAsyncScreencap — required, allPlatforms;缺失变体省略 */
#ifdef LOADER_FIXTURE_MISSING
/* 缺失变体:本必需符号不在场 */
#else
void AsstAsyncScreencap(void) {}
#endif

/* 19/28: AsstGetImage — required, allPlatforms */
void AsstGetImage(void) {}

/* 20/28: AsstGetImageBgr — optional, allPlatforms */
void AsstGetImageBgr(void) {}

/* 21/28: AsstGetUUID — required, allPlatforms */
void AsstGetUUID(void) {}

/* 22/28: AsstGetTasksList — required, allPlatforms */
void AsstGetTasksList(void) {}

/* 23/28: AsstGetNullSize — required, allPlatforms */
void AsstGetNullSize(void) {}

/* 24/28: AsstGetVersion — required, allPlatforms */
void AsstGetVersion(void) {}

/* 25/28: AsstLog — required, allPlatforms;缺失变体省略 */
#ifdef LOADER_FIXTURE_MISSING
/* 缺失变体:本必需符号不在场 */
#else
void AsstLog(void) {}
#endif

/* 26/28: AsstAsyncAttachWindow — optional, windowsOnly;
 * 夹具在 Linux 上同样导出本符号,是否查找由 Go 侧 symbolSpecs 的
 * 平台属性决定:非 Windows 平台不查找,不影响装载结果与能力快照。 */
void AsstAsyncAttachWindow(void) {}

/* 27/28: AsstGetMapLevelKey — optional, allPlatforms(实验导出,
 * ASST_WITH_EXTRA_CALLERS 条件;夹具恒导出,缺席只关闭能力)。 */
void AsstGetMapLevelKey(void) {}

/* 28/28: AsstGetItemName — optional, allPlatforms(实验导出,
 * ASST_WITH_EXTRA_CALLERS 条件;夹具恒导出,缺席只关闭能力)。 */
void AsstGetItemName(void) {}
