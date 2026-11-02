---
type: C4 Component
title: Theme selection adapter
status: stable
groma:
  id: switchtheme-adapter
  parent: cli
  code:
    - scanner: go
      file: packages/control/adapters/switchtheme/input.go
    - scanner: go
      file: packages/control/adapters/switchtheme/doc.go
    - scanner: go
      file: packages/control/adapters/switchtheme/result.go
  group: Explicit task adapters
  technology: Go / explicit control adapter
description: Prepares one theme target and interprets confirmation evidence
---

Strictly compiles SwitchTheme input, validates candidate themes and declares one target under all. MaaCore selects the random candidate; the adapter interprets the observed selection. Already-set evidence can satisfy that objective; a confirmation callback alone remains incomplete and does not establish that the final theme state changed.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/adapters/switchtheme/input.go](../../../../../../packages/control/adapters/switchtheme/input.go) | [packages/control/internal/strictjson/object.go](../../../../../../packages/control/internal/strictjson/object.go) | Validates typed fields | Exact JSON parsing |
