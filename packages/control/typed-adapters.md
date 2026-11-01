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
and create a fresh reducer for each task with interpreted targets. Input-only
adapters return no reducer. Adapters may be prepared concurrently.
The package bundles four strict adapters: `NewOperProgressAdapter`
(`oper_progress`, version 1), `NewSwitchThemeAdapter` (`switch_theme`,
version 1), `NewCopilotAdapter` (`copilot`, version 1) and
`NewRoguelikeAdapter` (`roguelike`, version 1). The
[standalone CLI](../../apps/cli/README.md) registers all four explicitly for
`run` and `dry-run`; a task still selects one only through `Task.Adapter`.
These adapters have no authorized device acceptance. The CLI
capability report keeps the reviewed MAA source revision, the rolling Core
version policy, the uninspected installation and the missing device acceptance
separate. Actual versions and digests come from doctor/run/record; the host
maintains [installation provenance](../../apps/cli/installation-provenance.md).
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

## Copilot input and stage results

Select `adapter: {"name":"copilot","version":1}` with Core type `Copilot`.
The strict input accepts exactly one source:

```json
{"copilot_list":[{"filename":"job.json"},{"filename":"job.json","is_raid":true}],"formation":true,"operbox_data_path":"box.json","user_additional":[{"name":"Example","skill":3}]}
```

| Field | Accepted values |
| --- | --- |
| `filename` | Nonempty path string, mutually exclusive with `copilot_list` |
| `copilot_list` | 1–128 objects with required nonempty `filename`, optional unique signed 32-bit integer `id`, string `nav_name_override` (may be empty), boolean `is_raid` (default false) |
| `formation` | Boolean, default false |
| `formation_index` | Integer 0–4; 0 leaves selection unspecified |
| `add_trust`, `ignore_requirements`, `use_sanity_potion` | Boolean |
| `support_unit_usage` | Integer 0–3; 2 requires `support_unit_name` |
| `support_unit_name` | Optional nonempty string |
| `loop_times` | Integer 1–2147483647; lists only allow omission or 1 |
| `operbox_data_path` | Optional nonempty path string; requires `formation=true` |
| `user_additional` | Array of objects with unique nonempty `name` and required integer `skill` 0–3; requires `formation=true` |

Strings must be valid UTF-8 without NUL; unpaired surrogate escapes are rejected.
Unknown fields, explicit nulls, duplicate keys at any depth and input or compiled
JSON over 64 KiB are rejected. `is_paradox` and list-entry `stage_name` are not
supported; use `nav_name_override` for a navigation override. Omitted options
other than `formation`, `id` and `is_raid` retain Core defaults. Preparation
does not open files, check their contents or existence, normalize paths or fetch
network jobs. It validates structure, not whether the installation or device
can execute the requested job.

Compilation first reserves all explicit IDs, then assigns omitted IDs from
zero in list order while skipping reserved values. A filename is a source,
not an identity: normal/raid variants and repeated executions of the same file
remain separate entries. Each list target is `copilot:<compiled id>` and uses
policy `all`. Single-file inputs, including loops, use `none` and produce no
typed results or typed events. Their chain success does not establish a clear
or a safe device handoff; battle actions may still be in progress.

For lists, `CopilotListLoadTaskFileSuccess` extra-info from `MultiCopilotTask`
must name the exact compiled `details.id` and matching `details.file_name`.
It only establishes an entry scope. On Windows the association accounts for
Core converting backslashes to slashes; it does not clean paths, fold case or
change submitted params. A `MultiCopilotTask` start or unusable load boundary
clears the previous scope. Re-entering an entry whose scope was left is not
interpreted as another execution. Callbacks without an explicit usable scope
leave targets unobserved, including installations that omit the entry ID.

Core can emit Box prechecks and file/parameter rejection diagnostics while
appending tasks, before it allocates a task ID. The runtime ignores only
recognized preparation diagnostics with explicit `taskid=0` and exact Copilot
chain, subtask and diagnostic identifiers; they create no typed facts or entry
scope. Append acceptance or rejection remains authoritative for that phase.
Unknown diagnostics, missing IDs and unassociated settlement callbacks retain
the normal conservative error handling. Duplicate keys are never exempted.

Only `SubTaskStart` from the same accepted Core task, `taskchain=Copilot`,
`subtask=ProcessTask` and exact `first=["Copilot@WaitUntilEndOfAction"]`
can establish these facts within that scope:

