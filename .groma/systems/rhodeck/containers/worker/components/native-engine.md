---
type: C4 Component
title: Native engine adapter
status: stable
groma:
  id: native-engine
  parent: worker
  code:
    - scanner: go
      file: apps/worker/internal/app/backend.go
  group: Worker session
  technology: Go / maa package
description: Translates worker commands and observations to the maa API
---

Initializes the explicit MaaCore installation, checks the expected library SHA256 and Core version when supplied, hashes resource version metadata when available, opens the binding and creates one instance. It does not verify a complete resource tree or host installation provenance. Implements the session engine/operation interfaces for task, connection, query, action and cached-image commands. Forwards Events, operation settlements, faults and terminal diagnostics without deriving business success.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [apps/worker/internal/app/backend.go](../../../../../../apps/worker/internal/app/backend.go) | [packages/maa-go/runtime.go](../../../../../../packages/maa-go/runtime.go) | Uses native runtime | Go maa Runtime / Instance / Operation APIs |
| [apps/worker/internal/app/backend.go](../../../../../../apps/worker/internal/app/backend.go) | [apps/worker/internal/app/session.go](../../../../../../apps/worker/internal/app/session.go) | Publishes native observations | Go eventEmitter callback |
