// profile.go 维护锁定 profile 注册表:生产注册表只含 maa-v6.20.1,
// 文件 size 与 SHA-256 以 checker 内部常量为期望值,不信任输入清单
// 自带字段(清单字段只用于与内部值交叉核对,不一致同样失败)。
// profile/target 查找只对已注册 profile 开放。测试在 _test.go 中直接
// 写入合成 profile 并用 t.Cleanup 恢复注册表;生产入口不得提供跳过
// 摘要或未锁定 profile 的逃逸参数。

package main

// LockedProfileID 是生产注册表中唯一 profile 的 ID;source 标识只含
// 公开上游来源(tag 与 commit 对应 MAA v6.20.1)。
const (
	LockedProfileID = "maa-v6.20.1"

	lockedProfileRepository = "https://github.com/MaaAssistantArknights/MaaAssistantArknights"
	lockedProfileTag        = "v6.20.1"
	lockedProfileCommit     = "d94dd007aca3b519395fb1f0aee26c7bd5ba5d5f"
)

// c 输入在清单中的归档相对路径;checker_input 三个头文件必需,
// semantic_reference 与 license 条目缺省可容忍,存在即校验。
const (
	FileAsstCaller      = "upstream/AsstCaller.h"
	FileAsstPort        = "upstream/AsstPort.h"
	FileAsstCallerExtra = "upstream/AsstCallerExtra.h"
	FileAsstTypes       = "upstream/AsstTypes.h"
	FileAsstMsg         = "upstream/AsstMsg.h"
	FileLicense         = "upstream/LICENSE"
)

// 清单条目用途;只有 checker_input 是必需文件。
const (
	PurposeCheckerInput      = "checker_input"
	PurposeSemanticReference = "semantic_reference"
	PurposeUpstreamLicense   = "upstream_license"
)

// ProfileFile 记录单个归档文件的用途与期望摘要。
type ProfileFile struct {
	Purpose string
	Size    int64
	SHA256  string
}

// Profile 是锁定 profile:ID、公开上游来源标识与已知文件集合。
type Profile struct {
	ID               string
	SourceRepository string
	SourceTag        string
	SourceCommit     string
	Files            map[string]ProfileFile
}

// newLockedProfile 返回 maa-v6.20.1 的锁定常量;size 与 SHA-256 逐
// 字节对应上游 v6.20.1 commit d94dd007... 的归档原文。
func newLockedProfile() Profile {
	return Profile{
		ID:               LockedProfileID,
		SourceRepository: lockedProfileRepository,
		SourceTag:        lockedProfileTag,
		SourceCommit:     lockedProfileCommit,
		Files: map[string]ProfileFile{
			FileAsstCaller: {Purpose: PurposeCheckerInput, Size: 5413,
				SHA256: "8f1245d55198364bc15beaa2ff91456c747e258beb7a7547f80751563f8c94b7"},
			FileAsstPort: {Purpose: PurposeCheckerInput, Size: 1087,
				SHA256: "1c99ea5ceb121e323fefbd4116e344efa73640b719474b738aad1b15b6de4a64"},
			FileAsstCallerExtra: {Purpose: PurposeCheckerInput, Size: 1312,
				SHA256: "9229054cf8cd6a36eb4ae59e4927fb3e16de89800bf922f96b67d0c672450737"},
			FileAsstTypes: {Purpose: PurposeSemanticReference, Size: 29622,
				SHA256: "884f71e8804808aa79a1c01249532c4ada462ea8fdbc83c66b6ed9a751b3200b"},
			FileAsstMsg: {Purpose: PurposeSemanticReference, Size: 2636,
				SHA256: "f6a8b551effa5a3a8cca42805b5a09c9cfa7f53a3f3e01f4bad5b858ee6df7c6"},
			FileLicense: {Purpose: PurposeUpstreamLicense, Size: 34523,
				SHA256: "8486a10c4393cee1c25392769ddd3b2d6c242d6ec7928e1414efff7dfb2f07ef"},
		},
	}
}

// profileRegistry 是 profile 注册表:生产只注册锁定 profile;测试在
// _test.go 可达的内部接口上写入合成 profile,生产入口不暴露注册或
// 跳过机制。
var profileRegistry = map[string]Profile{LockedProfileID: newLockedProfile()}

// LookupProfile 按 ID 查找已注册 profile;未注册 ID 一律失败。
func LookupProfile(id string) (Profile, bool) {
	p, ok := profileRegistry[id]
	return p, ok
}
