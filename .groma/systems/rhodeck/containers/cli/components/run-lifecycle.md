---
type: C4 Component
title: Run lifecycle and outcomes
status: stable
groma:
  id: run-lifecycle
  parent: cli
  code:
    - scanner: go
      file: packages/control/runtime.go
    - scanner: go
      file: packages/control/types.go
    - scanner: go
      file: packages/control/run.go
    - scanner: go
      file: packages/control/execute.go
    - scanner: go
      file: packages/control/observe.go
    - scanner: go
      file: packages/control/stop.go
    - scanner: go
      file: packages/control/events.go
    - scanner: go
      file: packages/control/doctor.go
      symbol: Doctor
  group: Application runtime
  technology: Go / channels and contexts
description: Coordinates execution, evidence, stopping and final cleanup
---

Owns Runtime and Run state, run identity, worker launch/initialization/connect/append/start sequencing, the sole worker Events consumer and typed application events. It associates native task IDs with admitted task entries, routes callbacks to task-local reducers and derives whole-batch outcomes from matching terminal evidence and completed-task facts.

Confirmed typed facts are saved before typed-event publication; terminal persistence and child cleanup precede Run.Done. Cancellation and Stop converge on bounded stopping, with unresolved actions left unknown. Doctor reuses supervised initialization and teardown without connecting or submitting tasks. The runtime uses injected adapter contracts and never imports concrete adapters.

The single observation consumer feeds bounded application events without waiting for readers. Overflow during execution records a fatal condition and cancels outstanding commands; finalization preserves established business proof. Runtime.Close starts shared cleanup. Cancellation of one caller's close wait does not cancel that cleanup or release the data-directory lock early; ownership lasts until cleanup finishes.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/runtime.go](../../../../../../packages/control/runtime.go) | [packages/control/record.go](../../../../../../packages/control/record.go) | Persists admitted runs | Go record store API |
| [packages/control/observe.go](../../../../../../packages/control/observe.go) | [packages/control/objective.go](../../../../../../packages/control/objective.go) | Aggregates objective facts | Task-local typed result updates |
| [packages/control/execute.go](../../../../../../packages/control/execute.go) | [packages/control/worker/client.go](../../../../../../packages/control/worker/client.go) | Controls isolated execution | Go Client API |
| [packages/control/observe.go](../../../../../../packages/control/observe.go) | [packages/control/adapters/copilot/result.go](../../../../../../packages/control/adapters/copilot/result.go) | Reduces task callbacks | Injected task-local Go reducer |
| [packages/control/observe.go](../../../../../../packages/control/observe.go) | [packages/control/adapters/operprogress/result.go](../../../../../../packages/control/adapters/operprogress/result.go) | Reduces task callbacks | Injected task-local Go reducer |
| [packages/control/observe.go](../../../../../../packages/control/observe.go) | [packages/control/adapters/roguelike/result.go](../../../../../../packages/control/adapters/roguelike/result.go) | Reduces task callbacks | Injected task-local Go reducer |
| [packages/control/observe.go](../../../../../../packages/control/observe.go) | [packages/control/adapters/switchtheme/result.go](../../../../../../packages/control/adapters/switchtheme/result.go) | Reduces task callbacks | Injected task-local Go reducer |
| [packages/control/runtime.go](../../../../../../packages/control/runtime.go) | [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | Prepares original plan independently | Go PreparePlan / injected registry |
