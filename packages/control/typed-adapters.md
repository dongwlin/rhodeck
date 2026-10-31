# Explicit task adapters

`Task.Adapter` opts one task into a locally registered `TaskAdapter`. The
selection is an exact `AdapterRef{Name, Version}` match. A familiar `Task.Type`
without that selection remains an open Core task, preserving raw parameters,
unknown fields and integer literals. Worker and protocol have no adapter registry.

Configure adapters with `Config.Adapters`, or call
`PreparePlan(plan, adapters...)` to validate and preview the compiled plan and
initial target results. Preparation does not create directories, take locks,
launch workers, load native code or connect devices. Each adapter must implement
deterministic preparation without side effects, reject incompatible task types,
and create a fresh reducer for each task. Adapters may be prepared concurrently.
The package bundles two strict adapters: `NewOperProgressAdapter`
(`oper_progress`, version 1) and `NewSwitchThemeAdapter` (`switch_theme`,
version 1). The [standalone CLI](../../apps/cli/README.md) registers both
explicitly for `run` and `dry-run`; a task still selects one only through
`Task.Adapter`. Neither adapter has authorized device acceptance. The CLI
capability report keeps the reviewed MAA source revision, the `v6.20.1` release
candidate snapshot, the uninspected installation and the missing device
acceptance separate.
The OperProgress
input accepts only exact, case-sensitive `plans` entries with `role`, `name` and
exactly one of `elite` (1-2), `skill_level` (2-7) or `skill_mastery` (three
integers, exactly one nonzero slot in 1-3):

```json
{"plans":[{"role":"Warrior","name":"Example","elite":2}]}
```

`role` is exactly one of `Pioneer`, `Warrior`, `Tank`, `Sniper`, `Caster`,
`Medic`, `Support` or `Special`. `name` must be valid UTF-8, 1-256 bytes,
contain no NUL and not be all whitespace; names are never trimmed or
translated. Unknown fields, explicit nulls, duplicate keys at any depth,
unpaired UTF-16 surrogate escapes and duplicate `(role, name)` entries are
rejected with `ErrInvalidPlan`. See `ExampleNewOperProgressAdapter` for a
deterministic preview of compiled params and target IDs.

The SwitchTheme input accepts only an exact, case-sensitive `themes` array of
1-32 distinct names. Each name must be valid UTF-8, 1-256 bytes, contain no NUL
and have no leading or trailing whitespace:

```json
{"themes":["Theme One","Theme Two"]}
```

The compiled Core array keeps input order and the sole target is
`theme-selection`. Multiple candidates mean Core selects one at random once; the
order is not a retry, progressive-switch or fallback sequence. A selected theme
is recorded only after a completed `SwitchThemeByNameSelectTheme` node whose
`details.result.text` exactly and uniquely matches a candidate. Subsequent
`SwitchThemeByNameAlreadySet`, `SwitchThemeByNameLockedTheme` and
`SwitchThemeByNameConfirmTheme` nodes become `already_set`, `locked` and
`confirmation_observed`; a matching `SwitchThemeNotFound` extra-info event
becomes `not_found`. Skipped, start-only, missing OCR text and conflicting
selections remain incomplete. Details contain only the associated theme name,
and confirmation never implies an actual change or a verified final state.
Unknown fields, explicit nulls, duplicate keys at any depth, unpaired UTF-16
surrogate escapes and duplicate candidates are rejected with `ErrInvalidPlan`.
See `ExampleNewSwitchThemeAdapter` for a deterministic preview.

`PreparedTask.Params` is the final Core object; `PreparedTask.Targets` contains
stable, unique target IDs. `PreparedPlan.Plan` contains the compiled parameters.
The runtime prepares its own detached submission and keeps the original caller
plan in the private record. Mutating a preview cannot alter an admitted run.

## Schema

| Container | Generic input | Explicit adapter input |
| --- | --- | --- |
| Plan | `schema_version: 1` | Same schema, optional per-task `adapter`; mixed raw tasks allowed |
| Result, Event, Record | `schema_version: 1` | Same schema, optional `typed` on task results and typed events |
| TypedResult | Absent | `schema_version: 1` |
| CLI envelope | `schema_version: 1` | `schema_version: 1` |

The schema is under development and extends version 1 directly before the first
release. Unpublished snapshots have no compatibility guarantee. Adapter versions
select reducer implementations and do not denote product releases.

