---
type: C4 Container
title: Rhodeck CLI
status: stable
groma:
  id: cli
  parent: rhodeck
  technology: Go / Cobra / local JSON files
description: Validates plans, supervises execution and emits stable machine output
---

The rhodeck-cli executable embeds the command interface, plan service, four explicit adapters, control runtime, record storage and worker supervisor. Commands expose run, dry-run, doctor, capabilities and version/help. Input comes from an explicitly supplied plan and installation paths.

A run holds ownership of its configured data directory, persists admission before launching the child, consumes worker events through control and publishes a terminal JSON result after cleanup. Dry-run prepares input without starting processes or touching devices. Doctor initializes and closes an isolated worker without connecting or submitting tasks.

The local `runtime.lock` and `runs/<run_id>.json` files are this application's storage boundary. Shared packages remain reusable by other Go hosts; placement here reflects this repository's implemented CLI composition.
