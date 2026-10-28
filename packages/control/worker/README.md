# Worker supervisor

`worker.Client` owns one child process, its versioned JSONL channel, stderr tail,
and cleanup. It depends only on Go and the [protocol package](protocol/protocol.go);
only [core-worker](../../../apps/core-worker/README.md) imports the native binding.

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

Use the typed Initialize/Connect/AppendTask/SetTaskParams/Start/Stop/Status/Ping
methods. Calls are concurrent-safe; a single writer preserves request ID order,
and responses can arrive in any order. Connect returns acceptance and later emits
an `operation` event. Task completion must be interpreted from native callbacks;
the supervisor does not infer success from status or acknowledgments.

`Events()` is a bounded single-consumer stream. Unknown native messages stay in
their raw JSON. Observe it alongside `Done()`/`Err()`; do not wait only for a task
callback when the process may have failed. After Done, Events is closed and any
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
