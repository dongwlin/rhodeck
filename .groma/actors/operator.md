---
type: C4 Actor
title: Operator
description: Runs and inspects automation plans
status: stable
groma:
  id: operator
---

Provides an explicit worker executable, MaaCore installation, resources, ADB connection and plan. Uses the CLI to preview input, diagnose installation compatibility, execute a task batch and inspect machine-readable outcomes.

## Relationships

| Source | Target | Description | Technology |
| --- | --- | --- | --- |
| [operator](operator.md) | [command-interface](../systems/rhodeck/containers/cli/components/command-interface.md) | Runs explicit plans | CLI arguments / stdin |
