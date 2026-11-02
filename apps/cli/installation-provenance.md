# Caller-owned installation provenance

Rhodeck validates the latest Core and resource combination supplied by the host.
It does not install, update, discover resource origins or guarantee compatibility
with older versions. Keep the selected library, dependencies and resources
immutable for the entire worker lifetime. Revalidate after any change; a version
string alone proves neither task acceptance nor task success.

The [manifest template](installation-manifest.example.json) is a host-owned
acceptance record, not an additional CLI input or a manifest discovered by the
worker. Copy it outside the repository, replace every placeholder and repeat the
resource row for each `initialize.resource_roots` entry, preserving order. Do not
submit private installation paths or runtime records to the repository.

| Manifest field | CLI evidence |
| --- | --- |
| `snapshot_id` | `runtime_info.snapshot_id`; must also equal the plan's `initialize.snapshot_id` |
| `core_version` | Actual `runtime_info.core_version` |
| `library_sha256` | Actual `runtime_info.library_sha256`, SHA256 of the main library file only |
| `resources[].root` | Exact ordered `runtime_info.resources[].root`, the root containing `resource/` |
| `resources[].version_sha256` | SHA256 of that root's `resource/version.json` only |
| `library_source`, `resources[].source` | Host evidence; no CLI discovery of these fields |
| `version_digest_coverage`, `additional_digests` | Host description of digest algorithm and covered paths |

`doctor` emits `runtime_info` at the top level. A run's terminal envelope carries
it at `result.runtime_info`; the private durable record uses the same
`result.runtime_info`. Initialization failure may leave no metadata. Missing
metadata or a missing `version_sha256` is unverified, never a successful match.
Keep the plan, doctor report and final run record associated with the manifest;
a matching snapshot ID is caller correlation, not proof that a tree was hashed.

For each source, set `status` to `recorded` only after recording the repository
or download URL and its version, immutable commit or release reference. Record
local modifications and overlays separately, with their actual origins and
coverage. Unknown origins stay `unrecorded` (or `unverified` if a claimed origin
cannot be checked) and do not pass provenance acceptance. The blank template
deliberately represents incomplete evidence. Do not infer resource origin from
the Core version or from the resource version file.

The version-file digest is not a resource-tree digest. Optional additional
digests must each name `algorithm`, `value` and `paths` relative to the resource
root, with a documented enumeration/serialization rule for a directory digest.
A full-tree digest is not required. No library or version-file digest covers
native dependencies, ADB, task support, device state or successful execution.

## Reconciliation example

For a chosen installation, run `rhodeck-cli doctor --input plan.json --worker
/absolute/worker --data-dir /absolute/control-data > doctor.json`, recording
its exit status separately. Fill the host manifest from observed metadata and
independently recorded origins. These checks compare the manifest with a doctor
report, a terminal envelope or a saved record:

```sh
python3 - manifest.json doctor.json <<'PY'
import json
import re
import sys

manifest, evidence = [json.load(open(p, encoding="utf-8")) for p in sys.argv[1:]]
info = evidence.get("runtime_info") or evidence.get("result", {}).get("runtime_info")
def require(ok, message):
    if not ok:
        raise SystemExit(message)

require(info is not None, "UNVERIFIED: no runtime metadata")
require(manifest.get("manifest_schema_version") == 1, "unsupported manifest schema")
for key in ("snapshot_id", "core_version", "library_sha256"):
    require(bool(info.get(key)) and manifest.get(key) == info[key], "mismatch: " + key)
require(re.fullmatch(r"[0-9a-f]{64}", info["library_sha256"]), "invalid library digest")
resources = manifest["resources"]
require(bool(resources) and len(resources) == len(info["resources"]), "resource count mismatch")
for expected, actual in zip(resources, info["resources"]):
    require(expected["root"] == actual["root"], "resource root/order mismatch")
    digest = actual.get("version_sha256", "")
    require(re.fullmatch(r"[0-9a-f]{64}", digest) and expected["version_sha256"] == digest,
            "UNVERIFIED: missing or mismatched version-file digest")
    require(expected["version_digest_coverage"] ==
            {"algorithm": "sha256", "paths": ["resource/version.json"]}, "wrong digest coverage")
for source in [manifest.get("library_source")] + [r.get("source") for r in resources]:
    require(isinstance(source, dict) and source.get("status") == "recorded" and
            source.get("reference") and (source.get("repository") or source.get("download_url")),
            "UNVERIFIED: origin not recorded")
print("Metadata matches; independently check the recorded origin evidence and command results.")
PY
```

Use the same comparison for the run's terminal JSON object and durable record.
Compare `snapshot_id` and resource roots against the submitted plan as well.
This example checks field agreement and that origins are recorded; it cannot
authenticate an origin or prove successful doctor cleanup, task semantics or
device acceptance. Keep those results as separate evidence layers.

The plan may optionally set `initialize.expected_core_version` and
`initialize.expected_library_sha256` to values from this specific installation.
The worker still rejects mismatches with `snapshot_mismatch`; leaving them empty
does not select a repository-wide default version. Library hashing and
version-file hashing report facts at initialization, not continuous integrity
monitoring or permission to replace a running installation.
