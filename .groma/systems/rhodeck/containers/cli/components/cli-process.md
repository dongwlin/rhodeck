---
type: C4 Component
title: CLI process shell
status: stable
groma:
  id: cli-process
  parent: cli
  code:
    - scanner: go
      file: apps/cli/main.go
    - scanner: go
      file: apps/cli/internal/app/app.go
  group: CLI interface
  technology: Go executable
description: Owns signals, standard streams and exit status
---

Builds the command tree around process streams and a signal-aware context, executes it, and translates completion or structured failure into the documented process exit status. It delegates command behavior to the command interface and serialization to the output contract.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [apps/cli/main.go](../../../../../../apps/cli/main.go) | [apps/cli/internal/cmd/root.go](../../../../../../apps/cli/internal/cmd/root.go) | Assembles command tree | In-process Go calls |
| [apps/cli/internal/app/app.go](../../../../../../apps/cli/internal/app/app.go) | [apps/cli/internal/output/failure.go](../../../../../../apps/cli/internal/output/failure.go) | Classifies process exit | Typed Go errors |
