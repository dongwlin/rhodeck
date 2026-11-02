---
type: C4 Component
title: Shared worker protocol
status: stable
groma:
  id: worker-protocol
  parent: worker
  code:
    - scanner: go
      file: packages/control/worker/protocol/protocol.go
    - scanner: go
      file: packages/control/worker/protocol/contract.go
    - scanner: go
      file: packages/control/worker/protocol/codec.go
    - scanner: go
      file: packages/control/worker/protocol/queue.go
  group: Worker session
  technology: Go / protocol-v1 JSONL
description: Defines bounded DTO, codec and queue contracts
---

Pure transport contract shared by worker and parent client: snake_case DTOs, strict request/response validation, bounded line encoding/decoding, image transfer constraints and bounded queues. It imports neither the native binding nor process/application services. Single source ownership under the worker keeps source links unambiguous; both executables compile and use it.

Protocol version 1 keeps request IDs, IPC event sequence and Core callback sequence as separate correlation domains. Accepted operation settlements must match their request and async identities; JSON decoding or an IPC response alone is not completion evidence.
