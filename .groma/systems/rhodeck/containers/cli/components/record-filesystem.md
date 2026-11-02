---
type: C4 Component
title: Private record filesystem
status: stable
groma:
  id: record-filesystem
  parent: cli
  code:
    - scanner: go
      file: packages/control/internal/recordfs/store.go
    - scanner: go
      file: packages/control/internal/recordfs/store_unix.go
  group: Application runtime
  technology: Go / filesystem locks / JSON files
description: Locks the data directory and atomically replaces run files
---

Owns private directory creation and path validation, `runtime.lock` acquisition/release, bounded reads and atomic replacement of `runs/<run_id>.json`. The lock protects one configured data directory; it does not reserve a device across different directories. Platform helpers implement the local locking and file-safety details.
