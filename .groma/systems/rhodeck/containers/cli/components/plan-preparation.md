---
type: C4 Component
title: Plan admission and preparation
status: stable
groma:
  id: plan-preparation
  parent: cli
  code:
    - scanner: go
      file: packages/control/plan.go
    - scanner: go
      file: packages/control/prepare.go
    - scanner: go
      file: packages/control/adapter.go
  group: Application runtime
  technology: Go / json.RawMessage
description: Validates plans and freezes raw or typed task input
---

Validates the plan contract and prepares task specifications before worker/device operations. Raw tasks preserve open MaaCore task types, unknown fields and integer precision. Explicit adapter descriptors compile stricter task-specific input, immutable targets and task-local result reducers. PreparedPlan contains compiled parameters; Runtime and Run separately retain the admitted source plan for run records.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/plan.go](../../../../../../packages/control/plan.go) | [packages/control/internal/strictjson/object.go](../../../../../../packages/control/internal/strictjson/object.go) | Validates plan fields | Exact JSON parsing |
| [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | [packages/control/adapters/copilot/input.go](../../../../../../packages/control/adapters/copilot/input.go) | Compiles typed task input | Injected Go Prepare callback |
| [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | [packages/control/adapters/operprogress/input.go](../../../../../../packages/control/adapters/operprogress/input.go) | Compiles typed task input | Injected Go Prepare callback |
| [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | [packages/control/adapters/roguelike/input.go](../../../../../../packages/control/adapters/roguelike/input.go) | Compiles typed task input | Injected Go Prepare callback |
| [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | [packages/control/adapters/switchtheme/input.go](../../../../../../packages/control/adapters/switchtheme/input.go) | Compiles typed task input | Injected Go Prepare callback |
| [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | [packages/control/objective.go](../../../../../../packages/control/objective.go) | Validates prepared objectives | Go typed-result validation |
