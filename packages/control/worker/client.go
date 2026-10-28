// Package worker supervises one Core worker process. It never loads native code.
// A failed session is terminal: callers must explicitly create a new worker and
// decide what to do with any unconfirmed device actions.
package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// op is the shared operation state of an accepted async submission. The
	// read loop assigns it before delivering the acceptance response, so the
	// handle exists before any operation event can be dispatched.
	op *operationState
}

// maxTrackedOperations bounds the operations the client tracks between
// acceptance and settlement. It mirrors the binding's default pending-operation
// budget; the worker rejects submissions beyond its own limit first.
const maxTrackedOperations = 128

// operationState is the shared terminal-state record of one accepted async
// operation. The read loop creates it on acceptance, settles it exactly once
// from the matching operation event, and drops it from its tracking map; waiters
// reach it only through their Operation handle.
type operationState struct {
	requestID uint64
	id        int32
	what      string
	done      chan struct{}
	mu        sync.Mutex
	settled   bool
	result    protocol.OperationResult
}

// settle records the first terminal observation. Later calls never rewrite it:
// a confirmed result survives every subsequent fault or close.
func (s *operationState) settle(r protocol.OperationResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return
	}
	s.settled = true
	s.result = r
	close(s.done)
}

// cloneOperationResult returns an independent copy: the details bytes and the
// structured error are cloned so no two callers ever share a mutable buffer.
func cloneOperationResult(r protocol.OperationResult) protocol.OperationResult {
	r.Details = bytes.Clone(r.Details)
	if r.Error != nil {
		e := *r.Error
		if e.Detail != nil {
			d := *e.Detail
			e.Detail = &d
		}
		r.Error = &e
	}
	return r
}

// Operation is the handle for one accepted async submission (connect, click,
// screencap, attach_window). The nonzero ID only proves the worker accepted the
// submission; the terminal state arrives as an operation event that the client
// dispatches internally — waiting never depends on draining Events. Wait is
// repeatable and concurrency-safe, and every call returns an independent copy.
// Use the pointer returned by the submission method; do not copy it.
type Operation struct {
	state  *operationState
	client *Client
}

// ID returns the accepted submission's nonzero async ID. It is stable and stays
// readable after settlement; it is not proof that the action succeeded.
func (o *Operation) ID() int32 { return o.state.id }

// Wait returns the operation's terminal state. Semantics:
//
//   - Settled with ok=true: (result, nil).
//   - Settled with ok=false: (result, *protocol.Error) — the stable Code
//     distinguishes a native rejection from a lifecycle settlement (close or
//     fault); the cached result is never rewritten by a later failure.
//   - ctx cancelled or expired first: (zero, ctx.Err()). This ends only this
//     wait: it never cancels the submitted native action, kills the worker, or
//     affects other waiters, and the operation can be waited on again.
//   - The session terminated before any result arrived: (zero, terminal
//     failure) — a *Failure with Unknown=true after a fault, or ErrClosed after
//     a clean close. An unconfirmed result stays unknown; it is never invented.
func (o *Operation) Wait(ctx context.Context) (protocol.OperationResult, error) {
	if ctx == nil {
		return protocol.OperationResult{}, errors.New("worker: operation wait requires a context")
	}
	for {
		o.state.mu.Lock()
		settled := o.state.settled
		var result protocol.OperationResult
		if settled {
			result = cloneOperationResult(o.state.result)
		}
		o.state.mu.Unlock()
		if settled {
			if result.Error != nil {
				return result, result.Error
			}
			return result, nil
		}
		select {
		case <-o.state.done:
			// The terminal state is published; loop to read it.
		case <-ctx.Done():
			// The terminal state may have been published in the same instant;
			// a published result wins over this wait's cancellation.
			o.state.mu.Lock()
			settled := o.state.settled
			o.state.mu.Unlock()
			if settled {
				continue
			}
			return protocol.OperationResult{}, ctx.Err()
		case <-o.client.ctx.Done():
			o.state.mu.Lock()
			settled := o.state.settled
			o.state.mu.Unlock()
			if settled {
				continue
			}
			return protocol.OperationResult{}, context.Cause(o.client.ctx)
		}
	}
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
	pending           map[uint64]*pendingRequest
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
	c := &Client{ctx: lifetime, cancel: cancel, stdin: stdin, stdout: stdout, config: cfg, out: protocol.NewQueue(), pending: make(map[uint64]*pendingRequest), hello: make(chan protocol.Greeting, 1), events: make(chan protocol.Message, protocol.QueueMessages), readDone: make(chan struct{}), writeDone: make(chan struct{}), processDone: make(chan struct{}), done: make(chan struct{})}
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
	operations := make(map[int32]*operationState)
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
			if isAsyncOperationCommand(m.Name) && m.Error == nil {
				var accepted protocol.OperationAccepted
				if protocol.Decode(m.Payload, &accepted) != nil || accepted.AsyncID == 0 || len(operations) >= maxTrackedOperations || operations[accepted.AsyncID] != nil {
					c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("invalid operation acceptance")})
					return
				}
				// The shared state is assigned before the response is delivered:
				// the submitting goroutine receives a complete handle, and the
				// dispatch below can never race its creation.
				state := &operationState{requestID: m.ID, id: accepted.AsyncID, what: expectedWhat(m.Name), done: make(chan struct{})}
				operations[accepted.AsyncID] = state
				pending.op = state
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
				if protocol.Decode(m.Payload, &result) != nil || result.RequestID == 0 || result.OK != (result.Error == nil) {
					c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("unmatched operation result")})
					return
				}
				state := operations[result.AsyncID]
				// A completion carries its submission's what; a lifecycle
				// settlement (close or fault) carries no what at all. A wrong
				// ID/what or a duplicate result is a protocol failure.
				if state == nil || state.requestID != result.RequestID || (result.What != "" && result.What != state.what) {
					c.abort(&Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("unmatched operation result")})
					return
				}
				delete(operations, result.AsyncID)
				state.settle(result)
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

