---
type: C4 System
title: Android device
description: Runs the automation target
status: stable
groma:
  id: android-device
  technology: Android device or emulator / ADB
---

The device or emulator at the explicitly configured connection address. MaaCore reaches it through the caller-provided ADB installation and connection profile. Rhodeck forwards configuration and actions to MaaCore; it does not implement a device agent, install ADB or manage a device fleet.
