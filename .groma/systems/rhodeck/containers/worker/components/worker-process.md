---
type: C4 Component
title: Worker process shell
status: stable
groma:
  id: worker-process
  parent: worker
  code:
    - scanner: go
      file: apps/worker/main.go
    - scanner: go
      file: apps/worker/internal/app/run.go
      symbol: Run
    - scanner: go
      file: apps/worker/internal/app/stdio_unix.go
      symbol: isolateStdout
    - scanner: go
      file: apps/worker/internal/app/stdio_linux.go
      symbol: redirectDescriptor
  group: Worker session
  technology: Go / OS-specific stdio handling
description: Keeps protocol stdout isolated from native diagnostics
---

Preserves a private protocol stdout handle, redirects ordinary Go/native stdout to stderr, establishes signal handling and runs the session. The process shell applies a bounded interrupt cleanup window; the parent supervisor independently enforces process deadlines.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [apps/worker/main.go](../../../../../../apps/worker/main.go) | [apps/worker/internal/app/session.go](../../../../../../apps/worker/internal/app/session.go) | Runs isolated session | Go process shell / private stdout |
