package cli

import (
	"os"
	"runtime"
	"strconv"

	"github.com/dongwlin/rhodeck/packages/control"
)

type localCheck struct {
	Field string `json:"field"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
}

func checkLocalPaths(o options, plan control.Plan) (checks []localCheck, allOK, nativeReady bool) {
	allOK, nativeReady = true, true
	add := func(field, code string) {
		ok := code == ""
		checks = append(checks, localCheck{Field: field, OK: ok, Code: code})
		if !ok {
			allOK = false
			if field != "connection.adb_path" {
				nativeReady = false
			}
		}
	}
	for _, file := range []struct {
		field, path string
		executable  bool
	}{
		{"worker", o.worker, true}, {"initialize.library_path", plan.Initialize.LibraryPath, false}, {"connection.adb_path", plan.Connection.ADBPath, true},
	} {
		info, err := os.Stat(file.path)
		code := ""
		switch {
		case err != nil:
			code = "file_unavailable"
		case !info.Mode().IsRegular():
			code = "not_regular_file"
		case file.executable && runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0:
			code = "not_executable"
		}
		add(file.field, code)
	}
	for i, path := range plan.Initialize.ResourceRoots {
		info, err := os.Stat(path)
		code := ""
		if err != nil || !info.IsDir() {
			code = "directory_unavailable"
		}
		add("initialize.resource_roots["+strconv.Itoa(i)+"]", code)
	}
	for _, dir := range []struct{ field, path string }{{"data_dir", o.dataDir}, {"initialize.user_dir", plan.Initialize.UserDir}} {
		info, err := os.Stat(dir.path)
		code := ""
		if err == nil && !info.IsDir() {
			code = "not_directory"
		} else if err != nil && !os.IsNotExist(err) {
			code = "directory_unavailable"
		}
		add(dir.field, code)
	}
	return checks, allOK, nativeReady
}
