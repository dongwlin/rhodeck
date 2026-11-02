---
type: C4 Component
title: Native callback bridge
status: stable
groma:
  id: native-callbacks
  parent: worker
  code:
    - scanner: go
      file: packages/maa-go/internal/native/callback_registry.go
    - scanner: go
      file: packages/maa-go/internal/native/callback_linux.go
    - scanner: go
      file: packages/maa-go/internal/native/callback_types.go
  group: MaaCore binding
  technology: Go / purego callback trampoline
description: Routes native callbacks through non-reused integer tokens
---

Owns one process trampoline and an integer-token registry for callback targets. Native user data never contains a Go pointer; dispatch releases the registry lock before delivery. Dispatch passes the borrowed native pointer to the binding, which acquires the instance callback lease before bounded length scanning, then reserves queue capacity before copying the message. Platform entry points implement the native callback ABI.

Callback tokens are removed only after admission closes and in-flight callbacks drain. The Go panic boundary records callback diagnostics; it cannot contain native crashes or C++ exceptions, which require the worker's process boundary.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/maa-go/internal/native/callback_registry.go](../../../../../../packages/maa-go/internal/native/callback_registry.go) | [packages/maa-go/callback.go](../../../../../../packages/maa-go/callback.go) | Delivers callback messages | Go callback target / borrowed native pointer |
