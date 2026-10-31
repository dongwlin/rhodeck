# Control application service

`control.Runtime` owns one active run, a fresh supervised worker for that run,
its business state and a private durable record. Only `core-worker` loads
MaaCore. The [standalone CLI](../../apps/cli/README.md) uses this service without
an HTTP server or desktop host.

```go
rt, err := control.New(control.Config{
    DataDir: "/var/lib/rhodeck",
    Worker: worker.Config{Executable: "/opt/rhodeck/core-worker"},
})
if err != nil { return err }
defer rt.Close(context.Background())
run, err := rt.Run(ctx, plan)
if err != nil { return err }
// Consume run.Events() and run.Done() together. After Done, buffered events
// remain readable and run.Snapshot() contains the result including cleanup.
```

Plans use schema version 1, explicit absolute installation/ADB paths, one
connection, and an ordered batch of 1–128 tasks. Each task has a caller-owned
unique `id`, an open string `type`, and raw JSON object `params`. `host_run_id`
is optional host correlation metadata; the application generates its own
random `run_id`. Core task IDs are scoped to that worker and never replace
either identity. `DecodePlan` rejects unknown or duplicate control fields,
invalid UTF-8, trailing data and input over 16 MiB; native JSON objects retain
unknown fields and large integers. `ValidatePlan` checks syntax and local/wire
budgets. Core task acceptance, native limit cross-field rules and device
preconditions remain unverified until the selected installation is used.

Explicit [typed adapters](typed-adapters.md) extend the same schema with an
optional selection and a runtime-local registry. `PreparePlan` provides a pure compiled preview; task
names alone never enable an adapter. The package bundles the strict `oper_progress`
(`NewOperProgressAdapter`) and `switch_theme` (`NewSwitchThemeAdapter`) adapters,
which callers must register explicitly; the standalone CLI registers both for
`run` and `dry-run`. Adapter inputs and results are locally implemented and
tested without authorized device acceptance, so capability output does not claim
they work on a device.

## Results and stopping

The application observes admitted, connecting, submitted, started and stopping
states. Its terminal states are `succeeded`, `failed`, `stopped`, `rejected` and
`unknown`. State is separate from `cleanup` (`complete`, `failed` or
`unconfirmed`): confirmed task facts survive later process or cleanup faults.
A successful host exit requires both `succeeded` and complete cleanup.

Success requires each submitted Core task ID to have a `TaskChainCompleted`
callback and `AllTasksCompleted.finished_tasks` to match the whole batch.
`TaskChainError` remains a failure even if that ID appears in `finished_tasks`.
Subtask errors alone are progress observations because Core may recover them.
For an explicitly adapted task, `TaskResult.State` remains the chain fact while
`TaskResult.Typed` carries independent target facts. A matching completed batch
also requires every typed target to be achieved or already set before the run
can succeed. A typed failure makes the run failed; incomplete evidence makes it
unknown. A fault, lost event or unconfirmed stop before a business conclusion
keeps the run unknown even when typed facts already recorded a failure; those
facts remain preserved. A confirmed stop without an established failure stays
stopped, while a confirmed stop that already recorded a task or typed failure is
failed. Reported training startup and a confirmation observation do not prove
achievement.
Missing/malformed terminal associations, lost events and unconfirmed process
failure preserve `unknown`. Acceptance, Start acknowledgment and
`Running=false` cannot establish success.

Cancelling the run context or calling `Run.Stop` requests the same graceful
stop. Commands use independent bounded request contexts so cancellation does
not casually abandon an enqueued command. Stop confirmation has a separate
five-second default grace period; the supervisor retains its own startup,
request and shutdown deadlines. Unconfirmed work stays unknown after forced
termination. A late cleanup failure is recorded in `cleanup` and `cleanup_code`
but does not rewrite an established `succeeded`, `failed`, `stopped` or
`rejected` state; a confirmed stop without an established failure stays
`stopped`. `Runtime.Close` prevents admission, stops an active run, waits for
worker reaping and then releases storage ownership. A caller's expired Close
context ends that wait; cleanup continues. No component retries, reconnects or
replays device actions automatically.

Events have schema version, run/host identity, monotonic sequence, UTC time,
kind, state and available task/Core associations. The stream is bounded by
count and retained bytes. Consumers that stop reading can cause an explicit
overflow, never an unbounded buffer or cleanup deadlock. Snapshot is
authoritative after Done. Default events retain native message ID, sequence
and task association; `IncludeRawEvents` explicitly opts into private raw JSON.
Worker stderr and native diagnostic prefixes are never automatically exported.

## Records and ownership

`New` takes an OS lock on `<data-dir>/runtime.lock`; another runtime using the
same directory gets `ErrBusy`. The lock file stays in place and OS ownership
ends when the process exits. It is a data-directory lock; different directories
or another CLI may still address the same device. The integrating host must
serialize those device owners.

Records are `<data-dir>/runs/<run_id>.json`, schema version 1 for all runs, containing the
explicit plan, task associations, runtime/library/resource snapshot and result.
The application persists admission before worker launch and metadata before
connection, then saves each newly confirmed terminal task fact while the batch
is still running. Adapted target facts are also saved synchronously before
their typed event is published. The record retains the original caller plan;
the worker receives the separately compiled parameters. Records use private
file permissions, a flushed temporary file and
atomic replacement. Newly created directories and replacements are synced on
Linux/macOS; Windows flushes record contents before replacement, and power-loss
durability of directory metadata has not been verified. Process-crash recovery
and file replacement are covered separately from platform power-loss behavior.
A record persistence failure wraps `ErrRecordWrite`. This includes the admission
save returned by `Runtime.Run`, later run saves and a recovery resave during
`New`; the corresponding code is `record_write_failed`. It is a persistence
failure, not input or environment rejection; the CLI maps it to exit `6`.

On reopening a directory, unfinished records become `unknown` with
`host_interrupted` and unconfirmed cleanup. Already confirmed task facts stay
intact. Recovery launches no worker and never resumes a task. An interrupted
worker may still be stopping after its host dies; recovering a record does not
prove device actions were rolled back. Starting a new run is an explicit host
decision. Corrupt or incompatible records refuse startup instead of silently
discarding evidence. `ReadRecord` reads a bounded atomic snapshot for explicit
inspection; it does not acquire execution ownership.

Records include raw task parameters, paths and connection data and may contain
secrets. Keep the data directory private. Retention, backup and deletion belong
to its owner; this version neither uploads records nor deletes old evidence.
Raw callback streams are transient and are not journaled. The current schema
is still under development and is extended directly before the first release;
unpublished snapshots have no compatibility or migration guarantee. Unsupported
versions and malformed records are rejected without deleting or replaying them.

`Doctor` acquires directory ownership and initializes an explicit installation
in a supervised child, then closes and reaps it. It never connects, appends or
starts tasks, though native initialization can write Core logs to `user_dir`.
Capabilities report native symbols and installation facts, not an enumeration
of supported task types or proof of device compatibility.

This stage provides no scheduler, execution queue, named configurations,
network API, automatic updater, cross-runtime device lock or task-specific
presets. Those can reuse the application service when separately implemented.
