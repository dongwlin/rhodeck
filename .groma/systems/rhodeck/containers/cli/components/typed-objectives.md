---
type: C4 Component
title: Typed objective aggregation
status: stable
groma:
  id: typed-objectives
  parent: cli
  code:
    - scanner: go
      file: packages/control/objective.go
  group: Application runtime
  technology: Go
description: Merges bounded immutable target facts into objective outcomes
---

Checks adapter result updates against the prepared target set, preserves immutable facts and aggregates the declared all or any policy. Tasks declaring none bypass typed-result preparation and reduction. It supports run-level result interpretation without treating callback presence, submission acceptance or partially observed targets as success.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/control/objective.go](../../../../../../packages/control/objective.go) | [packages/control/internal/strictjson/value.go](../../../../../../packages/control/internal/strictjson/value.go) | Validates immutable facts | Exact JSON parsing |
