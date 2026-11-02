# Installation metadata and version checks

Rhodeck uses the Core and resource combination supplied by the host; it imposes
no fixed Core version.
It does not install, update, discover resource origins or guarantee compatibility
with older versions. Keep the selected library, dependencies and resources
immutable for the entire worker lifetime. Revalidate after any change; a version
string alone proves neither task acceptance nor task success.

`doctor` emits `runtime_info` at the top level. A run's terminal envelope and
durable record carry it at `result.runtime_info`:

| Metadata field | Meaning |
| --- | --- |
| `snapshot_id` | Caller correlation from the plan's `initialize.snapshot_id` |
| `core_version` | Actual Core version |
| `library_sha256` | SHA256 of the main library file only |
| `resources[].root` | Ordered resource roots, each containing `resource/` |
| `resources[].version_sha256` | SHA256 of that root's `resource/version.json` only |

Initialization failure may leave no metadata. A missing resource version file
can leave `version_sha256` empty. Missing metadata provides no verification of
the corresponding installation fact. A matching snapshot ID does not establish
file integrity, and a version-file digest does not cover the resource tree.
Neither digest covers native dependencies, ADB, device state or successful
execution.

Resource origins, local modifications and overlays are caller-owned information;
the CLI does not discover them or read an installation manifest. Do not infer
resource origin from the Core version or the resource version file.

The plan may optionally set `initialize.expected_core_version` and
`initialize.expected_library_sha256` to values from this specific installation.
The worker still rejects mismatches with `snapshot_mismatch`; leaving them empty
does not select a repository-wide default version. Library hashing and
version-file hashing report facts at initialization, not continuous integrity
monitoring or permission to replace a running installation.
