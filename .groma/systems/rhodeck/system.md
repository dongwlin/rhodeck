---
type: C4 System
title: Rhodeck
status: stable
groma:
  id: rhodeck
  technology: Go libraries and CLI / JSONL worker IPC
description: Runs MaaCore automation with observable outcomes and process isolation
---

Rhodeck gives operators a standalone CLI and gives Go integrators an embeddable control runtime, supervised worker client and low-level MaaCore binding. The implemented applications are rhodeck-cli and a separately launched Core worker. Libraries shown inside those applications are compiled into the process that uses them; they are not additional services.

The CLI validates and prepares plans, owns run identity and business conclusions, persists records, and supervises the worker. Of the repository executables, only the worker loads the caller-provided MaaCore shared library. MaaCore, resources, ADB and the device are provisioned outside Rhodeck.

Storage is a private local data directory: `runtime.lock` coordinates ownership of that directory and `runs/<run_id>.json` records accepted plans, installation facts and confirmed outcomes. This is file storage, not a database service or a cross-directory device lock. Recovery changes nonterminal run and task states to unknown while preserving confirmed facts. If business completion was already recorded but cleanup was interrupted, the terminal business state is retained and cleanup is marked unconfirmed with host_interrupted. Reopening storage does not resume or replay device actions.

Acceptance, an async/task ID, an IPC response or Running=false never proves business success. Control interprets Core callback evidence; missing confirmation remains unknown. There is no scheduler, daemon, HTTP API, automatic reconnect or action replay in the current product.
