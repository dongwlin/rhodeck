---
type: C4 Component
title: Command interface
status: stable
groma:
  id: command-interface
  parent: cli
  code:
    - scanner: go
      file: apps/cli/internal/cmd/root.go
    - scanner: go
      file: apps/cli/internal/cmd/help.go
    - scanner: go
      file: apps/cli/internal/cmd/version.go
      symbol: versionCommand
    - scanner: go
      file: apps/cli/internal/cmd/doctor.go
    - scanner: go
      file: apps/cli/internal/cmd/dryrun.go
    - scanner: go
      file: apps/cli/internal/cmd/run.go
    - scanner: go
      file: apps/cli/internal/cmd/flags.go
    - scanner: go
      file: apps/cli/internal/cmd/capabilities.go
  group: CLI interface
  technology: Go / Cobra
description: Maps Cobra commands and flags onto application workflows
---

Defines run, dry-run, doctor, capabilities, version and help, validates command arguments and flags, and obtains input from a file or stdin. Delegates plan behavior to the plan service and emits the versioned machine-output envelope. It owns the CLI surface, not task success interpretation.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [apps/cli/internal/cmd/run.go](../../../../../../apps/cli/internal/cmd/run.go) | [apps/cli/internal/plan/service.go](../../../../../../apps/cli/internal/plan/service.go) | Executes plan workflows | In-process Go calls |
| [apps/cli/internal/cmd/run.go](../../../../../../apps/cli/internal/cmd/run.go) | [apps/cli/internal/output/writer.go](../../../../../../apps/cli/internal/output/writer.go) | Emits machine results | Versioned JSON / JSONL |
