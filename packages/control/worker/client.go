// Package worker supervises one Core worker process. It never loads native code.
// A failed session is terminal: callers must explicitly create a new worker and
// decide what to do with any unconfirmed device actions.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

var ErrClosed = errors.New("worker: client is closed")

// Failure means transport/supervision failed. Unknown is true once a request
// might have reached the worker, or execution facts might have been lost. Killing
// a worker never proves that its device actions were undone or cancelled.
type Failure struct {
	Code    string
	Unknown bool
	Cause   error
}

func (e *Failure) Error() string {
	return fmt.Sprintf("worker: %s (result unknown=%t): %v", e.Code, e.Unknown, e.Cause)
}
func (e *Failure) Unwrap() error { return e.Cause }

type Config struct {
	Executable            string
	Args                  []string
	Env                   []string // Appended to the inherited environment; no shell is used.
	ExpectedWorkerVersion string
	StartupTimeout        time.Duration // Defaults to 10 seconds.
	RequestTimeout        time.Duration // Defaults to 30 seconds.
	ShutdownTimeout       time.Duration // Defaults to 5 seconds.
}

type pendingRequest struct {
	name     string
	response chan protocol.Message
}

// Client owns its child process and all IPC goroutines. Use the returned pointer;
// do not copy it. Events has a single consumer. Close and fault recovery never
// depend on that consumer draining the channel.
type Client struct {
	cmd               *exec.Cmd
	stdin             *os.File
	stdout            *os.File
	ctx               context.Context
	cancel            context.CancelCauseFunc
	out               *protocol.Queue
	config            Config
	logs              diagnosticTail
	mu                sync.Mutex
	nextID            uint64
	pending           map[uint64]pendingRequest
	failure           error
	finished          bool
	closing           bool
	closeAcknowledged bool
	greeting          protocol.Greeting
	hello             chan protocol.Greeting
	events            chan protocol.Message
	readDone          chan struct{}
	writeDone         chan struct{}
	processDone       chan struct{}
	done              chan struct{}
	closeOnce         sync.Once
}

// Launch bounds startup with ctx and StartupTimeout. After it returns, the
// session lifetime is owned by Client.Close; cancelling the startup context does
// not abandon an already started worker.
func Launch(ctx context.Context, cfg Config) (*Client, error) {
	if ctx == nil {
		return nil, errors.New("worker: startup requires a context")
	}
	if !filepath.IsAbs(cfg.Executable) || !utf8.ValidString(cfg.Executable) || strings.ContainsRune(cfg.Executable, 0) {
		return nil, errors.New("worker: executable must be an explicit absolute path")
	}
	if cfg.StartupTimeout < 0 || cfg.RequestTimeout < 0 || cfg.ShutdownTimeout < 0 {
		return nil, errors.New("worker: timeouts must not be negative")
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = 10 * time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	childIn, stdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdout, childOut, err := os.Pipe()
	if err != nil {
		childIn.Close()
		stdin.Close()
		return nil, err
	}
	lifetime, cancel := context.WithCancelCause(context.Background())
	c := &Client{ctx: lifetime, cancel: cancel, stdin: stdin, stdout: stdout, config: cfg, out: protocol.NewQueue(), pending: make(map[uint64]pendingRequest), hello: make(chan protocol.Greeting, 1), events: make(chan protocol.Message, protocol.QueueMessages), readDone: make(chan struct{}), writeDone: make(chan struct{}), processDone: make(chan struct{}), done: make(chan struct{})}
	c.cmd = exec.Command(cfg.Executable, cfg.Args...)
	c.cmd.Env = append(os.Environ(), cfg.Env...)
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = childIn, childOut, &c.logs
	c.cmd.WaitDelay = 250 * time.Millisecond
	err = c.cmd.Start()
	childIn.Close()
	childOut.Close()
	if err != nil {
		cancel(err)
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	go c.write()
	go c.read()
	go c.wait()
	startup, stop := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer stop()
	select {
	case greeting := <-c.hello:
		c.greeting = greeting
		return c, nil
	case <-startup.Done():
		c.abort(&Failure{Code: "startup_timeout", Unknown: false, Cause: startup.Err()})
	case <-c.ctx.Done():
	}
	<-c.done
	return nil, c.Err()
}

func (c *Client) Greeting() protocol.Greeting     { return c.greeting }
func (c *Client) Events() <-chan protocol.Message { return c.events }

// Done closes only after the child has been waited for, IPC has stopped and the
// event channel has closed. Buffered events remain readable after Done.
func (c *Client) Done() <-chan struct{} { return c.done }
func (c *Client) Diagnostics() string   { return c.logs.String() }
func (c *Client) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.failure }

func (c *Client) abort(err error) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	if c.failure == nil {
		c.failure = err
	}
	c.mu.Unlock()
	c.cancel(err)
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	_ = c.cmd.Process.Kill()
}

