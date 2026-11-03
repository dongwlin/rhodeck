package plan

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// Options are the validated command options the plan layer needs.
type Options struct {
	Input           string
	Worker          string
	DataDir         string
	Timeout         time.Duration
	StartupTimeout  time.Duration
	RequestTimeout  time.Duration
	StopTimeout     time.Duration
	ShutdownTimeout time.Duration
	OutputTimeout   time.Duration
	IncludeRaw      bool
}

// Sink accepts one machine object at a time. done reports that the producer
// finished and release frees the producer's resources; the sink calls release
// when a write is blocked and one of them arrives.
type Sink interface {
	Emit(ctx context.Context, value any, done <-chan struct{}, release func()) error
}

// Service owns the plan workflows. The adapter registry is static and lives
// in the package.
type Service struct{}

// NewService returns a plan service.
func NewService() *Service {
	return &Service{}
}

// Load reads one explicit plan from a file or from stdin. It creates no
// directory, takes no lock and starts no worker.
func (s *Service) Load(input string, stdin io.Reader) (control.Plan, error) {
	if input == "-" {
		return control.DecodePlan(stdin)
	}
	f, err := os.Open(input)
	if err != nil {
		return control.Plan{}, err
	}
	defer f.Close()
	return control.DecodePlan(f)
}

// Prepare compiles the selected adapters and reports the preview. It creates
// no directory, takes no lock, starts no worker, loads no native code and
// connects no device.
func (s *Service) Prepare(p control.Plan) (control.PreparedPlan, []string, error) {
	prepared, err := control.PreparePlan(p, Adapters()...)
	if err != nil {
		return control.PreparedPlan{}, nil, err
	}
	unverified := []string{
		"Core task acceptance and semantics",
		"installation and native dependencies",
		"device connection and task preconditions",
	}
	unverified = append(unverified, adapterUnverified(p)...)
	return prepared, unverified, nil
}

// Execute runs one explicit task batch through the shared control runtime. The
// runtime owns the worker, task association, observations, durable record and
// cleanup. The observations are streamed into the sink; the final result is
// returned after the run finished and the runtime released its directory lock.
func (s *Service) Execute(ctx context.Context, p control.Plan, o Options, sink Sink) (control.Result, error) {
	adapters := Adapters()
	if _, err := control.PreparePlan(p, adapters...); err != nil {
		return control.Result{}, output.Reject(output.CodeInvalidPlan)
	}
	_, pathsOK, _ := CheckLocalPaths(o, p)
	if !pathsOK {
		return control.Result{}, output.Reject(output.CodeEnvironmentRejected)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if o.Timeout != 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, o.Timeout)
		defer stop()
	}
	cfg := control.Config{
		DataDir: o.DataDir, StopTimeout: o.StopTimeout, IncludeRawEvents: o.IncludeRaw,
		Adapters: adapters,
		Worker: worker.Config{Executable: o.Worker, StartupTimeout: o.StartupTimeout,
			RequestTimeout: o.RequestTimeout, ShutdownTimeout: o.ShutdownTimeout},
	}
	rt, err := control.New(cfg)
	if err != nil {
		return control.Result{}, output.Failed(output.CodeFor(err), output.ExitFor(err))
	}
	defer rt.Close(context.Background())
	run, err := rt.Run(ctx, p)
	if err != nil {
		_ = rt.Close(context.Background())
		return control.Result{}, output.Failed(output.CodeFor(err), output.ExitFor(err))
	}
	var (
		runtimeCloseErr error
		closeOnce       sync.Once
	)
	closeRuntime := func() {
		closeOnce.Do(func() {
			if err := rt.Close(context.Background()); err != nil {
				runtimeCloseErr = err
			}
		})
	}
	events, done := run.Events(), run.Done()
	for events != nil || done != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Type == "terminal" {
				continue
			}
			if err := sink.Emit(ctx, event, done, closeRuntime); err != nil {
				cancel()
				closeRuntime()
				return control.Result{}, output.Broken(output.WriteFailureCode(err))
			}
		case <-done:
			done = nil
		}
	}
	result := run.Snapshot()
	closeRuntime()
	if runtimeCloseErr != nil {
		result.Cleanup, result.CleanupCode = "failed", "runtime_close_failed"
	}
	return result, nil
}

// DoctorReport is the result of one installation diagnosis.
type DoctorReport struct {
	Checks      []output.Check
	Unverified  []string
	RuntimeInfo *protocol.RuntimeInfo
	Code        string
	Exit        int
}

// Diagnose reports the explicit installation. It prepares no adapter and
// submits no device action, but it may create the data directory, lock it and
// initialise Core.
func (s *Service) Diagnose(ctx context.Context, p control.Plan, o Options) DoctorReport {
	checks, pathsOK, nativeReady := CheckLocalPaths(o, p)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if o.Timeout != 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, o.Timeout)
		defer stop()
	}
	cfg := control.Config{
		DataDir: o.DataDir, StopTimeout: o.StopTimeout, IncludeRawEvents: o.IncludeRaw,
		Adapters: Adapters(),
		Worker: worker.Config{Executable: o.Worker, StartupTimeout: o.StartupTimeout,
			RequestTimeout: o.RequestTimeout, ShutdownTimeout: o.ShutdownTimeout},
	}
	report := DoctorReport{
		Checks:     checks,
		Unverified: []string{"device connection", "task acceptance and task preconditions"},
		Exit:       output.ExitSuccess,
	}
	if !pathsOK {
		report.Code, report.Exit = output.CodeEnvironmentRejected, output.ExitRejected
	}
	if nativeReady {
		info, err := control.Doctor(ctx, cfg, p.Initialize)
		if err != nil {
			report.Checks = append(report.Checks, output.Check{Field: "native_initialize_and_cleanup", Code: output.CodeFor(err)})
			report.Code, report.Exit = output.CodeFor(err), output.ExitFor(err)
		} else {
			report.RuntimeInfo = &info
			report.Checks = append(report.Checks, output.Check{Field: "native_initialize_and_cleanup", OK: true})
		}
	}
	return report
}
