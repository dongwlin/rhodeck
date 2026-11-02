---
type: Groma Project
title: Rhodeck architecture
groma:
  profile: architecture
description: Current CLI, supervised Core worker and embeddable Go libraries
---

This map describes the current implemented product. Start with Rhodeck for users and boundaries, then inspect the CLI and Core worker components and the supported execution flows. The private run-record directory is documented in the system and storage components.

Development tools, test fixtures and test support packages are deliberately excluded. The Go scanner observes the active Linux build: Windows, Darwin and other build-constrained source files are not all represented by Code links. Platform-specific implementations were read when identifying responsibilities, but this map does not claim real MaaCore or device execution on any platform. No application source was changed by curation.

Go scanner evidence uses a linux/amd64 build context with cgo and excludes tests. Imported dependencies and dynamic dispatch are not fully resolved by the scanner; the authored interactions were checked against the implementation. Platform source omissions and duplicate-logic suggestions do not establish runtime support or require changing product boundaries.
