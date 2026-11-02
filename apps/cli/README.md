# Rhodeck CLI

`rhodeck-cli` runs an explicit MaaCore task batch through a supervised
[worker](../worker/README.md). The caller supplies MaaCore and its native
dependencies, resources, ADB, a device address and writable directories.

## Quick start

Build both executables from the repository root:

```sh
CGO_ENABLED=0 go build -o /tmp/rhodeck-cli ./apps/cli
CGO_ENABLED=0 go build -o /tmp/worker ./apps/worker
```

The examples use Linux paths; use host-appropriate absolute paths and `.exe`
executables on Windows. `CGO_ENABLED=0` does not remove MaaCore's native dependencies.

Save the following as `plan.json`, replacing the installation paths and device
address. Each resource root is the directory containing `resource/`.

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

Preview the plan, diagnose the installation, then execute:

```sh
/tmp/rhodeck-cli dry-run --input plan.json \
  --worker /tmp/worker --data-dir /var/lib/rhodeck/control
/tmp/rhodeck-cli doctor --input plan.json \
  --worker /tmp/worker --data-dir /var/lib/rhodeck/control
/tmp/rhodeck-cli run --input plan.json \
  --worker /tmp/worker --data-dir /var/lib/rhodeck/control \
  --timeout 30m
```

All three commands require `--input`, `--worker` and `--data-dir`, including
`dry-run`. Use `--input -` to read the complete plan from stdin. Worker, data,
Core, resource, user-directory and ADB paths must be absolute. Installation and
device selection are explicit; the CLI performs no discovery or downloads.

`dry-run` validates and compiles the plan, then reports the compiled parameters,
per-task objective policies, initial targets, input sources and unverified
conditions. It creates no directories, takes no locks and starts no worker or
device actions; it does not check whether the supplied installation exists.
`run --dry-run` is equivalent.

`doctor` checks local paths and executable permissions, initializes Core in a
worker and closes it. It can create and lock the data directory, recover
interrupted records and write to the Core user directory. It does not prepare
adapters, run ADB, connect a device or submit tasks. A passing diagnosis does not
establish device readiness or task support.

## Commands

| Command | Purpose |
| --- | --- |
| `run` | Execute one task batch; also accepts `--dry-run`. |
| `dry-run` | Validate and preview a plan without side effects. |
| `doctor` | Check an explicit installation and initialize Core. |
| `capabilities` | Report the platform, worker protocol, adapter inputs, policies and result interpretation. |
| `version` | Print the CLI build version. |
| `help [command]` | Show usage and flags, for example `help run`. |

Help is text by default. Machine callers can use `help run --format json` to
receive `{"schema_version":1,"type":"help","usage":"..."}`. `--format` accepts
`text` or `json` and affects only help; command results and errors are always JSON.

`capabilities` describes the locally registered adapters. It does not inspect the
installation or enumerate every task supported by Core. Use `doctor` for actual
Core/library/resource metadata and `dry-run` for the policy selected by a task's
input.

## Plan and adapters

Each task requires a unique `id`, a Core task `type` and an object `params`.
`host_run_id` is optional caller correlation; the runtime generates a separate
`run_id`. Raw task types and parameters remain open: unknown fields and large
integer literals are preserved, and the selected Core and resources determine
their meaning and acceptance.

Control fields are strict: unknown or duplicate control fields, trailing JSON,
malformed objects and oversized input are rejected. Plans are limited to 16 MiB
and 128 tasks; binding and worker protocol limits also apply.

An explicit `adapter` enables strict parameter validation and, where supported,
business result interpretation. These adapters are registered at version `1`:

| Adapter name | Core task type |
| --- | --- |
| `oper_progress` | `OperProgress` |
| `switch_theme` | `SwitchTheme` |
| `copilot` | `Copilot` |
| `roguelike` | `Roguelike` |

For example, replace a task in the plan with:

```json
{
  "id": "theme-1",
  "type": "SwitchTheme",
  "adapter": {"name": "switch_theme", "version": 1},
  "params": {"themes": ["Theme One", "Theme Two"]}
}
```

Tasks without `adapter` remain raw, even when their type matches a registered
adapter. `run` and `dry-run` share adapter validation and reject unknown adapter
names, unsupported versions, mismatched task types and invalid parameters with
`invalid_plan` (exit `2`) before creating directories or starting a worker.

Preparation selects each adapted task's `objective_policy`: `all` requires every
target to succeed, `any` requires at least one, and `none` validates input without
publishing a typed business result. `dry-run` shows the policy selected for the
actual input. Full parameter constraints and result rules are in the adapter
reference linked below.

## Output and records

Except for text help, stdout contains schema version 1 JSON, one object per line.
Stderr contains stable diagnostic codes without input values or raw worker logs.
Pre-run rejections use an `error` envelope with a stable `code`; `doctor` reports
check failures in its own envelope.

During execution, read the event stream through the final `type: "terminal"`
object. Its `result` contains task results, overall `state`, a stable `code` when
relevant, and separate `cleanup` status. The terminal is emitted after execution
and cleanup finish and the data-directory lock is released. Worker acceptance,
a task ID or chain completion alone does not establish an interpreted business
objective; use the final result and exit code together.

Business state and cleanup status are independent. Cleanup or persistence failure
produces exit `6` while preserving an established business result. Incomplete
objective evidence can leave a completed batch `unknown` with `typed_incomplete`.
Process termination alone cannot establish that a device action stopped.

Run records are stored at `<data-dir>/runs/<run_id>.json` and retain the original
plan and confirmed task facts. Keep this directory private: plans, previews and
records can contain paths and connection data. `--include-raw-events` additionally
exposes raw Core callback details in run output. Typed events contain filtered
business facts and require no raw-event opt-in.

Each run uses a new worker. Failed or interrupted work is never automatically
reconnected, retried or replayed. The data-directory lock excludes other runtimes
using that directory; callers must serialize access to the same device across
different directories or external tools.

## Stopping and timeouts

SIGINT and SIGTERM request a graceful Core stop, followed by bounded worker
cleanup. Unconfirmed outcomes remain `unknown`.

`--timeout` bounds run or doctor work; its default `0` disables the overall
deadline. Other timeouts must be positive:

| Flag | Default |
| --- | --- |
| `--startup-timeout` | `10s` |
| `--request-timeout` | `30s` |
| `--stop-timeout` | `5s` |
| `--shutdown-timeout` | `5s` |
| `--output-timeout` | `5s` per run output write |

Cleanup has separate deadlines, so the CLI can continue stopping and cleaning up
after the overall deadline. A deadline does not overwrite an already established
successful or failed batch result.

Blocked or broken stdout cancels the run, triggers cleanup and exits `6` without
another stdout write. The terminal may be undeliverable; inspect the local record
for persisted facts.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Run succeeded with complete cleanup, or another command passed. |
| `2` | Input/environment rejection with complete cleanup and no persistence failure, or a failed doctor check. |
| `3` | Runtime or data directory already owned. |
| `4` | Failed result with complete cleanup, including an established failure before a stop or deadline. |
| `5` | Confirmed stop with complete cleanup, no established failure and no deadline. |
| `6` | Unknown or typed-incomplete result, deadline on unfinished work, persistence failure, worker/transport failure, failed cleanup or output failure. |

## References

- [Adapter inputs and result interpretation](../../packages/control/typed-adapters.md)
- [Runtime results, stopping and records](../../packages/control/README.md)
- [Installation metadata and version checks](installation-provenance.md)
