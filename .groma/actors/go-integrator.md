---
type: C4 Actor
title: Go integrator
description: Embeds the public Go libraries
status: stable
groma:
  id: go-integrator
---

Builds host applications using control, the supervised worker client or the lower-level maa package. Owns installation paths, resource provisioning and event consumption. These are supported library integration roles; this repository implements no additional host application.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [go-integrator](go-integrator.md) | [run-lifecycle](../systems/rhodeck/containers/cli/components/run-lifecycle.md) | Embeds application runtime | Public Go API |
| [go-integrator](go-integrator.md) | [worker-supervisor](../systems/rhodeck/containers/cli/components/worker-supervisor.md) | Supervises Core workers | Public Go API |
| [go-integrator](go-integrator.md) | [maa-binding](../systems/rhodeck/containers/worker/components/maa-binding.md) | Embeds native binding | Public Go API |
