---
type: C4 Component
title: Strict JSON primitives
status: stable
groma:
  id: strict-json
  parent: cli
  code:
    - scanner: go
      file: packages/control/internal/strictjson/object.go
    - scanner: go
      file: packages/control/internal/strictjson/value.go
  group: Application runtime
  technology: Go / encoding/json
description: Validates typed JSON while preserving integer precision
---

Provides object/field and exact-value parsing for control contracts and explicit adapters. Rejects malformed or unsupported typed input without a map[string]any float64 round-trip. Raw task parameters, native callback messages and connection extras retain their intentionally open contracts.