`DecodePlan` and `ValidatePlan` check structural versions and budgets.
`PreparePlan` additionally resolves adapter identity/version and validates
adapter output. `Runtime.Run` prepares before admission or launching a worker;
an unknown adapter returns `ErrInvalidPlan`. The standalone CLI's `run` prepares
the same registry before `control.New`, so an unsupported selection or rejected
strict input exits `2` with `invalid_plan` before any directory, lock or worker
exists. Its dry-run prints the compiled plan, each task's unobserved target
preview and adapter-specific unverified conditions without side effects.

Records do not require the original adapter to be installed for reading.
Recovery retains confirmed facts, marks unfinished work unknown, and executes
nothing. Omitted optional adapter/result fields use the generic path.
Unsupported container or nested result versions, malformed associations and
over-budget records refuse reading/startup. No automatic migration, deletion or
replay is performed for records from unpublished snapshots.

## Observations and results

The runtime's single worker event consumer dispatches associated callbacks to
the task-local `TaskReducer`. `Observation.JSON` is detached from internal state.
Reducers should ignore unrelated message kinds, require sufficient task and
target association, and retain bounded scratch state. They must select business
fields for `Details` and `Summary`; unfiltered native payloads, OCR geometry,
private paths and diagnostic logs do not belong in normal typed events.

A `TypedUpdate` may confirm a subset of the prepared target IDs and one summary
object. An unobserved target may receive its first fact. Repeating an identical
fact is harmless; a different fact for that target returns `ErrTypedConflict`
and preserves the original. A summary is also immutable after its first update.
Equality of raw JSON details is byte equality. After a task chain terminates or
the batch result is established, late callbacks cannot add typed facts.

| Target outcome | Typed aggregate contribution |
| --- | --- |
| `achieved`, `already_set` | Success evidence |
| `failed`, `locked`, `not_found` | Failure evidence |
| `unobserved`, `ambiguous` | Incomplete evidence |
| `reported_target_training_started`, `reported_prerequisite_training_started` | Reported startup, incomplete achievement evidence |
| `confirmation_observed` | Observed confirmation, incomplete final-state evidence |

All targets must contribute success evidence for `TypedResult.State` to be
`succeeded`. Any failure produces `failed` with `typed_failed`; otherwise the
aggregate is `unknown` with `typed_incomplete`. Native task-chain state remains
in `TaskResult.State`. For a matching completed batch, failure wins over unknown,
which wins over success. A confirmed stop retains target facts and the existing
stopped result unless a failure has already been established; a stop that
already recorded a task or typed failure stays failed. A worker fault, lost
event or unconfirmed stop before a business conclusion keeps the run unknown
even when a typed failure was already recorded; confirmed target facts are
preserved. Cleanup remains
independent: a cleanup failure never produces a successful CLI exit and does not
rewrite an established business state.

Every changed fact is validated and saved before its `type: "typed"` event is
published. Typed events include a full detached task `TypedResult` and work
without raw diagnostics. Snapshots, events, adapter inputs/outputs and decoded
records do not share mutable target slices or JSON buffers. `Snapshot` after
`Done` is authoritative if a consumer falls behind. Persistence failure preserves
observed in-memory facts, stops further work and reports `record_write_failed`;
it cannot establish durable success or trigger replay.

## Limits and errors

| Resource | Limit |
| --- | --- |
| Explicit adapter input and compiled params | 64 KiB per task |
| Targets | 128 per task, 512 per run |
| Encoded typed result | 64 KiB per task, 512 KiB across the run |
| Target detail / summary | 4 KiB / 8 KiB |
| Encoded typed event | 64 KiB |
| Queued typed events | 32 messages, 256 KiB, within the existing 64-message / 16-MiB total queue |
| Record | Existing 16-MiB plan allowance plus 1 MiB |

Details and summaries must be UTF-8 JSON objects with no duplicate keys at any
depth. Unknown business fields and large integer literals retain their raw JSON;
an adapter decides which fields belong in its public contract. Unsupported
outcomes and inconsistent aggregates are rejected. Retained results contain
current immutable facts, not callback histories.

Result validation errors support `errors.Is` with `ErrTypedResult`,
`ErrTypedBudget` and `ErrTypedConflict`. Record persistence errors wrap
`ErrRecordWrite`, including an admission save returned by `Runtime.Run`, a later
run save and a recovery resave during `New`; the run result itself uses the
stable `record_write_failed` code. Runtime codes are `typed_invalid_event`,
`typed_result_overflow`, `typed_event_overflow`, `typed_conflict` and
`record_write_failed`.
Rejected updates preserve confirmed facts and cannot turn the run into success.
Queue overflow stops work without waiting for the consumer to drain. Raw
callbacks remain transient and require the separate diagnostic opt-in; original
input plans remain private durable data under the existing retention policy.
