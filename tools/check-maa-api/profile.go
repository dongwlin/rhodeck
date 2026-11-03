package main

const (
	FileAsstCaller      = "upstream/AsstCaller.h"
	FileAsstPort        = "upstream/AsstPort.h"
	FileAsstCallerExtra = "upstream/AsstCallerExtra.h"
	FileAsstTypes       = "upstream/AsstTypes.h"
	FileAsstMsg         = "upstream/AsstMsg.h"
	FileLicense         = "upstream/LICENSE"
)

const (
	PurposeCheckerInput      = "checker_input"
	PurposeSemanticReference = "semantic_reference"
	PurposeUpstreamLicense   = "upstream_license"
)

type ProfileFile struct {
	Purpose string
	Size    int64
	SHA256  string
}

type Profile struct {
	ID               string
	SourceRepository string
	SourceTag        string
	SourceCommit     string
	Files            map[string]ProfileFile
}
