---
type: Groma Flow
title: Stop a run and recover interrupted records
groma:
  id: stop-a-run-and-recover-interrupted-records
---

Stopping and recovery retain evidence boundaries. Forced process termination leaves unconfirmed device results unknown. Reopening after directory ownership is released preserves confirmed terminal business states and typed facts. Records interrupted before business completion change only nonterminal run and task states to unknown; if business completion was already recorded but cleanup was interrupted, its business state survives. Interrupted records receive host_interrupted and cleanup unconfirmed. Recovery never resumes, reconnects or replays actions.

## Steps

| From | To | Action |
| --- | --- | --- |
| [go-integrator](../actors/go-integrator.md) | [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | Cancel a run context or request Stop. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | Perform bounded native stop and worker teardown; enforce child termination when necessary. |
| [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | [worker-session](../systems/rhodeck/containers/worker/components/worker-session.md) | Request graceful close while keeping stdin open; require acknowledgment and process exit. |
| [worker-session](../systems/rhodeck/containers/worker/components/worker-session.md) | [native-engine](../systems/rhodeck/containers/worker/components/native-engine.md) | Release cached image snapshots, close the engine and wait for observers and operation waiters. |
| [native-engine](../systems/rhodeck/containers/worker/components/native-engine.md) | [maa-binding](../systems/rhodeck/containers/worker/components/maa-binding.md) | Close the runtime; the supervisor can terminate the process if native cleanup blocks. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | Persist final confirmed facts and unknown outcomes before Run.Done. |
| [go-integrator](../actors/go-integrator.md) | [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | Close the owning runtime; shared cleanup retains the directory lock even if the caller stops waiting. Reopen only after cleanup releases ownership. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | Validate saved records; preserve confirmed terminal business states and typed facts, change only nonterminal states to unknown, and record host_interrupted with cleanup unconfirmed for interrupted records. |
| [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | [record-filesystem](../systems/rhodeck/containers/cli/components/record-filesystem.md) | Read bounded record bytes and atomically replace recovered JSON under directory ownership; domain validation belongs to run records. |