func (c *Client) write() {
	defer close(c.writeDone)
	for {
		frame, err := c.out.Next(c.ctx)
		if err != nil {
			return
		}
		err = protocol.WriteFrame(c.stdin, frame.Data)
		frame.Written <- err
		if err != nil {
			c.abort(&Failure{Code: "write_failed", Unknown: true, Cause: err})
			return
		}
	}
}

func (c *Client) read() {
	defer close(c.readDone)
	reader := protocol.NewReader(c.stdout)
	first := true
	var sequence, coreSequence uint64
	var sizes []int
	retainedBytes := 0
	operations := make(map[int32]uint64)
	for {
		m, err := reader.Read()
		if err != nil {
			c.mu.Lock()
			expected := c.closeAcknowledged
			c.mu.Unlock()
			if err != io.EOF || !expected {
				c.abort(&Failure{Code: "disconnected", Unknown: !first, Cause: err})
			}
			return
		}
		if first {
			var g protocol.Greeting
			if m.Type != protocol.Hello || protocol.Decode(m.Payload, &g) != nil || g.WorkerVersion == "" || g.SessionID == "" || g.OS == "" || g.Arch == "" || g.MaxFrameBytes != protocol.MaxFrameBytes || g.QueueMessages != protocol.QueueMessages || g.QueueBytes != protocol.QueueBytes || (c.config.ExpectedWorkerVersion != "" && g.WorkerVersion != c.config.ExpectedWorkerVersion) {
				c.abort(&Failure{Code: "incompatible_worker", Cause: errors.New("invalid or incompatible worker greeting")})
				return
			}
			first = false
			c.hello <- g
			continue
		}
		switch m.Type {
		case protocol.Response:
			if err := validateResponse(m); err != nil {
				c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: err})
				return
			}
			if m.Name == protocol.Close && m.Error == nil && len(operations) != 0 {
				c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("invalid close acknowledgment or missing operation results")})
				return
			}
			c.mu.Lock()
			pending, ok := c.pending[m.ID]
			if ok && pending.name == m.Name {
				delete(c.pending, m.ID)
				if m.Name == protocol.Close && m.Error == nil {
					c.closeAcknowledged = true
				}
			}
			c.mu.Unlock()
			if !ok || pending.name != m.Name {
				c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("unmatched or duplicate response")})
				return
			}
			if m.Name == protocol.Connect && m.Error == nil {
				var accepted protocol.OperationAccepted
				if protocol.Decode(m.Payload, &accepted) != nil || accepted.AsyncID == 0 || len(operations) >= 128 || operations[accepted.AsyncID] != 0 {
					c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("invalid operation acceptance")})
					return
				}
				operations[accepted.AsyncID] = m.ID
			}
			pending.response <- m
		case protocol.Event:
			if sequence == math.MaxUint64 || m.Sequence != sequence+1 {
				c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("event sequence is not contiguous")})
				return
			}
			sequence = m.Sequence
			switch m.Name {
			case protocol.CoreEvent:
				var callback protocol.Callback
				if protocol.Decode(m.Payload, &callback) != nil || coreSequence == math.MaxUint64 || callback.CoreSequence != coreSequence+1 || !json.Valid(callback.JSON) {
					c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("invalid Core event")})
					return
				}
				coreSequence = callback.CoreSequence
			case protocol.OperationEvent:
				var result protocol.OperationResult
				if protocol.Decode(m.Payload, &result) != nil || result.RequestID == 0 || operations[result.AsyncID] != result.RequestID || result.OK != (result.Error == nil) {
					c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("unmatched operation result")})
					return
				}
				delete(operations, result.AsyncID)
			default:
				c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("unknown worker event")})
				return
			}
			// Only this goroutine sends events. Channel length tells us how many
			// oldest reservations the consumer has released since the last send.
			consumed := len(sizes) - len(c.events)
			for _, size := range sizes[:consumed] {
				retainedBytes -= size
			}
			copy(sizes, sizes[consumed:])
			sizes = sizes[:len(sizes)-consumed]
			size := len(m.Payload) + len(m.Name) + 64
			if size > protocol.QueueBytes-retainedBytes {
				c.abort(&Failure{Code: "event_overflow", Unknown: true, Cause: protocol.ErrQueueFull})
				return
			}
			select {
			case c.events <- m:
				sizes = append(sizes, size)
				retainedBytes += size
			default:
				c.abort(&Failure{Code: "event_overflow", Unknown: true, Cause: protocol.ErrQueueFull})
				return
			}
		default:
			c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("unexpected worker message")})
			return
		}
	}
}

