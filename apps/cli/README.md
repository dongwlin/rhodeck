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

## Capability report

`capabilities` is schema version 1 JSON. Besides the platform, worker protocol
and command list it reports:

- `task_adapters`: one row per locally registered adapter with `name`,
  `version`, `core_type`, the accepted `input` contract and an `acceptance`
  statement;
- `adapter_basis`: the reviewed implementation source revision (MAA source
  `9b75f9f7b103079458414c6e20216c8c531d6987`) and that the adapters were not
  executed;
- `static_snapshot_basis`: the `MAA v6.20.1` release candidate is a separate
  library and resource snapshot, not the reviewed source revision, and was not
  executed;
- `installation_facts`: `not inspected`. `capabilities` performs no installation
  discovery or native loading; run `doctor` with an explicit plan to inspect an
  installation;
- `device_acceptance`: `none`.

Adapter names and versions come from the same `Descriptor()` values the runner
registers, so the report cannot drift from the registry. There is no dynamic
Core API enumerating supported task types, and an installed Core version string
does not prove that a task or adapter works. `help` restates the adapter names,
the explicit `task.adapter` selection rule and the missing device acceptance.
The generic `task_input` continues to describe the open raw path.

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

### Strict task adapters

Two adapters are registered locally and stay inactive unless the task selects
one with an exact `adapter` object. Selection never happens by task type alone:
an `OperProgress` or `SwitchTheme` task without `adapter` keeps the open raw
path and Core decides whether it accepts the parameters. Both adapter inputs
are accepted from a file or stdin and are identical for `run` and `dry-run`.

```json
{
  "id": "grow-1",
  "type": "OperProgress",
  "adapter": {"name": "oper_progress", "version": 1},
  "params": {"plans": [
    {"role": "Warrior", "name": "Example", "elite": 2},
    {"role": "Caster", "name": "Example Two", "skill_mastery": [0, 0, 3]}
  ]}
}
```

`oper_progress` accepts 1-128 `plans` entries in input order. Each entry requires
`role` and `name` and exactly one of `elite` (1-2), `skill_level` (2-7) or
`skill_mastery` (three integers with exactly one nonzero slot in 1-3). Roles are
exactly `Pioneer`, `Warrior`, `Tank`, `Sniper`, `Caster`, `Medic`, `Support` or
`Special`. `name` must be valid UTF-8, 1-256 bytes, contain no NUL and not be all
whitespace; names are never trimmed or translated. Target IDs are
`op-0001`..`op-0128` by input position. Duplicate `(role, name)` entries, unknown
or null fields, duplicate JSON keys and unsupported roles are rejected.

```json
{
  "id": "theme-1",
  "type": "SwitchTheme",
  "adapter": {"name": "switch_theme", "version": 1},
  "params": {"themes": ["Theme One", "Theme Two"]}
}
```

`switch_theme` accepts 1-32 distinct candidate names. Each candidate must be
valid UTF-8, 1-256 bytes, contain no NUL and have no leading or trailing
whitespace; names are never trimmed, translated or reordered. The sole target is
`theme-selection`. Multiple candidates mean Core selects exactly one at random
once; the array is not a retry, progressive-switch or fallback order. Duplicate
candidates, non-string or null elements and unknown fields are rejected.

For `run` and `dry-run`, strict input is validated before any directory, lock or
worker exists. An unknown adapter name or version, a mismatched Core type,
malformed parameters or an over-budget input exits `2` with code `invalid_plan`,
and stdout remains one JSON error object. `doctor` does not prepare adapters or
submit tasks; it accepts the plan only for installation diagnosis. Raw tasks,
unknown task types and large integer literals are not interpreted or rewritten.

## Preview and diagnosis

```sh
/tmp/rhodeck-cli dry-run --input plan.json \
  --worker /tmp/core-worker --data-dir /var/lib/rhodeck/control
/tmp/rhodeck-cli doctor --input plan.json \
  --worker /tmp/core-worker --data-dir /var/lib/rhodeck/control
```

`dry-run` validates syntax and budgets, calls `control.PreparePlan` with the same
registered adapters `run` uses, and prints one JSON object containing:

- `plan`: the explicit plan with each selected adapter's final compiled Core
  parameters; raw tasks keep their original parameters;
- `tasks`: the prepared per-task results, including each adapted task's
  `unobserved` target IDs and `typed_incomplete` aggregate;
- `sources` and `unverified`: the input/worker/data-dir sources plus
  installation, Core, device and adapter-specific conditions that local
  preparation cannot verify (operator identity, skills and prerequisites, theme
  availability and unlock state, and the absence of authorized device acceptance).

