---
type: C4 System
title: MaaCore
description: Executes native automation and emits callbacks
status: stable
groma:
  id: maacore
  technology: Native shared library / C ABI
---

Caller-provided upstream automation engine loaded inside the worker address space. It consumes separately provisioned resources, connection profiles and ADB configuration, performs device actions and reports native callback messages. The external-system boundary denotes ownership of the engine, not a separate MaaCore server process.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [maacore](maacore.md) | [native-callbacks](../systems/rhodeck/containers/worker/components/native-callbacks.md) | Emits native callbacks | C ABI callback / integer user data |
| [maacore](maacore.md) | [android-device](android-device.md) | Automates configured target | Caller-provided ADB / connection profile |
