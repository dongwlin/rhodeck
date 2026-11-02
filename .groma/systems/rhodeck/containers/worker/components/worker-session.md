---
type: C4 Component
title: Worker command session
status: stable
groma:
  id: worker-session
  parent: worker
  code:
    - scanner: go
      file: apps/worker/internal/app/session.go
    - scanner: go
      file: apps/worker/internal/app/image.go
  group: Worker session
  technology: Go / protocol-v1 JSONL
description: Serializes native commands and emits correlated observations
---

Validates requests and budgets, serializes access to the engine, sends acceptance before operation waiters publish settlement, and forwards native events and terminal diagnostics with session sequencing. Native callback sequence and IPC session sequence are distinct.

Cached-image requests retain immutable bounded snapshots and serve validated chunks; reads do not trigger screencap. Cleanup releases snapshots, closes the engine, waits for observation/operation workers and flushes close acknowledgment. No run history is persisted in the worker.

Hello precedes MaaCore initialization. Malformed initialize payloads and incompatible wire budgets are rejected before the engine factory and leave the session usable; an initialization failure after entering the factory ends the session. Ping is answered in the reader, so it can respond while the serialized native-command loop is blocked; that response says nothing about task completion.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [apps/worker/internal/app/session.go](../../../../../../apps/worker/internal/app/session.go) | [packages/control/worker/protocol/contract.go](../../../../../../packages/control/worker/protocol/contract.go) | Validates command contracts | Shared Go protocol package |
| [apps/worker/internal/app/session.go](../../../../../../apps/worker/internal/app/session.go) | [apps/worker/internal/app/backend.go](../../../../../../apps/worker/internal/app/backend.go) | Executes native commands | Go engine and operation interfaces |
