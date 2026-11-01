package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/apps/cli/internal/plan"
	"github.com/spf13/cobra"
)

// Options are the plan-facing flags shared by run, dry-run and doctor.
type Options struct {
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

// register binds the shared plan flags to one command. The names, defaults
// and usage strings are the accepted CLI contract.
func (o *Options) register(cmd *cobra.Command) {
	flags := cmd.Flags()
	flags.StringVar(&o.input, "input", "", "plan JSON file, or - for stdin")
	flags.StringVar(&o.worker, "worker", "", "absolute Core worker executable path")
	flags.StringVar(&o.dataDir, "data-dir", "", "absolute private control data directory")
	flags.DurationVar(&o.timeout, "timeout", 0, "run deadline; zero disables the deadline")
	flags.DurationVar(&o.startupTimeout, "startup-timeout", 10*time.Second, "worker startup deadline")
	flags.DurationVar(&o.requestTimeout, "request-timeout", 30*time.Second, "worker request deadline")
	flags.DurationVar(&o.stopTimeout, "stop-timeout", 5*time.Second, "graceful stop deadline")
	flags.DurationVar(&o.shutdownTimeout, "shutdown-timeout", 5*time.Second, "worker shutdown deadline")
	flags.DurationVar(&o.outputTimeout, "output-timeout", 5*time.Second, "run output write deadline")
	flags.BoolVar(&o.includeRaw, "include-raw-events", false, "include private raw Core callback JSON")
}

// registerDryRun binds the preview switch that only run accepts.
func (o *Options) registerDryRun(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "validate and preview without native or device actions")
}

// validate rejects unusable options. It runs before the plan is read, so an
// unusable invocation never reports a plan problem. A zero timeout is legal
// and disables the deadline; every other deadline must be positive.
func (o *Options) validate() error {
	if o.input == "" || !validAbsolutePath(o.worker) || !validAbsolutePath(o.dataDir) ||
		o.timeout < 0 || o.startupTimeout <= 0 || o.requestTimeout <= 0 || o.stopTimeout <= 0 ||
		o.shutdownTimeout <= 0 || o.outputTimeout <= 0 {
		return errors.New("invalid CLI options")
	}
	return nil
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}

// planOptions returns the validated options the plan layer needs.
func (o *Options) planOptions() plan.Options {
	return plan.Options{
		Input:           o.input,
		Worker:          o.worker,
		DataDir:         o.dataDir,
		Timeout:         o.timeout,
		StartupTimeout:  o.startupTimeout,
		RequestTimeout:  o.requestTimeout,
		StopTimeout:     o.stopTimeout,
		ShutdownTimeout: o.shutdownTimeout,
		OutputTimeout:   o.outputTimeout,
		IncludeRaw:      o.includeRaw,
	}
}