// Validate before waking a caller or acknowledging close. A fast child exit
// must not seal a clean session while another goroutine still decodes bad facts.
func validateResponse(m protocol.Message) error {
	if m.Error != nil {
		return nil
	}
	var target any = &struct{}{}
	switch m.Name {
	case protocol.Initialize:
		target = &protocol.RuntimeInfo{}
	case protocol.Connect:
		target = &protocol.OperationAccepted{}
	case protocol.AppendTask:
		target = &protocol.TaskAccepted{}
	case protocol.Status:
		target = &protocol.ExecutionStatus{}
	case protocol.Stop:
		target = &protocol.StopResult{}
	}
	if err := protocol.Decode(m.Payload, target); err != nil {
		return err
	}
	switch r := target.(type) {
	case *protocol.RuntimeInfo:
		if r.CoreVersion == "" || r.SnapshotID == "" || len(r.LibrarySHA256) != 64 || len(r.Resources) == 0 {
			return errors.New("worker: incomplete runtime information")
		}
	case *protocol.TaskAccepted:
		if r.TaskID == 0 {
			return errors.New("worker: zero task ID")
		}
	}
	return nil
}

func (c *Client) wait() {
	err := c.cmd.Wait()
	close(c.processDone)
	// Pipes are owned by Client, not Cmd.Wait, so a fast exit cannot discard a
	// final response. Bound draining if a descendant inherited a stdout handle.
	timer := time.NewTimer(250 * time.Millisecond)
	select {
	case <-c.readDone:
	case <-timer.C:
		_ = c.stdout.Close()
		<-c.readDone
	}
	timer.Stop()
	c.mu.Lock()
	clean := c.closing && c.closeAcknowledged && len(c.pending) == 0 && c.failure == nil && err == nil
	c.mu.Unlock()
	if !clean {
		if err == nil {
			err = errors.New("worker exited without a confirmed close")
		}
		c.abort(&Failure{Code: "worker_exited", Unknown: true, Cause: err})
	}
	c.mu.Lock()
	c.finished = true
	c.mu.Unlock()
	c.cancel(ErrClosed)
	_ = c.stdin.Close()
	_ = c.stdout.Close()
	<-c.writeDone
	close(c.events)
	close(c.done)
}

func (c *Client) request(ctx context.Context, name string, input any, closing bool) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("worker: request requires a context")
	}
	ctx, stop := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.ctx.Err() != nil || (c.closing && !closing) {
		err := c.failure
		if err == nil {
			err = ErrClosed
		}
		c.mu.Unlock()
		return nil, err
	}
	if len(c.pending) >= protocol.QueueMessages || c.nextID == math.MaxUint64 {
		c.mu.Unlock()
		return nil, &Failure{Code: "request_limit", Cause: protocol.ErrQueueFull}
	}
	c.nextID++
	id := c.nextID
	m, err := protocol.NewMessage(protocol.Request, name, id, input)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	pending := pendingRequest{name: name, response: make(chan protocol.Message, 1)}
	c.pending[id] = pending
	_, err = c.out.Send(m)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	var response protocol.Message
	select {
	case response = <-pending.response:
	case <-ctx.Done():
		select {
		case response = <-pending.response:
		default:
			err = &Failure{Code: "request_timeout", Unknown: true, Cause: ctx.Err()}
			c.abort(err)
			return nil, err
		}
	case <-c.ctx.Done():
		select {
		case response = <-pending.response:
		default:
			return nil, context.Cause(c.ctx)
		}
	}
	if response.Error != nil {
		return response.Payload, response.Error
	}
	return response.Payload, nil
}

