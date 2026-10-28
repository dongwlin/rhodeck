package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const shutdownGrace = 5 * time.Second

// Set with -ldflags '-X main.version=...'. Local builds identify themselves.
var version = "dev"

type session struct {
	ctx      context.Context
	fail     context.CancelCauseFunc
	out      *protocol.Queue
	requests chan protocol.Message
	mu       sync.Mutex // Orders all event producers and their sequence allocation.
	sequence uint64
}

// serve bounds graceful cleanup even when a native call never returns. main
// exits after it returns; a supervisor imposes its own independent deadline.
func serve(ctx context.Context, input io.ReadCloser, output io.Writer, factory engineFactory, grace time.Duration) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer input.Close()
	s := &session{ctx: ctx, fail: cancel, out: protocol.NewQueue(), requests: make(chan protocol.Message, protocol.RequestQueue)}
	go s.write(output)
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	greeting := protocol.Greeting{WorkerVersion: version, SessionID: hex.EncodeToString(id[:]), OS: runtime.GOOS, Arch: runtime.GOARCH, MaxFrameBytes: protocol.MaxFrameBytes, QueueMessages: protocol.QueueMessages, QueueBytes: protocol.QueueBytes}
	m, _ := protocol.NewMessage(protocol.Hello, "", 0, greeting)
	if _, err := s.send(m); err != nil {
		return err
	}
	go s.read(input)
	done := make(chan error, 1)
	go func() { done <- s.commands(factory) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
			return context.Cause(ctx)
		case <-timer.C:
			return fmt.Errorf("worker: shutdown deadline exceeded: %w", context.Cause(ctx))
		}
	}
}

func (s *session) send(m protocol.Message) (<-chan error, error) {
	ack, err := s.out.Send(m)
	if err != nil {
		s.fail(err)
	}
	return ack, err
}

func (s *session) write(w io.Writer) {
	for {
		frame, err := s.out.Next(s.ctx)
		if err != nil {
			return
		}
		err = protocol.WriteFrame(w, frame.Data)
		frame.Written <- err
		if err != nil {
			s.fail(fmt.Errorf("worker: write protocol: %w", err))
			return
		}
	}
}

func (s *session) respond(req protocol.Message, payload any, cause error, flush bool) error {
	m, err := protocol.NewMessage(protocol.Response, req.Name, req.ID, payload)
	if err != nil {
		s.fail(err)
		return err
	}
	m.Error = classify(cause)
	ack, err := s.send(m)
	if err != nil {
		return err
	}
	if flush {
		select {
		case err = <-ack:
			return err
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		}
	}
	return nil
}

func (s *session) event(name string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sequence == math.MaxUint64 {
		s.fail(errors.New("worker: event sequence exhausted"))
		return context.Cause(s.ctx)
	}
	s.sequence++
	m, err := protocol.NewMessage(protocol.Event, name, 0, payload)
	if err != nil {
		s.fail(err)
		return err
	}
	m.Sequence = s.sequence
	_, err = s.send(m)
	return err
}

func (s *session) read(r io.Reader) {
	reader := protocol.NewReader(r)
	var previous uint64
	for {
		req, err := reader.Read()
		if err != nil {
			s.fail(fmt.Errorf("worker: read protocol: %w", err))
			return
		}
		if req.Type != protocol.Request || req.ID <= previous {
			s.fail(errors.New("worker: expected strictly increasing request IDs"))
			return
		}
		previous = req.ID
		if req.Name == protocol.Ping {
			if err := emptyPayload(req.Payload); err != nil {
				_ = s.respond(req, nil, err, false)
			} else {
				_ = s.respond(req, struct{}{}, nil, false)
			}
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case s.requests <- req:
		default:
			s.fail(protocol.ErrQueueFull)
			return
		}
	}
}

