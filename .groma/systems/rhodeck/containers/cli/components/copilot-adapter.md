---
type: C4 Component
title: Copilot adapter
status: stable
groma:
  id: copilot-adapter
  parent: cli
  code:
    - scanner: go
      file: packages/control/adapters/copilot/input.go
    - scanner: go
      file: packages/control/adapters/copilot/doc.go
    - scanner: go
      file: packages/control/adapters/copilot/result.go
  group: Explicit task adapters
  technology: Go / explicit control adapter
description: Compiles Copilot tasks and interprets supported settlement facts
---

Strictly validates Copilot input and compiles native parameters while preserving filenames and list order, canonicalizing JSON and assigning deterministic omitted list IDs. List inputs declare per-entry settlement targets under all; single-file and loop inputs use none and provide no typed objective result. Only associated task callbacks can settle supported targets.

Its optional UnassociatedObservationClassifier recognizes exact taskid=0 progress without creating target facts or changing reducer scope. This does not relax the association required for execution or settlement evidence.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/adapters/copilot/input.go](../../../../../../packages/control/adapters/copilot/input.go) | [packages/control/internal/strictjson/object.go](../../../../../../packages/control/internal/strictjson/object.go) | Validates typed fields | Exact JSON parsing |