| Exact `details.task` | Outcome / code |
| --- | --- |
| `StageDrops-Stars-3` | `achieved` / `three_star_clear` |
| `StageDrops-Stars-Adverse` | `achieved` / `adverse_clear` |
| `FightMissionFailed` | `failed` / `mission_failed` |

Core strips the namespace from `details.task`; prefixed or suffix lookalikes
are not matched. The adverse marker is also mapped to three stars by Core;
there is no supported non-three-star clear fact. Typed details expose only
`entry_id`, zero-based `entry_index` and `is_raid`, without file paths or raw
recognition data. Loading, `CopilotAction`, acceptance and chain completion
do not manufacture clears. Missing evidence remains `typed_incomplete` even
after chain success. Identical facts are idempotent; contradictory facts
preserve the first fact and trigger `typed_conflict`.

This scope relies on Core's synchronous list execution and FIFO callbacks,
reviewed at upstream revision `9b75f9f7b103079458414c6e20216c8c531d6987`;
it does not pin or guarantee compatibility with a Core version. Local tests
cover preparation, interpretation, lifecycle faults and durable recovery.
Actual installation callbacks and device behavior remain unverified. Result
integration describes lists as `stage_results` with policy `all` and
single-file/loop inputs as `none`; the CLI capability report states the same
selection through `objective_policies` and `policy_selection`, and its dry-run
shows the policy each task actually selected.

## Roguelike input and mode objectives

Select `adapter: {"name":"roguelike","version":1}` with Core type `Roguelike`.
The strict input requires an explicit `theme` (`Phantom`, `Mizuki`, `Sami`,
`Sarkaz`, `JieGarden` or `BlackFlow`) and `mode` from the frozen per-theme
matrix: 0, 1, 4, 6 and 7 for every theme except BlackFlow, plus 5 for Sami,
10001 for Sarkaz and 20001 for JieGarden; BlackFlow accepts only 0, 1, 30001
and 30003. Modes 2 and 3 and every other theme/mode combination are rejected.
Common squad, investment and early-stop options and theme- or mode-scoped
fields (such as `monthly_squad_auto_iterate`, `deep_exploration_auto_iterate`,
`find_playTime_target` and the `blackflow_*` set) are validated strictly:
unknown fields, explicit nulls, duplicate keys at any depth, misplaced
theme/mode fields and invalid combinations — including `only_start_with_elite_two`
outside mode 4, the mode 30003 start-run requirements and values Core would
silently override — are rejected with `ErrInvalidPlan`. Compilation keeps the
submitted values verbatim and injects no Core defaults.

The objective policy follows the compiled configuration instead of one static
value:

- Mode 0 declares one target per run, `roguelike:run:1..N`, with policy `any`
  for several runs and `all` for a single run — only for a `starts_count` of
  1-128 with no `stop_at_final_boss`/`stop_at_max_level` switch and no
  reachable investment stop, and only outside the BlackFlow strategy profile.
- Mode 1 declares `roguelike:investment` with policy `all` only when the
  investment limit can be reached at all.
- Mode 6 declares `roguelike:monthly_squad` and mode 7
  `roguelike:deep_exploration` with policy `all` only when
  `monthly_squad_auto_iterate` or `deep_exploration_auto_iterate` is true;
  with the switch omitted or false Core never emits the rotation callback, so
  those configurations stay input-only.
- Modes 20001 and 30003 declare `roguelike:jie_garden_target` and
  `roguelike:start_run` with policy `all`.
- Every other configuration — modes 4, 5, 10001 and 30001, the BlackFlow
  strategy profiles, and any configuration whose chain can end before its
  declared runs settle — uses `none`: input is validated and compiled, and no
  business objective is claimed.

Facts come only from callbacks of the accepted Core task with
`taskchain=Roguelike`: run settlements (`GamePass`, `MissionFailedFlag2`) are
counted per declared run in arrival order, and the extra-info facts
`RoguelikeInvestmentReachLimit`/`RoguelikeInvestmentReachFull`,
`MonthlySquadCompleted`, `DeepExplorationCompleted`,
`RoguelikeJieGardenTargetFound` and the BlackFlow start-run status establish
their single objective exactly. Chain completion, stops and unknown statuses
never create facts; missing evidence stays unobserved and is never inferred
from `AllTasksCompleted`. All roguelike facts have no authorized device
acceptance, and the CLI dry-run shows the policy each task actually selected.

