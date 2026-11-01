package app

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/control"
)

type options struct {
	input           string
	worker          string
	dataDir         string
	timeout         time.Duration
	startupTimeout  time.Duration
	requestTimeout  time.Duration
	stopTimeout     time.Duration
	shutdownTimeout time.Duration
	outputTimeout   time.Duration
	dryRun          bool
	includeRaw      bool
}

func parseOptions(command string, args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.input, "input", "", "plan JSON file, or - for stdin")
	fs.StringVar(&o.worker, "worker", "", "absolute Core worker executable path")
	fs.StringVar(&o.dataDir, "data-dir", "", "absolute private control data directory")
	fs.DurationVar(&o.timeout, "timeout", 0, "run deadline; zero disables the deadline")
	fs.DurationVar(&o.startupTimeout, "startup-timeout", 10*time.Second, "worker startup deadline")
	fs.DurationVar(&o.requestTimeout, "request-timeout", 30*time.Second, "worker request deadline")
	fs.DurationVar(&o.stopTimeout, "stop-timeout", 5*time.Second, "graceful stop deadline")
	fs.DurationVar(&o.shutdownTimeout, "shutdown-timeout", 5*time.Second, "worker shutdown deadline")
	fs.DurationVar(&o.outputTimeout, "output-timeout", 5*time.Second, "run output write deadline")
	fs.BoolVar(&o.includeRaw, "include-raw-events", false, "include private raw Core callback JSON")
	if command == "run" {
		fs.BoolVar(&o.dryRun, "dry-run", false, "validate and preview without native or device actions")
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 || o.input == "" || !validAbsolutePath(o.worker) || !validAbsolutePath(o.dataDir) ||
		o.timeout < 0 || o.startupTimeout <= 0 || o.requestTimeout <= 0 || o.stopTimeout <= 0 || o.shutdownTimeout <= 0 || o.outputTimeout <= 0 {
		return o, errors.New("invalid CLI options")
	}
	return o, nil
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}

func readPlan(path string, stdin io.Reader) (control.Plan, error) {
	if path == "-" {
		return control.DecodePlan(stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return control.Plan{}, err
	}
	defer f.Close()
	return control.DecodePlan(f)
}
