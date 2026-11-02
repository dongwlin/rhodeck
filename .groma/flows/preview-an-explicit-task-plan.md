---
type: Groma Flow
title: Preview an explicit task plan
groma:
  id: preview-an-explicit-task-plan
---

The operator previews an OperProgress plan through the same deterministic preparation used by execution. No worker, native loading, directory lock or device action is required.

## Steps

| From | To | Action |
| --- | --- | --- |
| [operator](../actors/operator.md) | [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | Supply dry-run with explicit plan input and installation paths. |
| [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | [plan-workflows](../systems/rhodeck/containers/cli/components/plan-workflows.md) | Load input and select preview workflow. |
| [plan-workflows](../systems/rhodeck/containers/cli/components/plan-workflows.md) | [plan-preparation](../systems/rhodeck/containers/cli/components/plan-preparation.md) | Validate the plan and prepare tasks through the supplied adapter registry. |
| [plan-preparation](../systems/rhodeck/containers/cli/components/plan-preparation.md) | [operprogress-adapter](../systems/rhodeck/containers/cli/components/operprogress-adapter.md) | Compile strict operator actions, native parameters and objective targets. |
| [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | [machine-output](../systems/rhodeck/containers/cli/components/machine-output.md) | Return the prepared-plan preview in the machine-output envelope. |
