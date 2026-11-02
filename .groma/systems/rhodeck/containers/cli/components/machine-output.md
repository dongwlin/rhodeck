---
type: C4 Component
title: Machine output contract
status: stable
groma:
  id: machine-output
  parent: cli
  code:
    - scanner: go
      file: apps/cli/internal/output/envelope.go
    - scanner: go
      file: apps/cli/internal/output/failure.go
    - scanner: go
      file: apps/cli/internal/output/writer.go
  group: CLI interface
  technology: Go / versioned JSON and JSONL
description: Serializes command facts and stable failures
---

Owns output envelopes, error schemas, stable error codes and exit-code classification; wraps the domain events and results defined by control. Help text and machine JSON share one bounded writer queue; diagnostics use stderr. Run execution configures a deadline for output writes. Blocked or broken stdout cancels execution and triggers runtime cleanup even while output cannot progress. After a write failure no further error or terminal envelope is attempted on that stdout. Errors are classified through typed/domain contracts rather than matching diagnostic text.
