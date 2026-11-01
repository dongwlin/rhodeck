package output

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrTimeout marks a machine output write that exceeded its deadline.
var ErrTimeout = errors.New("CLI output deadline exceeded")

// ErrClosed marks a write to a closed machine output stream.
var ErrClosed = errors.New("CLI output stream is closed")

// Writer is the only writer of the machine output stream. Every machine object
// and the help text pass through one bounded queue, so they can never
// interleave and a blocked stream can never start a second write.
type Writer struct {
	stdout  io.Writer
	encoder *json.Encoder
	jobs    chan writeJob
	abort   chan struct{}
	done    chan struct{}
	once    sync.Once

	mu    sync.Mutex
	grace time.Duration
}

// writeJob is one queued machine object. The writer goroutine performs the
// write and reports the result.
type writeJob struct {
	write  func() error
	result chan error
}

// NewWriter starts the single machine output writer of one process.
func NewWriter(stdout io.Writer) *Writer {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	w := &Writer{
		stdout:  stdout,
		encoder: encoder,
		jobs:    make(chan writeJob, 1),
		abort:   make(chan struct{}),
		done:    make(chan struct{}),
	}
	go w.run()
	return w
}

func (w *Writer) run() {
	defer close(w.done)
	for {
		select {
		case <-w.abort:
			return
		case job := <-w.jobs:
			select {
			case <-w.abort:
				return
			default:
			}
			job.result <- job.write()
		}
	}
}

// SetGrace sets the deadline of every following write. Zero disables the
// deadline.
func (w *Writer) SetGrace(grace time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.grace = grace
}

// Grace returns the current write deadline.
func (w *Writer) Grace() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.grace
}

// Close stops the writer. A write that is already blocked cannot be
// interrupted; it stays behind until the process exits.
func (w *Writer) Close() {
	w.once.Do(func() { close(w.abort) })
}

// Emit writes one machine object. done reports that the producer finished and
// release frees the producer's resources; the writer calls release when the
// write is blocked and one of them arrives.
func (w *Writer) Emit(ctx context.Context, value any, done <-chan struct{}, release func()) error {
	return w.submit(ctx, func() error { return w.encoder.Encode(value) }, done, release)
}

// EmitText writes the human readable help text. It shares the queue with the
// machine objects, so text and machine output can never interleave.
func (w *Writer) EmitText(ctx context.Context, text string) error {
	return w.submit(ctx, func() error {
		_, err := io.WriteString(w.stdout, text)
		return err
	}, nil, nil)
}

func (w *Writer) submit(ctx context.Context, write func() error, done <-chan struct{}, release func()) error {
	grace := w.Grace()
	var timeout <-chan time.Time
	if grace > 0 {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		timeout = timer.C
	}
	result := make(chan error, 1)
	job := writeJob{write: write, result: result}
	select {
	case <-w.abort:
		return ErrClosed
	default:
	}
	select {
	case w.jobs <- job:
	case <-w.abort:
		return ErrClosed
	case <-timeout:
		return ErrTimeout
	}
	cancelled := ctx.Done()
	for {
		select {
		case err := <-result:
			return err
		case <-w.abort:
			return ErrClosed
		case <-cancelled:
			cancelled = nil
			callRelease(release)
		case <-done:
			done = nil
			callRelease(release)
		case <-timeout:
			// A completed write wins a simultaneous deadline observation.
			select {
			case err := <-result:
				return err
			default:
				return ErrTimeout
			}
		}
	}
}

// callRelease runs the producer's release hook once per trigger. The producer
// keeps it idempotent.
func callRelease(release func()) {
	if release != nil {
		release()
	}
}

// Diagnostics writes the one stable diagnostic line. It never echoes input
// values or raw worker diagnostics.
type Diagnostics struct {
	stderr io.Writer
}

// NewDiagnostics returns the diagnostic writer of one process.
func NewDiagnostics(stderr io.Writer) *Diagnostics {
	return &Diagnostics{stderr: stderr}
}

// Write reports one stable diagnostic code.
func (d *Diagnostics) Write(code string) {
	_, _ = io.WriteString(d.stderr, "rhodeck-cli: "+code+"\n")
}