## Preparation and policies

`PreparedTask.Params` is the final Core object; `PreparedTask.Targets` contains
stable, unique target IDs. `PreparedTask.Policy` selects `ObjectiveAll` (`all`,
also the default when omitted), `ObjectiveAny` (`any`) or `ObjectiveNone`
(`none`). `all` and `any` require at least one target and a non-nil reducer.
`none` requires zero targets and a nil reducer: it validates and compiles input
without interpreting business results. Unknown policies and illegal combinations
return `ErrInvalidPlan` before admission. For example, an input-only adapter may
return `PreparedTask{Params: compiled, Policy: ObjectiveNone}`.

`PreparedPlan.Tasks[i].Policy` exposes the normalized choice as
`objective_policy`. Interpreted tasks repeat that choice in
`TypedResult.Policy`, so each typed event carries its own aggregation contract.
Input-only tasks carry `objective_policy: "none"` and omit `typed`; they publish
no typed events. Their run outcome follows native chain facts and makes no
business-objective claim. Raw task results omit both fields. OperProgress and
SwitchTheme use `all`. Copilot lists use `all` and its single-file and loop
inputs `none`. Roguelike selects `all`, `any` or `none` from the theme, mode,
run count and rotation switches (see the Roguelike section). Policy selection
belongs to the adapter; it is
not an additional caller-controlled plan field.

`PreparedPlan.Plan` contains the compiled parameters.
The runtime prepares its own detached submission and keeps the original caller
plan in the private record. Mutating a preview cannot alter an admitted run.

## Schema

| Container | Generic input | Explicit adapter input |
| --- | --- | --- |
| Plan | `schema_version: 1` | Same schema, optional per-task `adapter`; mixed raw tasks allowed |
| Result, Event, Record | `schema_version: 1` | Same schema, task result `objective_policy`; `typed` only for `all`/`any` |
| TypedResult | Absent | `schema_version: 1`, matching `objective_policy: "all"` or `"any"` |
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
nothing. A raw task omits adapter, policy and typed fields. An adapted record
requires an explicit policy: `none` has no typed result, while `all`/`any` require
a matching typed adapter and policy with nonempty targets. Missing or unknown
policies and inconsistent combinations are rejected; they never fall back to
raw processing. Recovery preserves the recorded policy and facts without
preparing the adapter or reinterpreting observations.
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
`succeeded` under `all`. Any failure produces `failed` with `typed_failed`;
otherwise the aggregate is `unknown` with `typed_incomplete`.

Under `any`, at least one successful target produces `succeeded`, even when
other independent targets failed or remain unobserved/ambiguous. Without success,
all targets must have confirmed failure evidence to produce `failed` with
`typed_failed`; any remaining incomplete evidence gives `unknown` with
`typed_incomplete`. Different rounds or entries require distinct prepared target
IDs. Both failure-then-success and success-then-failure across distinct targets
retain each fact and satisfy `any`. Reusing a single target for another round
is a conflict, not a way to overwrite its history. A confirmed failure cannot be
replaced by later success; a contradictory update in either direction stops
observation with `typed_conflict` while preserving the original fact. This
conflict remains a run fault even if `any` was already achieved.

Native task-chain state remains in `TaskResult.State`. For a matching completed
batch, aggregate failure wins over unknown,
which wins over success. A confirmed stop retains target facts and the existing
stopped result unless a failure has already been established; a stop that
already recorded a task or typed failure stays failed. A worker fault, lost
event or unconfirmed stop before a business conclusion keeps the run unknown
even when a typed failure was already recorded; confirmed target facts are
preserved. Cleanup remains
independent: a cleanup failure never produces a successful CLI exit and does not
rewrite an established business state.

Achieving an objective alone never completes a run or stops observation. Chain
errors still fail the run; confirmed stops, timeouts, worker faults and lost
events follow the same runtime rules even after an `any` objective was achieved.
`TaskChainCompleted` (10002) does not create target facts. `none` avoids
`typed_incomplete` by making no typed claim, rather than treating chain completion
as business success.

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
outcomes, policies and inconsistent aggregates are rejected. Retained results
contain current immutable facts, not callback histories.

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
