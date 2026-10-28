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

stdin/stdout carry protocol v1 JSONL. Protocol v1 is frozen as a whole: the
command/event tables, DTO shapes, numeric encoding, error codes, and transport
budgets are the contract, and the [protocol package](../../packages/control/worker/protocol/protocol.go)
is its single definition. Decoding is strict on control fields, so any later
change a v1 peer could not parse — added, removed, renamed, or retyped fields,
commands, events, or codes, or framing/sequence changes — requires a protocol
version bump; it cannot hide behind optional fields. Peers verify the version
on the first frame and refuse incompatible ones before any request, and
therefore before any device action.

Each complete frame ends with a newline. stdout is reserved for protocol; Go
and native stdout are redirected to stderr before MaaCore loads. The
supervisor captures a bounded stderr tail. Keep stdin open until the close
response: premature EOF means the parent lost the session.

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

`limits` is the full explicit [binding limit configuration](../../packages/control/worker/protocol/contract.go)
(`Limits`); omitting it selects the binding's whole default set, and a present
`limits` is one complete configuration — fields are never individually
backfilled from defaults, and an explicit zero capacity is rejected, never
silently refilled. Configurations whose single-unit maxima exceed the
wire budgets (`Limits.InputJSONBytes` > 4 MiB, `ConfigBytes` > 4 MiB,
`CallbackBytes` > 4 MiB, `Tasks.Max` > 65536 elements, `ImagePNG.Max`/`ImageBGR.Max`
> 128 MiB) are rejected with `limit_exceeded` and the effective maximum before
the native library is touched, so no accepted configuration can produce a
result the transport cannot deliver. The binding's own range and cross-field
validation runs inside the factory; a configuration it rejects terminates the
session like any other failed initialization.

The initialize response reports the effective `limits` — exactly the applied
configuration, materialized from the whole default set when omitted — and the
native `null_size` capacity sentinel (`math.MaxUint64` for the locked profile)
alongside the facts below.

`snapshot_id` is an identifier supplied by the installation owner. Initialization
reports the actual library SHA256, native Core version, optional native symbols,
and each available `resource/version.json` SHA256. Missing overlay version files
have an omitted digest. `expected_library_sha256` is checked before loading;
`expected_core_version` is checked before creating the instance. Resource version
digests identify metadata, not the complete resource tree. The owner must keep the
installation and its dependencies immutable during use. Symbol availability does
not enumerate accepted task types or expand the worker's command set.

## Commands and observations

The v1 command table below is frozen. A build answers every name in the
table; commands it does not implement yet are rejected with the stable
`unsupported` code and the session survives. Capabilities reported by
initialize describe native symbol availability only — never that a command is
implemented, a task type is accepted, or an action will succeed.

| Name | Payload | Response meaning | Status |
| --- | --- | --- | --- |
| `initialize` | `InitializeRequest` | Core and instance ready; version, snapshot facts, effective limits, `null_size` | implemented |
| `connect` | `adb_path`, `address`, `config` | Nonzero `async_id`: accepted only | implemented |
| `append_task` | string `type`, raw object `params` | Nonzero `task_id`: appended only | implemented |
| `set_task_params` | `task_id`, raw object `params` | Parameters accepted | implemented |
| `start` | absent or `{}` | Start call accepted | implemented |
| `stop` | absent or `{}` | `stopped` is true only after Stop returns and Running is false | implemented |
| `status` | absent or `{}` | Native `running`, `connected`, and `tasks` observations | implemented |
| `ping` | absent or `{}` | IPC loop can respond, including while a native call is blocked | implemented |
| `close` | absent or `{}` | Stop/Destroy returned, events and operations drained; process exits | implemented |
| `back_to_home` | absent or `{}` | Call accepted, or `native_rejected` when Core refuses it; acceptance carries no completion fact | implemented |
| `uuid` | absent or `{}` | `uuid` string read from the Core buffer; the empty string is a normal result and the read never connects | implemented |
| `click` | `x`, `y` int32 | Nonzero `async_id`: accepted only | implemented |
| `screencap` | absent or `{}` | Nonzero `async_id`: accepted only | implemented |
| `attach_window` | `hwnd` uint64, `screencap_method`, `mouse_method`, `keyboard_method` uint64 masks | Nonzero `async_id`: accepted only; a handle beyond the platform pointer width is `invalid_input`, a missing symbol or platform is `unsupported` — never an ADB fallback | implemented |
| `get_image_png` / `get_image_bgr` | absent or `{}` | `ImageTransfer` describing one owned snapshot of the cached image | implemented |
| `image_chunk` | `transfer_id`, `offset`, `length` | `ImageChunkResponse` with exactly the requested bytes | implemented |
| `log` | `level`, `message` | Call accepted; void return carries no completion fact | reserved |
| `map_level_key` | `key` | Four-field result; zero values mean "not found", not an error | reserved |
| `item_name` | `id` | `name`; the empty string means "not found", not an error | reserved |

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
- `fault`: the first instance fault as a structured `error` with its stable
  code. This is an independent observation: it is delivered through the same
  ordered sequence but never depends on the consumer draining other events —
  if the outbound channel cannot deliver it, the session fails and the
  supervisor reports the loss, so a fault is either delivered or loudly lost.
