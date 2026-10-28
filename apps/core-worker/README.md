# Core worker

`core-worker` hosts one MaaCore runtime and instance in a dedicated process. It
loads the native library through [maa-go](../../packages/maa-go/README.md), reuses
the instance across task batches, and exits after close or initialization failure.
The parent uses the [supervisor client](../../packages/control/worker/README.md).

```sh
CGO_ENABLED=0 go build -o /tmp/core-worker ./apps/core-worker
/tmp/core-worker -version
```

The build version defaults to `dev`; release builds can set
`-ldflags '-X main.version=v0.1.0'`. MaaCore, its native dependencies, resources,
and ADB are supplied separately. The worker does no discovery, installation,
updates, automatic reconnection, task replay, or device locking.

## Transport and initialization

stdin/stdout carry protocol v1 JSONL. Each complete frame ends with a newline.
stdout is reserved for protocol; Go and native stdout are redirected to stderr
before MaaCore loads. The supervisor captures a bounded stderr tail. Keep stdin
open until the close response: premature EOF means the parent lost the session.

The first frame is `hello`, containing worker version, random session ID,
OS/architecture and transport budgets. This handshake does not load MaaCore.
Every later request includes `version: 1`, `type: "request"`, a strictly increasing
nonzero uint64 `id`, `name`, and an optional object `payload`. A response echoes
the ID and name with `type: "response"`; a command error has a stable `error.code`
and bounded diagnostic `error.message`. Unexpected fields, duplicate control
keys, bad UTF-8/JSON, incompatible versions, truncated frames and non-increasing
IDs terminate the session. Unknown fields inside raw task/extra JSON are retained.

For example, an initialization request is:

```json
{"version":1,"type":"request","id":1,"name":"initialize","payload":{"snapshot_id":"installed-snapshot","library_path":"/opt/maa/libMaaCore.so","user_dir":"/var/lib/rhodeck/core","resource_roots":["/opt/maa"],"expected_core_version":"v6.20.1"}}
```

Paths must be explicit and absolute. `resource_roots` is ordered; each root
contains `resource/`. Optional `static_options`, `instance_options` and
`connection_extras` use the [protocol DTOs](../../packages/control/worker/protocol/protocol.go).
The worker uses maa-go's default input, callback and operation budgets.

`snapshot_id` is an identifier supplied by the installation owner. Initialization
reports the actual library SHA256, native Core version, optional native symbols,
and each available `resource/version.json` SHA256. Missing overlay version files
have an omitted digest. `expected_library_sha256` is checked before loading;
`expected_core_version` is checked before creating the instance. Resource version
digests identify metadata, not the complete resource tree. The owner must keep the
installation and its dependencies immutable during use. Symbol availability does
not enumerate accepted task types or expand the worker's command set.

## Commands and observations

| Name | Payload | Response meaning |
| --- | --- | --- |
| `initialize` | `InitializeRequest` | Core and instance ready; version and snapshot facts |
| `connect` | `adb_path`, `address`, `config` | Nonzero `async_id`: accepted only |
| `append_task` | string `type`, raw object `params` | Nonzero `task_id`: appended only |
| `set_task_params` | `task_id`, raw object `params` | Parameters accepted |
| `start` | absent or `{}` | Start call accepted |
| `stop` | absent or `{}` | `stopped` is true only after Stop returns and Running is false |
| `status` | absent or `{}` | Native `running`, `connected`, and `tasks` observations |
| `ping` | absent or `{}` | IPC loop can respond, including while a native call is blocked |
| `close` | absent or `{}` | Stop/Destroy returned, events and operations drained; process exits |

State-changing native calls are serialized. `ping` does not enter MaaCore;
responsiveness proves neither native responsiveness nor task completion.
`Running=false`, an appended task ID and an accepted Start are not task success.
`stop.stopped` confirms Core stopped, not rollback of device actions or cancellation
of the native asynchronous queue.

Events have `type: "event"`, no request ID, and a contiguous session `sequence`.
Their `name` identifies the payload:

- `core`: `message_id`, `core_sequence`, and complete raw `json`; unknown native
  messages and task IDs within their JSON are preserved.
- `operation`: originating `request_id`, `async_id`, `what`, `ok`, raw `details`
  and optional stable `error`; this follows the corresponding acceptance response.
  A failed connection is distinct from rejected submission.

Numbers never pass through float64 maps. JSON formatting/escaping may change;
unknown fields and integer values are retained. Callback and operation producers
enqueue owned data; they do not block native callback threads on IPC. Event
sequences and native callback sequences have separate domains. The greeting's
session ID distinguishes fresh workers. Protocol facts are not persisted business
IDs or product task outcomes; their interpretation belongs to the caller.

## Limits, faults, and shutdown

Frames are at most 8 MiB excluding the newline. Each outbound queue permits at
most 64 messages / 16 MiB encoded bytes, plus one frame in the writer. The worker
command queue holds at most 16 requests. The supervisor allows 64 pending requests
and 64 events / 16 MiB retained event data (including bounded accounting overhead).
All overflow paths are explicit errors; lost critical events terminate the session.
Consumers must drain the single event stream promptly, including while waiting
for requests or shutdown.

Remote error codes are `invalid_input`, `invalid_state`, `native_rejected`,
`unsupported`, `load_failed`, `symbol_missing`, `registration_failed`,
`snapshot_mismatch`, `limit_exceeded`, `event_overflow`, `protocol_error` and
`internal_error`. Mapping uses Go error identity/types, not native text parsing.
Any initialization attempt reaching the native factory is terminal on failure;
the caller starts a fresh worker to recover. Payload schema rejections happen
before that attempt. Reinitializing a ready worker is rejected.

EOF, SIGINT/SIGTERM, binding faults, malformed protocol, queue overflow and output
failure stop admission and enter cleanup. After those interruptions the worker
allows five seconds for cleanup, then exits even if a native call is stuck. The
supervisor independently bounds every request and close, kills a stuck worker,
and waits for its process. Missing acknowledgments, abnormal exit, event loss and
forced termination mean unconfirmed execution results are **unknown**. No action
is automatically retried or replayed. Normal close is exit 0; failed sessions are
exit 1; command-line usage errors are exit 2.

## Verification

```sh
CGO_ENABLED=0 go test ./apps/core-worker ./packages/control/worker/... -skip TestRealCoreWorker
go test -race ./apps/core-worker ./packages/control/worker/... -skip TestRealCoreWorker
```

The real-Core test is explicitly enabled with `RHODECK_WORKER_CORE_LIBRARY`,
`RHODECK_WORKER_RESOURCE_ROOT`, `RHODECK_WORKER_LIBRARY_SHA256` and
`RHODECK_WORKER_CORE_VERSION`. Run
`go test ./apps/core-worker -run '^TestRealCoreWorkerNoDevice$' -v -count=1`.
It initializes, appends disabled Award tasks, updates parameters, reads status and
destroys the instance. It never connects or starts tasks. Without explicit inputs
the test skips; a skip is not real-Core verification.

[Worker CI](../../.github/workflows/worker.yml) runs unit/process tests on Linux
and Windows amd64 and both macOS architectures, race tests on Linux amd64, and
cross-compiles Linux/Windows/macOS for amd64/arm64. Real MaaCore checks remain
opt-in. Compilation and fixture tests do not establish native Core support on a
platform; see maa-go's support and deployment contract.
