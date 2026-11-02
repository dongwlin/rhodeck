---
type: C4 Component
title: Native ABI loading and calls
status: stable
groma:
  id: native-abi
  parent: worker
  code:
    - scanner: go
      file: packages/maa-go/internal/native/loader.go
    - scanner: go
      file: packages/maa-go/internal/native/api.go
    - scanner: go
      file: packages/maa-go/internal/native/calls.go
    - scanner: go
      file: packages/maa-go/internal/native/doc.go
    - scanner: go
      file: packages/maa-go/internal/native/errors.go
    - scanner: go
      file: packages/maa-go/internal/native/loader_unix.go
    - scanner: go
      file: packages/maa-go/internal/native/register.go
    - scanner: go
      file: packages/maa-go/internal/native/symbols.go
  group: MaaCore binding
  technology: Go / purego / OS dynamic loader
description: Loads MaaCore symbols and preserves C ABI call shapes
---

Loads an explicit absolute shared-library path, resolves required and optional symbols, registers an immutable API table and supplies width-correct C ABI wrappers plus capability snapshots. It provides no resource/ADB installer, hot reload or Dlclose-based recovery. The public binding owns lifecycle policy and input validation.

Loading resolves symbols, registers wrappers and only then publishes the API table. Missing required symbols are reported together; failed registration never publishes a partially initialized table. Missing optional symbols disable their own capability and public calls return ErrUnsupported. Version strings are not used to infer symbol support.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [packages/maa-go/internal/native/calls.go](../../../../../../packages/maa-go/internal/native/calls.go) | [maacore](../../../../../externals/maacore.md) | Invokes automation engine | purego / C ABI in worker address space |
