---
type: C4 Container
title: Core worker
status: stable
groma:
  id: worker
  parent: rhodeck
  technology: Go / purego / protocol-v1 JSONL
description: Isolates MaaCore and exposes a bounded command session
---

A child executable launched by the parent client with stdin/stdout JSONL pipes and stderr diagnostics. One command loop owns the native engine, one runtime and one active instance. The hello handshake reports session and platform information before native initialization.

The worker validates commands, correlates accepted asynchronous operations, forwards native observations and serves bounded cached-image transfers. It retains no durable run records and does not decide application task success. The parent owns deadlines, termination and process reaping when native calls or cleanup block.

The maa binding and shared protocol are in-process Go code. Protocol source has one architecture owner here although both executables compile it; the worker supervisor uses the same package. Platform-specific source links reflect the active Linux Go scan, not complete cross-platform execution evidence.
