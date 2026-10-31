// profile.go 定义调用方 C 输入的身份与已核对的文件摘要；不注册版本。

package main

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

// Profile 记录显式输入的快照身份、上游来源与文件集合。
type Profile struct {
	ID               string
	SourceRepository string
	SourceTag        string
	SourceCommit     string
	Files            map[string]ProfileFile
}
