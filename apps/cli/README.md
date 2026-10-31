# Rhodeck CLI

`rhodeck-cli` executes one explicit task batch through the shared
[`control.Runtime`](../../packages/control/README.md) and a supervised
[`core-worker`](../core-worker/README.md). It needs no HTTP server, Web app or
desktop host. Only the worker loads MaaCore. The caller supplies the worker,
Core library and its native dependencies, resources, ADB and writable directories.

Build the two executables from the repository root:

```sh
CGO_ENABLED=0 go build -o /tmp/rhodeck-cli ./apps/cli
CGO_ENABLED=0 go build -o /tmp/core-worker ./apps/core-worker
/tmp/rhodeck-cli capabilities
/tmp/rhodeck-cli version
/tmp/rhodeck-cli help
```

The CLI build version defaults to `dev`; release builds can set it with
`-ldflags '-X main.version=VERSION'`. `CGO_ENABLED=0` does not remove the Core
library's native dependency requirements. Windows binaries use `.exe` output
paths; all installation and data paths below must be absolute on the host OS.

## Explicit plan input

`run`, `dry-run` and `doctor` all accept the same complete JSON plan through
`--input FILE` or `--input -` for stdin. There is no installation discovery,
shell expansion or default device selection. For example, create `plan.json`
with paths and a device address chosen by the caller:

```json
{
  "schema_version": 1,
  "host_run_id": "caller-run-42",
  "initialize": {
    "snapshot_id": "caller-installed-snapshot",
    "library_path": "/opt/maa/libMaaCore.so",
    "user_dir": "/var/lib/rhodeck/core",
    "resource_roots": ["/opt/maa"]
  },
  "connection": {
    "adb_path": "/usr/bin/adb",
    "address": "DEVICE_ADDRESS",
    "config": "General"
  },
  "tasks": [
    {
      "id": "fight-1",
      "type": "Fight",
      "params": {"stage": "1-7", "times": 1}
    }
  ]
}
```

`host_run_id` is optional opaque caller correlation. Each task has its own unique
`id`, an open string `type`, and JSON object `params`. Unknown task types, unknown
parameter fields and large integer literals survive the input path. Their
acceptance and meaning are decided by the selected Core and resource snapshot.
There is no dynamic Core API enumerating every supported task type, so
`capabilities` describes the runner input contract and installed platform rather
than claiming all task types work. Input control fields are strict; duplicate
control fields, trailing JSON, malformed objects and oversized input are rejected.
The plan limit is 16 MiB with at most 128 tasks; individual inputs also obey the
selected binding limits and frozen worker protocol budgets.

## Preview and diagnosis

```sh
/tmp/rhodeck-cli dry-run --input plan.json \
  --worker /tmp/core-worker --data-dir /var/lib/rhodeck/control
/tmp/rhodeck-cli doctor --input plan.json \
  --worker /tmp/core-worker --data-dir /var/lib/rhodeck/control
```

`dry-run` validates syntax and budgets and prints the final plan and explicit
input sources. `run --dry-run` is equivalent. It never starts a worker, loads
Core, connects a device, appends tasks, creates directories or acquires locks.
Installation availability, native task semantics and device preconditions remain
explicitly unverified. The preview contains the supplied plan, including any
private parameters.

`doctor` checks explicit files/directories and executable permission bits, then
initializes Core in a supervised worker and closes it. It reports structured
checks by field with stable codes and the Core/library/resource snapshot when
initialization succeeds. An unavailable ADB is reported while Core diagnosis can
still proceed. It never connects, appends or starts a task, runs ADB, downloads
resources or repairs an installation. It can create the private data directory,
acquire its lock, recover interrupted local records and let native initialization
write to the configured Core user directory. A file marked executable is only a
local path check; device connection and actual task support remain unverified.

## Execution, stopping and machine output