// isAsyncOperationCommand reports whether a response name is one of the four
// async submissions whose acceptance creates a waitable operation.
func isAsyncOperationCommand(name string) bool {
	switch name {
	case protocol.Connect, protocol.Click, protocol.Screencap, protocol.AttachWindow:
		return true
	}
	return false
}

// expectedWhat returns the native completion what the frozen Core reports for
// an accepted async submission command.
func expectedWhat(name string) string {
	switch name {
	case protocol.Click:
		return "Click"
	case protocol.Screencap:
		return "Screencap"
	case protocol.AttachWindow:
		return "AttachWindow"
	default:
		return "Connect"
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
	case protocol.Connect, protocol.Click, protocol.Screencap, protocol.AttachWindow:
		target = &protocol.OperationAccepted{}
	case protocol.AppendTask:
		target = &protocol.TaskAccepted{}
	case protocol.Status:
		target = &protocol.ExecutionStatus{}
	case protocol.Stop:
		target = &protocol.StopResult{}
	case protocol.UUID:
		target = &protocol.UUIDResult{}
	case protocol.MapLevelKey:
		target = &protocol.MapLevelKeyResult{}
	case protocol.ItemName:
		target = &protocol.ItemNameResult{}
	case protocol.GetImagePNG, protocol.GetImageBGR:
		target = &protocol.ImageTransfer{}
	case protocol.ImageChunk:
		target = &protocol.ImageChunkResponse{}
	}
	if err := protocol.Decode(m.Payload, target); err != nil {
		return err
	}
	switch r := target.(type) {
	case *protocol.RuntimeInfo:
		// The frozen initialize contract always reports the applied limit set
		// and the native null_size sentinel; an omitted set or a set above the
		// wire budgets means the peer cannot deliver what it accepted.
		if r.CoreVersion == "" || r.SnapshotID == "" || len(r.LibrarySHA256) != 64 || len(r.Resources) == 0 || r.NullSize == 0 || r.Limits == nil {
			return errors.New("worker: incomplete runtime information")
		}
		if err := protocol.CheckWireLimits(r.Limits); err != nil {
			return err
		}
	case *protocol.TaskAccepted:
		if r.TaskID == 0 {
			return errors.New("worker: zero task ID")
		}
	case *protocol.ImageTransfer:
		// The transfer description bounds every later allocation and each
		// chunk request, so it is verified before it can reach a caller.
		if err := r.Validate(); err != nil {
			return err
		}
	case *protocol.ImageChunkResponse:
		if r.TransferID == 0 {
			return errors.New("worker: zero transfer ID")
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

// openRequest validates session admission, reserves the next request ID, and
// enqueues the frame. The returned entry owns the response channel; the read
// loop matches responses to it by ID and name.
func (c *Client) openRequest(name string, input any, closing bool) (*pendingRequest, error) {
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
	pending := &pendingRequest{name: name, response: make(chan protocol.Message, 1)}
	c.pending[id] = pending
	if _, err := c.out.Send(m); err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	return pending, nil
}

// awaitResponse waits for one response. A timeout or cancellation while the
// request might already have reached the worker terminates the session and
// reports unknown under the frozen failure discipline; the response, if it
// arrives in the same instant, still wins.
func (c *Client) awaitResponse(ctx context.Context, pending *pendingRequest) (protocol.Message, error) {
	var response protocol.Message
	select {
	case response = <-pending.response:
	case <-ctx.Done():
		select {
		case response = <-pending.response:
		default:
			err := &Failure{Code: "request_timeout", Unknown: true, Cause: ctx.Err()}
			c.abort(err)
			return protocol.Message{}, err
		}
	case <-c.ctx.Done():
		select {
		case response = <-pending.response:
		default:
			return protocol.Message{}, context.Cause(c.ctx)
		}
	}
	return response, nil
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
	pending, err := c.openRequest(name, input, closing)
	if err != nil {
		return nil, err
	}
	response, err := c.awaitResponse(ctx, pending)
	if err != nil {
		return nil, err
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

// submitOperation sends one async submission command and returns the handle to
// its waitable terminal state. The read loop attaches the shared state before
// the acceptance response is delivered, so the handle always exists before any
// operation event can be dispatched. A rejection returns the worker's
// *protocol.Error and no operation.
func (c *Client) submitOperation(ctx context.Context, name string, input any) (*Operation, error) {
	if ctx == nil {
		return nil, errors.New("worker: request requires a context")
	}
	ctx, stop := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pending, err := c.openRequest(name, input, false)
	if err != nil {
		return nil, err
	}
	response, err := c.awaitResponse(ctx, pending)
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, response.Error
	}
	if pending.op == nil {
		// The read loop aborts the session on an acceptance it cannot pair with
		// a state; reaching this means that guarantee was violated.
		failure := &Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("acceptance without an operation state")}
		c.abort(failure)
		return nil, failure
	}
	return &Operation{state: pending.op, client: c}, nil
}

// Connect asynchronously submits a device connection. A returned *Operation
// proves only that the worker accepted the submission; Wait reports the
// terminal state from the native AsyncCallInfo event. A submission rejection is
// a *protocol.Error.
func (c *Client) Connect(ctx context.Context, req protocol.ConnectRequest) (*Operation, error) {
	return c.submitOperation(ctx, protocol.Connect, req)
}

// Click asynchronously submits a click at device coordinates. The *Operation
// semantics are Connect's; the nonzero ID is not proof the click completed.
func (c *Client) Click(ctx context.Context, req protocol.ClickRequest) (*Operation, error) {
	return c.submitOperation(ctx, protocol.Click, req)
}

// Screencap asynchronously submits a screen capture. Wait only confirms the
// capture request's native outcome; reading the cached image afterwards is a
// separate image transfer.
func (c *Client) Screencap(ctx context.Context) (*Operation, error) {
	return c.submitOperation(ctx, protocol.Screencap, nil)
}

// AttachWindow asynchronously submits the Win32 window binding. The handle and
// method masks travel as unsigned 64-bit integers without loss; a missing
// native symbol or platform support arrives as the worker's stable
// unsupported rejection, never as a fallback to another transport.
func (c *Client) AttachWindow(ctx context.Context, req protocol.AttachWindowRequest) (*Operation, error) {
	return c.submitOperation(ctx, protocol.AttachWindow, req)
}

// GetImagePNG transfers the Core's cached PNG encoding and returns one fully
// assembled, independently owned byte snapshot. The read never triggers a
// screencap: a fresh image requires waiting for a Screencap operation's real
// success first. The empty image is a normal result (an empty, non-nil slice).
func (c *Client) GetImagePNG(ctx context.Context) ([]byte, error) {
	return c.fetchImage(ctx, protocol.GetImagePNG)
}

// GetImageBGR transfers the Core's cached BGR pixels. Size, stride and
// channel order are decided by Core: the snapshot stays opaque bytes. A
// missing optional symbol arrives as the worker's stable unsupported
// rejection; buffer exhaustion as limit_exceeded — the two stay
// distinguishable.
func (c *Client) GetImageBGR(ctx context.Context) ([]byte, error) {
	return c.fetchImage(ctx, protocol.GetImageBGR)
}

// fetchImage runs one bounded image transfer. The acceptance describes an
// immutable snapshot (total length, whole-payload SHA256, chunk length); the
// chunks are pulled with at most one outstanding request per transfer so
// chunk traffic cannot flood the outbound queue. Every chunk is checked
// against its request (transfer ownership, offset, decoded length) before it
// is placed, and the payload is delivered only after the assembled bytes
// match the declared SHA256 — a truncated, duplicated, out-of-range,
// interrupted or mismatched transfer fails instead of publishing a partial
// success. Each request carries the supervisor's request deadline; the
// caller's context bounds the whole transfer, and a cancellation while a
// chunk request might already have executed terminates the session under the
// frozen failure discipline.
func (c *Client) fetchImage(ctx context.Context, name string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("worker: request requires a context")
	}
	raw, err := c.request(ctx, name, nil, false)
	if err != nil {
		return nil, err
	}
	var transfer protocol.ImageTransfer
	if err := protocol.Decode(raw, &transfer); err != nil {
		failure := &Failure{Code: "protocol_error", Unknown: true, Cause: err}
		c.abort(failure)
		return nil, failure
	}
	if transfer.TotalBytes > protocol.ImageTransferMaxBytes {
		// The read loop rejects this contract breach before the response is
		// delivered; the check keeps the assembly allocation bounded even if
		// that upstream guarantee were ever relaxed.
		failure := &Failure{Code: "protocol_error", Unknown: true, Cause: fmt.Errorf("worker: total_bytes %d exceeds the transfer budget of %d bytes", transfer.TotalBytes, uint64(protocol.ImageTransferMaxBytes))}
		c.abort(failure)
		return nil, failure
	}
	payload := make([]byte, transfer.TotalBytes)
	for offset := uint64(0); offset < transfer.TotalBytes; {
		length := uint64(transfer.ChunkBytes)
		if remaining := transfer.TotalBytes - offset; remaining < length {
			length = remaining
		}
		req := protocol.ImageChunkRequest{TransferID: transfer.TransferID, Offset: offset, Length: uint32(length)}
		raw, err := c.request(ctx, protocol.ImageChunk, req, false)
		if err != nil {
			return nil, err
		}
		var chunk protocol.ImageChunkResponse
		if err := protocol.Decode(raw, &chunk); err != nil {
			failure := &Failure{Code: "protocol_error", Unknown: true, Cause: err}
			c.abort(failure)
			return nil, failure
		}
		data, decodeErr := base64.StdEncoding.DecodeString(chunk.Data)
		if decodeErr != nil || chunk.TransferID != transfer.TransferID || chunk.Offset != offset || uint64(len(data)) != length {
			failure := &Failure{Code: "protocol_error", Unknown: true, Cause: fmt.Errorf("worker: image chunk for offset %d does not match its request", offset)}
			c.abort(failure)
			return nil, failure
		}
		copy(payload[offset:], data)
		offset += length
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != transfer.SHA256 {
		failure := &Failure{Code: "protocol_error", Unknown: true, Cause: errors.New("worker: assembled image does not match the declared sha256")}
		c.abort(failure)
		return nil, failure
	}
	return payload, nil
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

// BackToHome requests the instance to return to the home screen. A nil error
// only means the worker reported the call as accepted; the screen action's
// outcome is observed through Core events, never inferred here.
func (c *Client) BackToHome(ctx context.Context) error {
	return c.decode(ctx, protocol.BackToHome, nil, &struct{}{})
}

// UUID reads the device UUID from the Core cache. The worker never connects
// for this read, and the empty string is a normal result.
func (c *Client) UUID(ctx context.Context) (protocol.UUIDResult, error) {
	var r protocol.UUIDResult
	err := c.decode(ctx, protocol.UUID, nil, &r)
	return r, err
}

// Log feeds the process-level native log. The level and message travel
// verbatim, empty strings included; a nil error only states that the worker
// issued the void native call — it is not a completion fact about anything
// Core did with the text. Input rejections (invalid UTF-8, embedded NUL,
// byte budget) arrive as the stable invalid_input code.
func (c *Client) Log(ctx context.Context, req protocol.LogRequest) error {
	return c.decode(ctx, protocol.Log, req, &struct{}{})
}

// MapLevelKey queries the experiment level mapping. The key passes through
// verbatim, and the zero-valued four-field result means "not found" — a
// normal result, not an error. A missing optional symbol arrives as the
// stable unsupported rejection without any native call.
func (c *Client) MapLevelKey(ctx context.Context, req protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error) {
	var r protocol.MapLevelKeyResult
	err := c.decode(ctx, protocol.MapLevelKey, req, &r)
	return r, err
}

// ItemName queries the experiment item name. The empty result and the native
// "Unknown" are normal results, not errors; a missing optional symbol arrives
// as the stable unsupported rejection without any native call.
func (c *Client) ItemName(ctx context.Context, req protocol.ItemNameRequest) (protocol.ItemNameResult, error) {
	var r protocol.ItemNameResult
	err := c.decode(ctx, protocol.ItemName, req, &r)
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