`run --dry-run` is equivalent. An unknown adapter, unsupported version, rejected
strict input or over-budget adapter parameters exits `2` with `invalid_plan`.
The preview never starts a worker, loads Core, connects a device, appends tasks,
creates directories or acquires locks. Installation availability, native task
semantics and device preconditions remain explicitly unverified. The preview
includes the supplied installation, connection and raw task parameters, so
private input appears there; strict adapter parameters are shown compiled, and
target facts are local previews, not execution evidence.

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

An adapted task result also carries `typed`: schema version 1, the selected
`adapter`, an independent business `state`/`code`, the fixed per-task `targets`
and an optional `summary`. Each changed target fact is validated and saved
before a `type: "typed"` event carrying the full task result is written; these
events need no `--include-raw-events` and never include raw callback fields.
`TaskResult.state` remains the native task-chain fact. Once a business failure is
established, a chain success cannot overwrite it: any `failed`, `locked` or
`not_found` target makes the result `failed` with `typed_failed` and exit `4`.
This applies after a completed batch and also when a stop or deadline interrupts
a run that already recorded the failure. Every target must be `achieved` or
`already_set` for success; a completed batch without that evidence is `unknown`
with `typed_incomplete` and exit `6`. Operator mastery startup reports
(`reported_target_training_started`, `reported_prerequisite_training_started`)
and a SwitchTheme confirmation node (`confirmation_observed`) are incomplete
evidence, not completion. The confirmation node records that the action ran; it
does not re-read or verify the final theme state. A locked or not-found theme
fails even when its task chain completes, while a skip stays incomplete.
Operator failure details keep a stable lowercase reason such as
`resource_insufficient` or `operator_not_found`.

A worker fault, lost event or unconfirmed stop before a business conclusion makes
the run `unknown` and exits `6`. If a batch result or failure was already
established, it is preserved and the fault is reported in `cleanup`; the CLI
still exits `6` because cleanup did not complete. Persistence failure and failed
cleanup likewise make the CLI exit `6` without rewriting an established
`succeeded`, `failed`, `stopped` or `rejected` state. A confirmed stop without
an established failure remains `stopped`; a stop that already established a
task or typed failure remains `failed` (including `typed_failed`). A deadline
that stops an unfinished run exits `6`; an established failure wins with exit
`4`, and an already completed successful batch stays successful.

When persistence succeeds, the terminal snapshot and the durable record hold the
same typed facts. A typed persistence failure keeps the in-memory fact in the
terminal snapshot, does not publish the undurable typed event, and stops the run
with `record_write_failed`; a later final save may or may not repair the durable
record. The record keeps the original caller plan while the worker receives the
compiled parameters.

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
| `2` | Input or environment rejection with completed cleanup and no persistence failure; a doctor check failed. |
| `3` | Runtime or data directory already owned. |
| `4` | Failed result with completed cleanup, including `typed_failed` and a stop or deadline that already established a failure. |
| `5` | Confirmed stop with completed cleanup, no established failure and no deadline. |
| `6` | Typed-incomplete or unknown result, deadline without an established failure, `record_write_failed` or other persistence failure, worker/transport failure, failed cleanup or output failure. |

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

This is the generic execution foundation plus two opt-in local adapters. It
provides no task presets, scheduler, HTTP API, installer/updater, cross-runtime
device lock, Docker image or La Pluma Next integration. `oper_progress` and
`switch_theme` validate strict input and interpret filtered business facts, but
neither has authorized device acceptance; the static basis is a review of MAA
source `9b75f9f7b103079458414c6e20216c8c531d6987`, while the `v6.20.1` release
candidate remains a separate, unexecuted library/resource snapshot. All target
semantics remain unverified on a device. The open
raw path still accepts any Core task type. A data directory lock does not
establish mutual exclusion with another directory, external CLI or host.

Run CLI/control regressions from the repository root:

```sh
CGO_ENABLED=0 go test ./apps/cli ./packages/control/... -count=1 -timeout=3m
go test -race ./apps/cli ./packages/control/... -count=1 -timeout=3m
```

The CLI tests use real helper processes without a native library or device and
cover machine output, raw-parameter preservation, strict adapter input and
preview, mixed raw/adapted batches, typed failure and incomplete exits, record
and terminal parity, private-field filtering, rejection before directory
creation, task failure, crash, confirmed cancellation, SIGINT/SIGTERM on Unix,
and doctor command boundaries. The worker's opt-in real-Core no-device tests
remain a separate evidence layer.
