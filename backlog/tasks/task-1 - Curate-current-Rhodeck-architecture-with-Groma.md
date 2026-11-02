---
id: TASK-1
title: Curate current Rhodeck architecture with Groma
status: Done
assignee:
  - '@codex'
created_date: '2026-11-02T13:35:00Z'
updated_date: '2026-11-02T14:04:00Z'
labels: []
dependencies: []
references:
  - rhodeck
  - cli-process
  - command-interface
  - plan-workflows
  - machine-output
  - plan-preparation
  - run-lifecycle
  - typed-objectives
  - run-records
  - record-filesystem
  - strict-json
  - copilot-adapter
  - operprogress-adapter
  - roguelike-adapter
  - switchtheme-adapter
  - worker-supervisor
  - worker-protocol
  - worker-process
  - worker-session
  - native-engine
  - maa-binding
  - native-abi
  - native-callbacks
  - android-device
  - cli
  - go-integrator
  - maacore
  - operator
  - preview-an-explicit-task-plan
  - run-and-interpret-an-explicit-task-batch
  - stop-a-run-and-recover-interrupted-records
  - worker
modified_files:
  - .groma/config.json
  - .groma/systems/rhodeck/components/imagefixture-fixture.md
  - .groma/systems/rhodeck/components/queryfixture-fixture.md
  - .groma/systems/rhodeck/components/asyncfixture-fixture.md
  - .groma/systems/rhodeck/components/signaled-unix.md
  - .groma/systems/rhodeck/components/ffitest.md
  - .groma/systems/rhodeck/components/loaderfixture-fixture.md
  - .groma/systems/rhodeck/components/callbackfixture-fixture.md
  - .groma/systems/rhodeck/components/abifixture-fixture.md
  - .groma/systems/rhodeck/components/dependency.md
  - .groma/systems/rhodeck/components/platformfixture-fixture.md
  - .groma/systems/rhodeck/components/caller-positive.md
  - .groma/systems/rhodeck/components/extra-positive.md
  - .groma/systems/rhodeck/components/asstport.md
  - .groma/.groma.lock
  - .groma/systems/rhodeck/components/asstcallerextra.md
  - .groma/systems/rhodeck/components/port-positive.md
  - .groma/systems/rhodeck/components/asstcaller.md
  - .groma/systems/rhodeck/containers/maa-go-check/components/layers.md
  - .groma/systems/rhodeck/containers/maa-go-check/components/gotest.md
  - >-
    .groma/systems/rhodeck/containers/maa-go-check/components/maa-go-check-main.md
  - .groma/systems/rhodeck/containers/maa-go-check/components/checker.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/golink.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/compare.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/cparse.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/goextract.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/goselect.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/cinput.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/ctype.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/cextract.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/env.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/diag.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/expect.md
  - >-
    .groma/systems/rhodeck/containers/check-maa-api/components/check-maa-api-main.md
  - .groma/systems/rhodeck/containers/check-maa-api/components/profile.md
  - .groma/systems/rhodeck/containers/api-coverage/components/check.md
  - >-
    .groma/systems/rhodeck/containers/api-coverage/components/api-coverage-main.md
  - .groma/systems/rhodeck/containers/api-coverage/components/refs.md
  - .groma/systems/rhodeck/containers/api-coverage/components/scan.md
  - .groma/systems/rhodeck/containers/api-coverage/components/inventory.md
  - .groma/systems/rhodeck/containers/api-coverage/container.md
  - .groma/systems/rhodeck/containers/check-maa-api/container.md
  - .groma/systems/rhodeck/containers/maa-go-check/container.md
  - .groma/systems/rhodeck/containers/cli/components/app.md
  - .groma/systems/rhodeck/containers/cli/components/cli-main.md
  - .groma/systems/rhodeck/containers/cli/components/cli-process.md
  - .groma/systems/rhodeck/containers/cli/components/capabilities.md
  - .groma/systems/rhodeck/containers/cli/components/cmd-doctor.md
  - .groma/systems/rhodeck/containers/cli/components/cmd-run.md
  - .groma/systems/rhodeck/containers/cli/components/dryrun.md
  - .groma/systems/rhodeck/containers/cli/components/flags.md
  - .groma/systems/rhodeck/containers/cli/components/help.md
  - .groma/systems/rhodeck/containers/cli/components/root.md
  - .groma/systems/rhodeck/containers/cli/components/version.md
  - .groma/systems/rhodeck/containers/cli/components/command-interface.md
  - .groma/systems/rhodeck/containers/cli/components/paths.md
  - .groma/systems/rhodeck/containers/cli/components/registry.md
  - .groma/systems/rhodeck/containers/cli/components/service.md
  - .groma/systems/rhodeck/containers/cli/components/plan-workflows.md
  - .groma/systems/rhodeck/containers/cli/components/envelope.md
  - .groma/systems/rhodeck/containers/cli/components/failure.md
  - .groma/systems/rhodeck/containers/cli/components/writer.md
  - .groma/systems/rhodeck/containers/cli/components/machine-output.md
  - .groma/systems/rhodeck/containers/cli/components/adapter.md
  - .groma/systems/rhodeck/containers/cli/components/plan.md
  - .groma/systems/rhodeck/containers/cli/components/prepare.md
  - .groma/systems/rhodeck/containers/cli/components/plan-preparation.md
  - .groma/systems/rhodeck/containers/cli/components/control-doctor.md
  - .groma/systems/rhodeck/containers/cli/components/control-run.md
  - .groma/systems/rhodeck/containers/cli/components/control-runtime.md
  - .groma/systems/rhodeck/containers/cli/components/events.md
  - .groma/systems/rhodeck/containers/cli/components/execute.md
  - .groma/systems/rhodeck/containers/cli/components/observe.md
  - .groma/systems/rhodeck/containers/cli/components/stop.md
  - .groma/systems/rhodeck/containers/cli/components/types.md
  - .groma/systems/rhodeck/containers/cli/components/run-lifecycle.md
  - .groma/systems/rhodeck/containers/cli/components/objective.md
  - .groma/systems/rhodeck/containers/cli/components/typed-objectives.md
  - .groma/systems/rhodeck/containers/cli/components/control-store.md
  - .groma/systems/rhodeck/containers/cli/components/record.md
  - .groma/systems/rhodeck/containers/cli/components/recovery.md
  - .groma/systems/rhodeck/containers/cli/components/run-records.md
  - .groma/systems/rhodeck/containers/cli/components/recordfs-store.md
  - .groma/systems/rhodeck/containers/cli/components/store-unix.md
  - .groma/systems/rhodeck/containers/cli/components/record-filesystem.md
  - .groma/systems/rhodeck/containers/cli/components/object.md
  - .groma/systems/rhodeck/containers/cli/components/value.md
  - .groma/systems/rhodeck/containers/cli/components/strict-json.md
  - .groma/systems/rhodeck/containers/cli/components/copilot-doc.md
  - .groma/systems/rhodeck/containers/cli/components/copilot-input.md
  - .groma/systems/rhodeck/containers/cli/components/copilot-result.md
  - .groma/systems/rhodeck/containers/cli/components/copilot-adapter.md
  - .groma/systems/rhodeck/containers/cli/components/operprogress-doc.md
  - .groma/systems/rhodeck/containers/cli/components/operprogress-input.md
  - .groma/systems/rhodeck/containers/cli/components/operprogress-result.md
  - .groma/systems/rhodeck/containers/cli/components/operprogress-adapter.md
  - .groma/systems/rhodeck/containers/cli/components/roguelike-doc.md
  - .groma/systems/rhodeck/containers/cli/components/roguelike-input.md
  - .groma/systems/rhodeck/containers/cli/components/roguelike-result.md
  - .groma/systems/rhodeck/containers/cli/components/roguelike-adapter.md
  - .groma/systems/rhodeck/containers/cli/components/switchtheme-doc.md
  - .groma/systems/rhodeck/containers/cli/components/switchtheme-input.md
  - .groma/systems/rhodeck/containers/cli/components/switchtheme-result.md
  - .groma/systems/rhodeck/containers/cli/components/switchtheme-adapter.md
  - .groma/systems/rhodeck/containers/cli/components/client.md
  - .groma/systems/rhodeck/containers/cli/components/worker-supervisor.md
  - .groma/systems/rhodeck/components/codec.md
  - .groma/systems/rhodeck/components/contract.md
  - .groma/systems/rhodeck/components/protocol.md
  - .groma/systems/rhodeck/components/queue.md
  - .groma/systems/rhodeck/components/worker-protocol.md
  - .groma/systems/rhodeck/containers/worker/components/worker-protocol.md
  - .groma/systems/rhodeck/containers/worker/components/app-run.md
  - .groma/systems/rhodeck/containers/worker/components/stdio-linux.md
  - .groma/systems/rhodeck/containers/worker/components/stdio-unix.md
  - .groma/systems/rhodeck/containers/worker/components/worker-main.md
  - .groma/systems/rhodeck/containers/worker/components/worker-process.md
  - .groma/systems/rhodeck/containers/worker/components/image.md
  - .groma/systems/rhodeck/containers/worker/components/session.md
  - .groma/systems/rhodeck/containers/worker/components/worker-session.md
  - .groma/systems/rhodeck/containers/worker/components/backend.md
  - .groma/systems/rhodeck/containers/worker/components/native-engine.md
  - .groma/systems/rhodeck/containers/worker/components/buffer.md
  - .groma/systems/rhodeck/containers/worker/components/callback.md
  - .groma/systems/rhodeck/containers/worker/components/dispatch.md
  - .groma/systems/rhodeck/containers/worker/components/event.md
  - .groma/systems/rhodeck/containers/worker/components/ids.md
  - .groma/systems/rhodeck/containers/worker/components/instance-observation.md
  - .groma/systems/rhodeck/containers/worker/components/instance.md
  - .groma/systems/rhodeck/containers/worker/components/maa-go-doc.md
  - .groma/systems/rhodeck/containers/worker/components/maa-go-errors.md
  - .groma/systems/rhodeck/containers/worker/components/maa-go-input.md
  - .groma/systems/rhodeck/containers/worker/components/maa-go-runtime.md
  - .groma/systems/rhodeck/containers/worker/components/operation.md
  - .groma/systems/rhodeck/containers/worker/components/query.md
  - .groma/systems/rhodeck/containers/worker/components/snapshot.md
  - .groma/systems/rhodeck/containers/worker/components/state.md
  - .groma/systems/rhodeck/containers/worker/components/window.md
  - .groma/systems/rhodeck/containers/worker/components/maa-binding.md
  - .groma/systems/rhodeck/containers/worker/components/api.md
  - .groma/systems/rhodeck/containers/worker/components/calls.md
  - .groma/systems/rhodeck/containers/worker/components/loader-unix.md
  - .groma/systems/rhodeck/containers/worker/components/loader.md
  - .groma/systems/rhodeck/containers/worker/components/native-doc.md
  - .groma/systems/rhodeck/containers/worker/components/native-errors.md
  - .groma/systems/rhodeck/containers/worker/components/register.md
  - .groma/systems/rhodeck/containers/worker/components/symbols.md
  - .groma/systems/rhodeck/containers/worker/components/native-abi.md
  - .groma/systems/rhodeck/containers/worker/components/callback-linux.md
  - .groma/systems/rhodeck/containers/worker/components/callback-registry.md
  - .groma/systems/rhodeck/containers/worker/components/callback-types.md
  - .groma/systems/rhodeck/containers/worker/components/native-callbacks.md
  - .groma/actors/operator.md
  - .groma/index.md
  - .groma/actors/go-integrator.md
  - .groma/externals/maacore.md
  - .groma/externals/android-device.md
  - .groma/systems/rhodeck/system.md
  - .groma/systems/rhodeck/containers/cli/container.md
  - .groma/systems/rhodeck/containers/worker/container.md
  - .groma/project.md
  - .groma/flows/preview-an-explicit-task-plan.md
  - .groma/flows/run-and-interpret-an-explicit-task-batch.md
  - .groma/flows/stop-a-run-and-recover-interrupted-records.md
