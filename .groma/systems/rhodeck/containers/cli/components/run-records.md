---
type: C4 Component
title: Run records and recovery
status: stable
groma:
  id: run-records
  parent: cli
  code:
    - scanner: go
      file: packages/control/store.go
    - scanner: go
      file: packages/control/record.go
    - scanner: go
      file: packages/control/recovery.go
      symbol: recover
  group: Application runtime
  technology: Go / local JSON records
description: Persists accepted runs and preserves confirmed interruption facts
---

Defines saved run records, loads and validates them when storage opens, and persists original plan admission, actual installation metadata, task submissions and observed results. Recovery leaves terminal records unchanged when cleanup is complete or failed, or their result code is already host_interrupted. For an interrupted record, only nonterminal run and task states become unknown; confirmed terminal business states and typed facts survive. The record receives host_interrupted and cleanup unconfirmed, so an interrupted cleanup cannot erase an established business conclusion. Recovery performs no adapter preparation, worker launch, reconnect or action replay.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/store.go](../../../../../../packages/control/store.go) | [packages/control/internal/recordfs/store.go](../../../../../../packages/control/internal/recordfs/store.go) | Stores private records | Filesystem lock / atomic JSON replacement |
| [packages/control/record.go](../../../../../../packages/control/record.go) | [packages/control/plan.go](../../../../../../packages/control/plan.go) | Validates recorded plans | Go ValidatePlan |
| [packages/control/record.go](../../../../../../packages/control/record.go) | [packages/control/objective.go](../../../../../../packages/control/objective.go) | Validates recorded objectives | Go typed-result validation |