func (c *Client) decode(ctx context.Context, name string, input, output any) error {
	raw, err := c.request(ctx, name, input, false)
	if err != nil {
		return err
	}
	if err = protocol.Decode(raw, output); err != nil {
		failure := &Failure{Code: "protocol_error", Unknown: true, Cause: err}
		c.abort(failure)
		return failure
	}
	return nil
}

func (c *Client) Initialize(ctx context.Context, req protocol.InitializeRequest) (protocol.RuntimeInfo, error) {
	var r protocol.RuntimeInfo
	err := c.decode(ctx, protocol.Initialize, req, &r)
	return r, err
}
func (c *Client) Connect(ctx context.Context, req protocol.ConnectRequest) (protocol.OperationAccepted, error) {
	var r protocol.OperationAccepted
	err := c.decode(ctx, protocol.Connect, req, &r)
	return r, err
}
func (c *Client) AppendTask(ctx context.Context, req protocol.TaskRequest) (protocol.TaskAccepted, error) {
	var r protocol.TaskAccepted
	err := c.decode(ctx, protocol.AppendTask, req, &r)
	return r, err
}
func (c *Client) SetTaskParams(ctx context.Context, req protocol.TaskParamsRequest) error {
	return c.decode(ctx, protocol.SetTaskParams, req, &struct{}{})
}
func (c *Client) Start(ctx context.Context) error {
	return c.decode(ctx, protocol.Start, nil, &struct{}{})
}
func (c *Client) Stop(ctx context.Context) (protocol.StopResult, error) {
	var r protocol.StopResult
	err := c.decode(ctx, protocol.Stop, nil, &r)
	return r, err
}
func (c *Client) Status(ctx context.Context) (protocol.ExecutionStatus, error) {
	var r protocol.ExecutionStatus
	err := c.decode(ctx, protocol.Status, nil, &r)
	return r, err
}
func (c *Client) Ping(ctx context.Context) error {
	return c.decode(ctx, protocol.Ping, nil, &struct{}{})
}

// Close stops admission, asks the worker to Stop/Destroy its instance, and waits
// for exit. Its deadline is at most ShutdownTimeout. Any timeout/cancellation
// kills the process, reaps it and reports unknown. Concurrent calls share cleanup.
func (c *Client) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("worker: close requires a context")
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		c.mu.Unlock()
		go func() {
			deadline, stop := context.WithTimeout(ctx, c.config.ShutdownTimeout)
			defer stop()
			raw, err := c.request(deadline, protocol.Close, nil, true)
			if err == nil {
				err = protocol.Decode(raw, &struct{}{})
			}
			if err != nil {
				c.abort(&Failure{Code: "shutdown_failed", Unknown: true, Cause: err})
				return
			}
			select {
			case <-c.done:
			case <-deadline.Done():
				c.abort(&Failure{Code: "shutdown_timeout", Unknown: true, Cause: deadline.Err()})
			}
		}()
	})
	select {
	case <-c.done:
	case <-ctx.Done():
		c.abort(&Failure{Code: "shutdown_timeout", Unknown: true, Cause: ctx.Err()})
		<-c.done
	}
	return c.Err()
}

const diagnosticBytes = 64 << 10

type diagnosticTail struct {
	mu    sync.Mutex
	bytes []byte
}

func (d *diagnosticTail) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(p)
	if n >= diagnosticBytes {
		d.bytes = append(d.bytes[:0], p[n-diagnosticBytes:]...)
		return n, nil
	}
	if extra := len(d.bytes) + n - diagnosticBytes; extra > 0 {
		copy(d.bytes, d.bytes[extra:])
		d.bytes = d.bytes[:len(d.bytes)-extra]
	}
	d.bytes = append(d.bytes, p...)
	return n, nil
}
func (d *diagnosticTail) String() string { d.mu.Lock(); defer d.mu.Unlock(); return string(d.bytes) }