```sh
/tmp/rhodeck-cli run --input plan.json \
  --worker /tmp/core-worker --data-dir /var/lib/rhodeck/control \
  --timeout 30m
```

The runtime admits one active run and owns the worker, task association,
observations, durable record and cleanup. Its generated `run_id` is distinct from
caller IDs and Core task IDs. A new run uses a new worker; failed work is never
automatically reconnected, retried or replayed.

All stdout output is schema version 1 JSON, with one object per line during a
run. Diagnostic stderr contains stable error codes and no raw worker log tail.
Run events carry `run_id`, optional `host_run_id`, sequence, time, type and any
known task/Core association. Native callbacks retain message ID and Core sequence;
their raw `details` are included only with `--include-raw-events`. This explicit
local diagnostic opt-in can disclose connection details or other private Core
data. Records always retain the private input plan; callers choose their own
retention and access policy for the data directory.

The final JSON object has `type: "terminal"` and a `result` containing task facts,
the overall `state`, a stable `code` when relevant, and a separate `cleanup`
result. It is emitted only after the run's Done boundary, including persistence
and process cleanup, and the runtime has released its directory lock. A rare
storage-close failure is reported in this final output even when the preceding
durable record could not be updated with that failure. A nonzero Core task ID, successful Start response,
`Running=false`, worker liveness or process exit does not imply business success.
Success requires matching per-task completion facts and the matching batch
completion; a task error cannot be overwritten by `AllTasksCompleted`.

SIGINT and SIGTERM use the same control stop path as application cancellation.
The runtime requests Core stop, waits for a stop observation within the grace
period, then closes or terminates and reaps its worker. Forced process termination
does not prove a device action was cancelled. An uncertain result remains
`unknown`; confirmed task facts remain available separately from cleanup failure.

| Exit code | Meaning |
| --- | --- |
| `0` | Run succeeded and cleanup completed; or a read-only/preview/diagnostic command passed. |
| `2` | Input or environment rejected; a doctor check failed. |
| `3` | Runtime or data directory already owned. |
| `4` | Confirmed task failure with completed cleanup. |
| `5` | Confirmed stop with completed cleanup. |
| `6` | Unknown result, deadline, worker/transport failure, failed cleanup or output failure. |

`--timeout` bounds the requested run or doctor work; zero, the default, has no
overall deadline. Cleanup has its own deadlines. Defaults are
`--startup-timeout 10s`, `--request-timeout 30s`, `--stop-timeout 5s` and
`--shutdown-timeout 5s`; all four must be positive. A deadline can therefore be
followed by bounded stop and worker cleanup before the CLI exits. Output consumers
must continue reading until the terminal record. During `run`, each output write
has a separate `--output-timeout` deadline, default five seconds and required to
be positive. A blocked or broken stdout cancels the run, closes its runtime and
releases directory ownership, then exits `6` without another stdout write. The
terminal record may be undeliverable. Already confirmed task facts remain in the
local run record; a failed output sink is separate from native task and cleanup
facts. At most one output write can remain blocked until the process exits.

## Delivery scope

This is the generic execution foundation. It provides no task presets, scheduler,
HTTP API, installer/updater, cross-runtime device lock, Docker image or La Pluma
Next integration. `OperProgress` and `SwitchTheme` can be expressed by the open
task input path; task-specific validation, result interpretation and authorized
device acceptance remain separate product work. A data directory lock does not
establish mutual exclusion with another directory, external CLI or host.

Run CLI/control regressions from the repository root:

```sh
CGO_ENABLED=0 go test ./apps/cli ./packages/control/... -count=1 -timeout=3m
go test -race ./apps/cli ./packages/control/... -count=1 -timeout=3m
```

The CLI tests use real helper processes without a native library or device and
cover machine output, raw-parameter preservation, rejection, task failure, crash,
confirmed cancellation, SIGINT/SIGTERM on Unix, and doctor command boundaries.
The worker's opt-in real-Core no-device tests remain a separate evidence layer.
