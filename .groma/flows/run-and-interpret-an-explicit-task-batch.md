---
type: Groma Flow
title: Run and interpret an explicit task batch
groma:
  id: run-and-interpret-an-explicit-task-batch
---

An OperProgress plan illustrates execution through the CLI and isolated worker. Control persists admission before side effects and requires associated Core completion evidence plus typed objective facts. Submission acceptance and Running=false are insufficient.

## Steps

| From | To | Action |
| --- | --- | --- |
| [operator](../actors/operator.md) | [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | Provide a run plan and explicit worker, MaaCore, resources and ADB paths. |
| [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | [plan-workflows](../systems/rhodeck/containers/cli/components/plan-workflows.md) | Load caller input and invoke Execute. |
| [plan-workflows](../systems/rhodeck/containers/cli/components/plan-workflows.md) | [plan-preparation](../systems/rhodeck/containers/cli/components/plan-preparation.md) | Validate and freeze raw or typed tasks before execution side effects. |
| [plan-preparation](../systems/rhodeck/containers/cli/components/plan-preparation.md) | [operprogress-adapter](../systems/rhodeck/containers/cli/components/operprogress-adapter.md) | Prepare the declared operator targets and task-local reducer. |
| [plan-workflows](../systems/rhodeck/containers/cli/components/plan-workflows.md) | [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | Open the directory-owning runtime and submit the original plan for independent preparation and admission. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [plan-preparation](../systems/rhodeck/containers/cli/components/plan-preparation.md) | Prepare the original plan again with fresh task-local reducer state. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | Save the original plan and run admission before launching a child. |
| [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | [record-filesystem](../systems/rhodeck/containers/cli/components/record-filesystem.md) | Atomically write the private run JSON under the held directory lock. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | Launch and initialize the isolated worker. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | Persist actual installation facts before connecting. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | Connect, append tasks, save associated submissions and start the batch; control owns record persistence. |
| [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | [worker-session](../systems/rhodeck/containers/worker/components/worker-session.md) | Exchange correlated commands, responses, operation events and native observations. |
| [worker-session](../systems/rhodeck/containers/worker/components/worker-session.md) | [native-engine](../systems/rhodeck/containers/worker/components/native-engine.md) | Serialize engine commands and observe native activity. |
| [native-engine](../systems/rhodeck/containers/worker/components/native-engine.md) | [maa-binding](../systems/rhodeck/containers/worker/components/maa-binding.md) | Create the MaaCore instance and submit the requested native operations. |
| [maa-binding](../systems/rhodeck/containers/worker/components/maa-binding.md) | [native-abi](../systems/rhodeck/containers/worker/components/native-abi.md) | Use the resolved ABI table for native calls. |
| [native-abi](../systems/rhodeck/containers/worker/components/native-abi.md) | [maacore](../externals/maacore.md) | Invoke the caller-provided engine inside the worker process. |
| [maacore](../externals/maacore.md) | [android-device](../externals/android-device.md) | Perform configured device actions through ADB. |
| [maacore](../externals/maacore.md) | [native-callbacks](../systems/rhodeck/containers/worker/components/native-callbacks.md) | Report native completion and progress messages. |
| [native-callbacks](../systems/rhodeck/containers/worker/components/native-callbacks.md) | [maa-binding](../systems/rhodeck/containers/worker/components/maa-binding.md) | Route token-addressed messages for leased copying and event dispatch. |
| [native-engine](../systems/rhodeck/containers/worker/components/native-engine.md) | [maa-binding](../systems/rhodeck/containers/worker/components/maa-binding.md) | Consume the binding Events stream while watching Fault and Done separately and forwarding terminal diagnostics. |
| [native-engine](../systems/rhodeck/containers/worker/components/native-engine.md) | [worker-session](../systems/rhodeck/containers/worker/components/worker-session.md) | Publish native observations, faults and terminal diagnostics through the session emitter. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | Consume the sole Events stream and observe Done and Err independently. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [operprogress-adapter](../systems/rhodeck/containers/cli/components/operprogress-adapter.md) | Route associated Core task callbacks to the prepared reducer. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [typed-objectives](../systems/rhodeck/containers/cli/components/typed-objectives.md) | Validate immutable target facts and aggregate the declared objective policy. |
| [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | [run-records](../systems/rhodeck/containers/cli/components/run-records.md) | Save confirmed typed facts before typed-event publication, then persist the terminal outcome after cleanup. |
| [plan-workflows](../systems/rhodeck/containers/cli/components/plan-workflows.md) | [machine-output](../systems/rhodeck/containers/cli/components/machine-output.md) | Stream filtered run observations through the bounded sink; output failure triggers runtime release. |
| [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | [machine-output](../systems/rhodeck/containers/cli/components/machine-output.md) | Emit the final result after the plan service drains events and releases directory ownership. |