func payload(req protocol.Message, target any) error {
	if len(req.Payload) == 0 {
		return wireError(protocol.InvalidInput, "payload is required")
	}
	if err := protocol.Decode(req.Payload, target); err != nil {
		return wireError(protocol.InvalidInput, fmt.Sprintf("invalid %s payload: %v", req.Name, err))
	}
	return nil
}

func emptyPayload(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if err := protocol.Decode(raw, &struct{}{}); err != nil {
		return wireError(protocol.InvalidInput, "expected an empty payload")
	}
	return nil
}

func (s *session) commands(factory engineFactory) (result error) {
	var core engine
	var observed <-chan struct{}
	var operations sync.WaitGroup
	cleanup := func() error {
		if core == nil {
			return nil
		}
		err := core.Close()
		<-observed
		operations.Wait()
		core = nil
		return err
	}
	defer func() { result = errors.Join(result, cleanup()) }()
	for {
		select {
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		case req := <-s.requests:
			if s.ctx.Err() != nil {
				return context.Cause(s.ctx)
			}
			if req.Name == protocol.Initialize {
				if core != nil {
					_ = s.respond(req, nil, wireError(protocol.InvalidState, "worker is already initialized"), false)
					continue
				}
				var config protocol.InitializeRequest
				err := payload(req, &config)
				if err != nil {
					_ = s.respond(req, nil, err, false)
					continue
				}
				var info protocol.RuntimeInfo
				core, info, err = factory(config)
				if err != nil {
					_ = s.respond(req, nil, err, true)
					return err
				}
				done := make(chan struct{})
				observed = done
				go func(e engine) {
					defer close(done)
					if err := e.Observe(s.ctx, func(c protocol.Callback) error { return s.event(protocol.CoreEvent, c) }); err != nil {
						s.fail(err)
					}
				}(core)
				_ = s.respond(req, info, nil, false)
				continue
			}
			if req.Name == protocol.Close {
				if err := emptyPayload(req.Payload); err != nil {
					_ = s.respond(req, nil, err, false)
					continue
				}
				err := cleanup()
				if writeErr := s.respond(req, struct{}{}, err, true); writeErr != nil {
					return writeErr
				}
				return err
			}
			if core == nil {
				_ = s.respond(req, nil, wireError(protocol.InvalidState, "worker has not been initialized"), false)
				continue
			}
			var out any = struct{}{}
			var err error
			var op operation
			switch req.Name {
			case protocol.Connect:
				var cfg protocol.ConnectRequest
				if err = payload(req, &cfg); err == nil {
					op, err = core.Connect(cfg)
				}
				if err == nil {
					out = protocol.OperationAccepted{AsyncID: op.ID()}
				}
			case protocol.AppendTask:
				var task protocol.TaskRequest
				if err = payload(req, &task); err == nil {
					var id int32
					id, err = core.AppendTask(task)
					out = protocol.TaskAccepted{TaskID: id}
				}
			case protocol.SetTaskParams:
				var task protocol.TaskParamsRequest
				if err = payload(req, &task); err == nil {
					err = core.SetTaskParams(task)
				}
			case protocol.Start:
				if err = emptyPayload(req.Payload); err == nil {
					err = core.Start()
				}
			case protocol.Stop:
				if err = emptyPayload(req.Payload); err == nil {
					var stopped bool
					stopped, err = core.Stop()
					out = protocol.StopResult{Stopped: stopped}
				}
			case protocol.Status:
				if err = emptyPayload(req.Payload); err == nil {
					out, err = core.Status()
				}
			default:
				err = wireError(protocol.Unsupported, "unknown worker command")
			}
			_ = s.respond(req, out, err, false)
			if op != nil && err == nil {
				operations.Add(1)
				go func(requestID uint64, op operation) {
					defer operations.Done()
					r, err := op.Wait(s.ctx)
					r.RequestID, r.Error = requestID, classify(err)
					_ = s.event(protocol.OperationEvent, r)
				}(req.ID, op)
			}
		}
	}
}
