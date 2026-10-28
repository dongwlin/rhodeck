# Worker supervisor

`worker.Client` owns one child process, its versioned JSONL channel, stderr tail,
and cleanup. It depends only on Go and the [protocol package](protocol/protocol.go);
only [core-worker](../../../apps/core-worker/README.md) imports the native binding.
The wire side is frozen protocol v1: the client verifies the version on the
greeting and refuses an incompatible worker before sending any request, so no
device action can cross a version boundary.

```go
client, err := worker.Launch(ctx, worker.Config{
    Executable: "/opt/rhodeck/core-worker",
    RequestTimeout: 30 * time.Second,
    ShutdownTimeout: 5 * time.Second,
})
if err != nil { return err }
defer client.Close(context.Background())

info, err := client.Initialize(ctx, protocol.InitializeRequest{
    SnapshotID: "installed-snapshot",
    LibraryPath: "/opt/maa/libMaaCore.so",
    UserDir: "/var/lib/rhodeck/core",
    ResourceRoots: []string{"/opt/maa"},
    ExpectedCoreVersion: "v6.20.1",
})
// Check err and info before connecting. Consume client.Events() in one goroutine.
```

`Launch` requires an absolute executable path and uses no shell. Zero timeouts
select defaults: startup 10s, requests 30s, shutdown 5s. Negative values fail before
process launch. Its context bounds startup only; `Close` owns the established
session lifetime. `ExpectedWorkerVersion` can enforce an exact build version.

Use the typed Initialize/Connect/Click/Screencap/AttachWindow/AppendTask/
SetTaskParams/Start/Stop/BackToHome/Status/UUID/Ping methods. Calls are
concurrent-safe; a single writer preserves request ID order, and responses can
arrive in any order. The four async submissions return an `*Operation` whose
`Wait` reports the native terminal state (see the frozen client contract below);
task and other screen-action completion must still be interpreted from native
callbacks — the supervisor does not infer success from status or
acknowledgments. `UUID` reads the device UUID from the Core cache without
connecting; the empty string and an empty task list are normal results.

`Initialize` returns the worker's read-only runtime snapshot: the native Core
version, the library SHA256, one digest per available `resource/version.json`,
the optional-symbol capabilities, the effective limit set the worker applies
(the whole default set when the request omitted `limits`), and the native
`null_size` sentinel. The value is a detached copy: mutating it cannot affect
the worker or later operations. Capabilities describe native symbol presence
only — never that a wire command is implemented, a task type is accepted, or an
action will succeed; implemented commands are defined by the frozen protocol
table, and unimplemented ones fail with stable `unsupported`. A snapshot
missing the limit set or null size, or reporting limits above the wire
budgets, is a protocol failure.

### Frozen client contract for the reserved command set

The protocol freezes the v1 command/event tables (see the
[core worker README](../../../apps/core-worker/README.md)); the client grows
the surfaces below together with the worker commands they drive, and never
exposes a command the connected build rejects:

- **Async operations.** Each accepted async submission (`connect`, `click`,
  `screencap`, `attach_window`) yields one operation object carrying the
  request ID, nonzero async ID, and `what`. `Wait(ctx)` is repeatable and
  concurrency-safe: the first terminal observation is cached and immutable —
  a later fault or close never rewrites a confirmed result — and unconfirmed
  operations stay distinguishable as pending, session-closed, or unknown.
  Cancelling a `Wait` ends only that wait: it never cancels the submitted
  native action, kills the worker, or unblocks other waiters. A request that
  was enqueued but whose acceptance is unknown still terminates the session
  under the failure discipline below.
- **Image transfers.** `get_image_png`/`get_image_bgr` return a fully
  assembled, independently owned byte snapshot: the client drives the
  `image_chunk` pulls (at most one outstanding chunk per transfer), verifies
  offsets, lengths, and the declared SHA256, and delivers only a complete,
  verified payload. Chunk traffic never appears in the event stream. The
  configured binding image maxima (default PNG 32 MiB, BGR 64 MiB; explicit
  maxima within the frozen ceilings) are carried by bounded chunking, not by
  larger frames; capacity conflicts are rejected by the worker before any
  transfer starts.
- **Instance observations.** `fault` and `diagnostics` events carry the native
  instance's first fault and bounded diagnostics snapshot (saturating lost
  event count, structured first fault and stop diagnostic, base64 raw prefix
  that may be invalid UTF-8/JSON). The client caches them and exposes them
  separately from its own process-level `Done()`/`Err()`/stderr diagnostics;
  a forced kill is reported as process-level failure with unknown facts, and
  never as a fabricated final instance snapshot.
- **Queries.** `uuid`, `map_level_key`, `item_name`, and `log` return the
  frozen result DTOs; empty strings, zero-valued results, and empty lists are
  normal observations, not errors.

### Cancellation and waiting

Cancelling a request's context before it is enqueued rejects it locally and
leaves the session intact. Cancellation or timeout after enqueueing
terminates the session because execution may already have begun; such results
stay unknown. Waiting is different: waiting for an operation's terminal state
can be cancelled freely and repeatedly without side effects on the worker,
the native action, or other waiters.

`Events()` is a bounded single-consumer stream. The frozen v1 event names are
`core`, `operation`, `fault`, and `diagnostics`; an unknown event name aborts
the session as a protocol error, and a build that does not yet emit the newer
events simply never sends them. Unknown native messages stay in
their raw JSON. Observe it alongside `Done()`/`Err()`; do not wait only for a
task callback when the process may have failed. After Done, Events is closed and any
buffered events remain readable. Close does not wait for the consumer. Diagnostics
returns up to 64 KiB of raw stderr tail, which may contain private connection or
native diagnostic data; callers decide where it can be displayed or stored.

Command/operation rejection is a `*protocol.Error` with a stable Code. Process or
transport failure is a `*worker.Failure`; inspect Code and Unknown with errors.As.
Typical codes include `incompatible_worker`, `startup_timeout`, `request_timeout`,
`request_limit`, `write_failed`, `disconnected`, `protocol_error`, `event_overflow`,
`worker_exited`, `shutdown_failed` and `shutdown_timeout`. Unknown indicates facts
may have been lost; it never means cancellation was confirmed. Local encoding or
outbound capacity errors reject that request before enqueueing. Pre-cancelled
calls do not damage an established session. Cancellation/timeout after enqueueing
terminates the session because execution may already have begun.

`Close(ctx)` prevents new requests, sends close, and waits for the acknowledgment
and process exit within the configured shutdown deadline. It kills and reaps on
timeout, including when an acknowledgment was sent but the process remains alive.
Concurrent/repeated calls share cleanup. `Done()` confirms process waiting and IPC
cleanup, not business task completion. The client never restarts workers or
replays queued work. Explicit recovery and device/data-directory ownership belong
to the application using this package.
