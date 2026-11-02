---
type: C4 Component
title: Operator progression adapter
status: stable
groma:
  id: operprogress-adapter
  parent: cli
  code:
    - scanner: go
      file: packages/control/adapters/operprogress/input.go
    - scanner: go
      file: packages/control/adapters/operprogress/doc.go
    - scanner: go
      file: packages/control/adapters/operprogress/result.go
  group: Explicit task adapters
  technology: Go / explicit control adapter
description: Compiles operator actions and tracks declared targets
---

Validates the explicit action plan and operator targets, compiles native OperProgress parameters and produces task-local result interpretation under all. Mastery startup facts do not establish that training has finished; unsupported or absent completion evidence stays unconfirmed.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/adapters/operprogress/input.go](../../../../../../packages/control/adapters/operprogress/input.go) | [packages/control/internal/strictjson/object.go](../../../../../../packages/control/internal/strictjson/object.go) | Validates typed fields | Exact JSON parsing |
