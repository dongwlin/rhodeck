---
type: C4 Component
title: MaaCore Go binding
status: stable
groma:
  id: maa-binding
  parent: worker
  code:
    - scanner: go
      file: packages/maa-go/runtime.go
    - scanner: go
      file: packages/maa-go/buffer.go
    - scanner: go
      file: packages/maa-go/callback.go
    - scanner: go
      file: packages/maa-go/dispatch.go
    - scanner: go
      file: packages/maa-go/doc.go
    - scanner: go
      file: packages/maa-go/errors.go
    - scanner: go
      file: packages/maa-go/event.go
    - scanner: go
      file: packages/maa-go/ids.go
    - scanner: go
      file: packages/maa-go/input.go
    - scanner: go
      file: packages/maa-go/instance.go
    - scanner: go
      file: packages/maa-go/instance_observation.go
    - scanner: go
      file: packages/maa-go/operation.go
    - scanner: go
      file: packages/maa-go/query.go
    - scanner: go
      file: packages/maa-go/snapshot.go
    - scanner: go
      file: packages/maa-go/state.go
    - scanner: go
      file: packages/maa-go/window.go
  group: MaaCore binding
  technology: Go / purego-facing API
description: Owns safe public API calls and native instance lifecycle
---

Public maa Runtime, Instance and Operation APIs validate and freeze input, serialize instance calls, copy outputs and expose stable errors, diagnostics and bounded events. The callback dispatcher settles asynchronous operations by async_call_id and what while preserving other native message content. Cancelling Operation.Wait cancels that wait only.

Each process permits one runtime and one active instance. Creation installs callback routing before native CreateEx; closing rejects new calls, waits for leases, stops/destroys the instance and settles pending operations before Done. Cleanup does not depend on consumers draining buffered events. After lifecycle reservation/termination the process does not hot-reload or recreate the binding; a blocked native close is contained by killing the worker process.

Runtime queries and logging use Runtime lifecycle admission outside the instance serial gate. Callback and event queues never wait for readers: overflow or invalid operation-correlation evidence faults the instance, retains its first error and settles pending operations. PNG/BGR reads copy cached images without triggering a screenshot.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/maa-go/runtime.go](../../../../../../packages/maa-go/runtime.go) | [packages/maa-go/internal/native/loader.go](../../../../../../packages/maa-go/internal/native/loader.go) | Loads native API | Go loader / immutable API table |
| [packages/maa-go/instance.go](../../../../../../packages/maa-go/instance.go) | [packages/maa-go/internal/native/callback_registry.go](../../../../../../packages/maa-go/internal/native/callback_registry.go) | Registers callback targets | Integer token / trampoline |
| [packages/maa-go/instance.go](../../../../../../packages/maa-go/instance.go) | [packages/maa-go/internal/native/calls.go](../../../../../../packages/maa-go/internal/native/calls.go) | Invokes instance operations | Go API table / C ABI wrappers |
