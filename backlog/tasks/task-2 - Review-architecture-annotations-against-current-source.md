---
id: TASK-2
title: Review architecture annotations against current source
status: Done
assignee:
  - '@codex'
created_date: '2026-11-02T14:16:00Z'
updated_date: '2026-11-02T14:37:00Z'
labels: []
dependencies: []
references:
  - rhodeck
  - run-records
  - run-lifecycle
  - machine-output
  - plan-workflows
  - plan-preparation
  - typed-objectives
  - copilot-adapter
  - roguelike-adapter
  - worker-session
  - worker-supervisor
  - worker-protocol
  - native-engine
  - maa-binding
  - native-abi
  - native-callbacks
  - stop-a-run-and-recover-interrupted-records
  - run-and-interpret-an-explicit-task-batch
modified_files:
  - .groma/systems/rhodeck/system.md
  - .groma/systems/rhodeck/containers/cli/components/run-records.md
  - .groma/systems/rhodeck/containers/cli/components/run-lifecycle.md
  - .groma/systems/rhodeck/containers/cli/components/machine-output.md
  - .groma/systems/rhodeck/containers/cli/components/copilot-adapter.md
  - .groma/systems/rhodeck/containers/cli/components/roguelike-adapter.md
  - .groma/systems/rhodeck/containers/worker/components/worker-session.md
  - .groma/systems/rhodeck/containers/cli/components/worker-supervisor.md
  - .groma/systems/rhodeck/containers/worker/components/worker-protocol.md
  - .groma/systems/rhodeck/containers/worker/components/maa-binding.md
  - .groma/systems/rhodeck/containers/worker/components/native-abi.md
  - .groma/systems/rhodeck/containers/worker/components/native-callbacks.md
  - .groma/systems/rhodeck/containers/cli/components/plan-preparation.md
  - .groma/systems/rhodeck/containers/cli/components/plan-workflows.md
  - .groma/systems/rhodeck/containers/worker/components/native-engine.md
  - .groma/flows/stop-a-run-and-recover-interrupted-records.md
  - .groma/flows/run-and-interpret-an-explicit-task-batch.md
type: docs
ordinal: 2000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A follow-up source review of the curated Groma map found overbroad recovery wording and missing collaborations around observation delivery, output cleanup and typed-result validation. Readers need the map to distinguish confirmed business outcomes from interrupted cleanup and to follow these existing interactions.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Architecture annotations and relationships agree with the current implementation, retaining meaningful component boundaries and existing useful descriptions.
- [x] #2 Task references and changed-file links resolve; rendered map, source ownership and rescan stability are checked with any scanner limitations recorded.
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Compare the existing map and scenario flows with source across CLI/control, worker/protocol and native binding. 2. Preserve the established boundaries unless source supports a responsibility combine; correct recovery wording and add verified missing collaboration/overview details through Groma CLI. 3. Keep Backlog references and modified-file lists current after each change. 4. Inspect the map and source links, validate local links/ownership and run two consecutive stable scans.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Compared current CLI/control, worker/protocol and maa/native source with the existing curated map using three independent domain reviews. Kept the two executable containers, 22 responsibility components and existing multi-file combinations/groups: source did not justify another merge. Changed 17 architecture documents through Groma CLI only: enriched/corrected 12 overviews, added six file-addressed relationships and updated execution plus stop/recovery flows. Corrections distinguish terminal business results from interrupted cleanup, instance serialization from Runtime query/logging admission, and callback length scanning from quota reservation/copying. Backlog references and the complete modified-file list were updated immediately after every architecture write.

Verification: inspected the rendered local C4 map (actors west, external systems east), current recovery prose, the new native-engine-to-session relationship, How it is built and a source link, and both task links on the affected component. Checked all architecture/source links, 86 unique source owners, descriptions/overviews and TASK-1/TASK-2 references; no unresolved links or duplicate ownership. Two consecutive scans of the final text each created 0/refreshed 22 and left all 35 architecture/config files byte-identical. git diff --check and explicit whitespace checks of changed untracked documents passed. Application source is unchanged; the pre-existing AGENTS.md edit was preserved.

Limits: groma lint exits 1 with 19 possible duplicate groups; reviewed as already-combined helpers or distinct responsibilities, not merge instructions. The Go scanner observes linux/amd64 with cgo, omits other-platform files and incompletely resolves external packages/dynamic dispatch; those pre-existing coverage limits remain documented in project.md. No Go build, vet, test, race, real MaaCore, device or cross-platform execution was run for this documentation-only update. BrowserSkill could not connect because its CLI protocol is older than the extension; the rendered review succeeded through the app browser instead.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Corrected recovery and binding-lifecycle semantics, expanded missing collaboration details, added six source-backed relationships and refreshed two scenario flows through Groma CLI. Preserved the existing meaningful component combinations. All task/source links and ownership checks passed, the rendered map was reviewed, and two consecutive final scans changed no architecture files. No application code changed; scanner/platform and runtime-verification limits are recorded in the implementation notes.
<!-- SECTION:FINAL_SUMMARY:END -->
