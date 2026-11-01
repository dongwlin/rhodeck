package plan

import (
	"os"
	"runtime"
	"strconv"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/dongwlin/rhodeck/packages/control"
)

// CheckLocalPaths reports the explicit local installation. The checks never
// touch a device and never open the checked files.
func CheckLocalPaths(o Options, p control.Plan) (checks []output.Check, allOK, nativeReady bool) {
	allOK, nativeReady = true, true
	add := func(field, code string) {
		ok := code == ""
		checks = append(checks, output.Check{Field: field, OK: ok, Code: code})
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
		{"worker", o.Worker, true}, {"initialize.library_path", p.Initialize.LibraryPath, false}, {"connection.adb_path", p.Connection.ADBPath, true},
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
	for i, path := range p.Initialize.ResourceRoots {
		info, err := os.Stat(path)
		code := ""
		if err != nil || !info.IsDir() {
			code = "directory_unavailable"
		}
		add("initialize.resource_roots["+strconv.Itoa(i)+"]", code)
	}
	for _, dir := range []struct{ field, path string }{{"data_dir", o.DataDir}, {"initialize.user_dir", p.Initialize.UserDir}} {
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