- `diagnostics`: the bounded instance diagnostics snapshot, sent once the
  instance reaches its terminal state: saturating `lost_events`, optional
  `first_fault` and `stop_diagnostic` as structured errors, and `raw_prefix`
  as the standard base64 encoding of up to 4096 raw bytes that may be neither
  valid UTF-8 nor valid JSON. A normal close omits all three optional fields.
  The client must represent these native instance observations separately
  from its own process-level Done/Err/stderr facts.

### Numbers, bytes, and empty values

Control-plane numbers are typed integers decoded directly from JSON integer
literals; nothing in the contract path passes through float64. `uint64`
fields (request/event sequences, masks, `hwnd`, byte counts, saturating
counts) survive `math.MaxUint64` exactly; values beyond that range, non-
integer or exponent forms are rejected, never wrapped. Window handles travel
as unsigned 64-bit integers and are converted to platform pointer width by
the worker after validation. Win32 screencap/input masks are uint64 bit
fields passed through verbatim.

Raw task params, connection extras, Core callback JSON, and operation details
stay `json.RawMessage`: their nested schema — including unknown fields,
duplicate keys, and numbers of any magnitude — is preserved byte for byte and
is deliberately not validated by the protocol.

`false`, empty strings, zero-length cached images, empty task lists, and
zero-valued query results are normal results and never errors. A 0-length
cached image is a complete transfer with the SHA256 of the empty payload and
needs no chunks.

### Image transfers

`get_image_png`/`get_image_bgr` read only the Core cache; they never trigger
a screencap. The worker rejects a request beyond the concurrency budget
before reading any byte, takes one owned snapshot, and answers with
`ImageTransfer`: nonzero `transfer_id`, the snapshot's `total_bytes`, its
SHA256 (64 lowercase hex digits), and `chunk_bytes` (at most 4 MiB). The
snapshot beyond the per-transfer byte budget is the last admission gate —
the applied limit set was already wire-checked at initialize, so a snapshot
it could not deliver is rejected instead of retained. The client then
fetches chunks with `image_chunk` requests — `offset`, `length` within
`[1, chunk_bytes]` and `offset+length ≤ total_bytes`, in any order — and
receives exactly the requested bytes as standard base64. The supervisor
assembles the payload, verifies coverage and the declared SHA256, and only
then delivers it; chunks never enter the event stream or its sequence
domain.

Transfers are bounded: at most 2 concurrently retained, each at most 128 MiB
(the largest legal binding image maximum), so the aggregate in-flight image
memory is at most 256 MiB on each side of the pipe. An unknown or exhausted
transfer id — including one released after its last byte was served — is
`invalid_input`. A zero-length image is a complete transfer at acceptance:
it consumes a transfer ID but retains no snapshot and occupies no
concurrency slot. The worker releases a snapshot when its last byte is
served, on any session failure, and at close; the supervisor's request
deadline bounds every chunk fetch, and its pull keeps at most one
outstanding chunk request per transfer, so chunk traffic cannot flood the
outbound queue.

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
`internal_error`. The code is the only control-plane discriminator; mapping
uses Go error identity/types, not native text parsing. An optional bounded
`detail` object qualifies capacity rejections with machine-readable fields —
reason `kind`, the limited buffer/interface, its unit, the limits field path,
probe attempts, the observed capacity, and the effective wire maximum — so a
merged code such as `limit_exceeded` still distinguishes native buffer
exhaustion from operation-count limits and transport-budget rejections
without parsing messages.

Any initialization attempt reaching the native factory is terminal on failure;
the caller starts a fresh worker to recover. Payload schema rejections —
including explicit limits above the wire budgets — happen before that
attempt. Reinitializing a ready worker is rejected.

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
It initializes, confirms native rejection of an unknown task, appends disabled
Award tasks, updates parameters, reads status and
destroys the instance. It never connects or starts tasks. Without explicit inputs
the test skips; a skip is not real-Core verification.

[Worker CI](../../.github/workflows/worker.yml) runs unit/process tests on Linux
and Windows amd64 and both macOS architectures, race tests on Linux amd64, and
cross-compiles Linux/Windows/macOS for amd64/arm64. Real MaaCore checks remain
opt-in. Compilation and fixture tests do not establish native Core support on a
platform; see maa-go's support and deployment contract.
