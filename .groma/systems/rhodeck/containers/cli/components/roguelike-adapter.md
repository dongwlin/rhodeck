---
type: C4 Component
title: Roguelike adapter
status: stable
groma:
  id: roguelike-adapter
  parent: cli
  code:
    - scanner: go
      file: packages/control/adapters/roguelike/input.go
    - scanner: go
      file: packages/control/adapters/roguelike/doc.go
    - scanner: go
      file: packages/control/adapters/roguelike/result.go
  group: Explicit task adapters
  technology: Go / explicit control adapter
description: Compiles theme/mode options and interprets scoped objectives
---

Validates explicit theme, mode and scoped options, then prepares native Roguelike parameters and supported objective targets. The input determines all, any or none semantics. Unsupported terminal evidence remains unobserved rather than being promoted into success.

Its optional UnassociatedObservationClassifier recognizes exact taskid=0 progress without establishing an objective fact, selecting execution scope or weakening task association. Only supported, associated evidence can settle declared targets.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/adapters/roguelike/input.go](../../../../../../packages/control/adapters/roguelike/input.go) | [packages/control/internal/strictjson/object.go](../../../../../../packages/control/internal/strictjson/object.go) | Validates typed fields | Exact JSON parsing |