type: docs
ordinal: 1000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The initial Groma scan exposes source files without explaining current product responsibilities or runtime boundaries. New readers need an evidence-based map of the existing CLI, worker, embedded libraries, external dependencies and persisted run records.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Architecture reflects current users, runtime and storage boundaries, meaningful components, and source-backed interactions without changing application code.
- [x] #2 Rendered map and source links are reviewed and two consecutive rescans preserve curated ownership and groups; coverage limits are documented.
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Inspect source and existing scan, excluding development/test tooling. 2. Settle current container and component ownership through Groma CLI. 3. Describe responsibilities and add verified interactions and flows. 4. Inspect rendered map/source links, run two rescans and review final scope.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Curated through Groma CLI only: one system, two executable containers, 22 responsibility components, five groups, two actors, two external systems and three current scenario flows. Excluded development tools/test fixtures, detached their stale scan ownership and removed the empty architecture records without deleting source files. Shared libraries remain embedded under the actual executable composition; local run-record storage is explained explicitly.

Verification: reviewed source-backed descriptions/relations with independent readers; inspected rendered layout (actors west, externals east), scenario navigation, How it is built and a source viewer. Ownership/link/whitespace audit passed: 86 product source references, no duplicate owners, broken links or undescribed elements. Two consecutive final scans each reported created 0/refreshed 22 and changed no architecture files. git diff --check passed; tracked application source is unchanged and the pre-existing AGENTS.md edit was preserved.

Limits: Go scanner uses linux/amd64 with cgo, omits nine other-platform implementation files, and leaves imported dependencies/dynamic dispatch incompletely resolved. These limits are documented; authored interactions were manually checked. groma lint reported 19 possible duplicate groups (exit 1); reviewed as implementation similarities with distinct layer/semantics, requiring no architecture merge. No Go build/test/race, real MaaCore or device/platform execution was performed because this task changes architecture documentation/configuration only.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Replaced the unannotated source-shaped scan with the current CLI/worker architecture, coherent responsibility components, users/external systems, verified interactions and three flows. Rendered map/source navigation, exact ownership, links and whitespace were checked; two consecutive rescans were byte-stable. Scanner/platform omissions and unresolved inference are documented. Application code is unchanged.
<!-- SECTION:FINAL_SUMMARY:END -->
