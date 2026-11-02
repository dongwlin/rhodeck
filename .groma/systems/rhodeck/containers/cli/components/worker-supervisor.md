---
type: C4 Component
title: Worker supervisor
status: stable
groma:
  id: worker-supervisor
  parent: cli
  code:
    - scanner: go
      file: packages/control/worker/client.go
  group: Application runtime
  technology: Go / os/exec / pipes
description: Owns child processes, correlated requests and deadlines
---

Launches the explicit absolute worker executable without a shell, validates hello, correlates requests and asynchronous settlements, and exposes bounded events plus independently observable Done and Err. Caches stderr tails and native diagnostics separately from process failures.

After uncertain IPC or deadline failure it terminates and reaps the child instead of retrying or replaying device actions. Graceful Close requires acknowledgment and process exit; forced termination does not fabricate native completion facts. It never loads MaaCore.

Canceling a request after enqueue may leave execution uncertain and terminates the session; canceling Operation.Wait only ends that wait. Native faults and instance-terminal diagnostics are captured independently of Events consumption and remain distinct from process termination. Cached-image reads validate offsets, lengths and the final SHA256 before returning independently owned data.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/worker/client.go](../../../../../../packages/control/worker/client.go) | [apps/worker/main.go](../../../../../../apps/worker/main.go) | Launches and reaps worker | os/exec / process signals |
| [packages/control/worker/client.go](../../../../../../packages/control/worker/client.go) | [apps/worker/internal/app/session.go](../../../../../../apps/worker/internal/app/session.go) | Exchanges commands and events | Protocol-v1 JSONL / stdin and stdout |
| [packages/control/worker/client.go](../../../../../../packages/control/worker/client.go) | [packages/control/worker/protocol/codec.go](../../../../../../packages/control/worker/protocol/codec.go) | Validates IPC messages | Shared Go protocol package |
