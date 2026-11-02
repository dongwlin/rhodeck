---
type: C4 Component
title: Plan workflows and registry
status: stable
groma:
  id: plan-workflows
  parent: cli
  code:
    - scanner: go
      file: apps/cli/internal/plan/service.go
    - scanner: go
      file: apps/cli/internal/plan/registry.go
    - scanner: go
      file: apps/cli/internal/plan/paths.go
      symbol: CheckLocalPaths
  group: CLI interface
  technology: Go
description: Composes validation, execution and the explicit adapter set
---

Owns installation/path checks and the application-facing preview, execute, doctor and capabilities workflows. Its registry explicitly supplies Copilot, OperProgress, Roguelike and SwitchTheme descriptors to control; root control never imports those concrete adapters.

Preparation occurs before execution side effects. Execute opens the runtime, drains its single event stream, obtains the final snapshot and releases directory ownership before returning. Preview shares preparation without locking storage or launching a worker. Capabilities describe local preparation and interpretation scope, not proof of device execution.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [apps/cli/internal/plan/service.go](../../../../../../apps/cli/internal/plan/service.go) | [packages/control/prepare.go](../../../../../../packages/control/prepare.go) | Prepares admitted input | Go PreparePlan / injected registry |
| [apps/cli/internal/plan/service.go](../../../../../../apps/cli/internal/plan/service.go) | [packages/control/runtime.go](../../../../../../packages/control/runtime.go) | Runs prepared plans | Go Runtime / Run API |
| [apps/cli/internal/plan/registry.go](../../../../../../apps/cli/internal/plan/registry.go) | [packages/control/adapters/copilot/input.go](../../../../../../packages/control/adapters/copilot/input.go) | Registers explicit adapter | Go descriptor registry |
| [apps/cli/internal/plan/registry.go](../../../../../../apps/cli/internal/plan/registry.go) | [packages/control/adapters/operprogress/input.go](../../../../../../packages/control/adapters/operprogress/input.go) | Registers explicit adapter | Go descriptor registry |
| [apps/cli/internal/plan/registry.go](../../../../../../apps/cli/internal/plan/registry.go) | [packages/control/adapters/roguelike/input.go](../../../../../../packages/control/adapters/roguelike/input.go) | Registers explicit adapter | Go descriptor registry |
| [apps/cli/internal/plan/registry.go](../../../../../../apps/cli/internal/plan/registry.go) | [packages/control/adapters/switchtheme/input.go](../../../../../../packages/control/adapters/switchtheme/input.go) | Registers explicit adapter | Go descriptor registry |
| [apps/cli/internal/plan/service.go](../../../../../../apps/cli/internal/plan/service.go) | [apps/cli/internal/output/writer.go](../../../../../../apps/cli/internal/output/writer.go) | Streams run observations | Injected Sink / bounded writer |
