# Input documents for installed operations

These examples describe existing public inputs. Check actual installed help,
capabilities and MCP tools/list first; replace every selected identity from
authorized discovery or accepted receipts. Unknown fields are rejected.

## Choose exact identities and local roots

Every `REPLACE_*` value below is a mandatory replacement, not a usable ID. Obtain
the project from authorized `project list/show`, and asset/version IDs from an
actual accepted Push receipt or authorized `versions`/full version read. Use the
exact returned `asset_id` and `version_id`, not a slug, operation ID or latest
alias. Check the connected instance with `meta`/`whoami`. For permanent references
use the actual returned `lantai://...` reference; do not manufacture one from a
filename. A reference from another instance is not interchangeable.

Use the user's selected origin and existing private session. All connected
commands below append `--server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"`;
these variables must already identify that selection. Add `--ca-file` only when
the selected deployment requires its supplied CA. Do not put session contents
in an input document. Do not set arbitrary credentials to make a recipe pass.

Run commands from a user-selected workspace with separate sibling roots:

```text
inputs/       private JSON selections, frozen plans, original preparations
source/       explicitly selected source files
state/        original private upload/metadata state files
work/         downloaded working copies, one root per derivation
artifacts/    private derived outputs
reports/      private receipts and reports
records/      management record bytes, separate from preparations/state
```

Create the needed local directories explicitly. On Unix use owner-only
permissions for input/state/report files and directories (`umask 077` before
creating them); on Windows use current-user private ACLs. Do not follow links or
reuse a directory alias. Input, state, output, source and working-copy roots
must not overwrite or overlap one another. CLI paths below are local workspace
paths; JSON file paths use `/` and are relative to their selected source/work
root, never server storage paths.

## PushInput: an explicitly selected new asset

Save as `inputs/push.json`. This example assumes `source/sample.txt` is a small
synthetic text file authored for this exercise, with an explicitly confirmed
CC0 declaration. It does not apply that declaration to unknown or third-party
materials. For real materials preserve the actual authorized rights statement;
do not invent Owned/CC0 rights, provenance, approval or a redistribution grant.

```json
{
  "project_id": "REPLACE_PROJECT_ID",
  "slug": "synthetic-input-example",
  "content": {
    "asset_type": "doc",
    "files": [{"path": "sample.txt", "role": "source"}],
    "rights": {
      "usage": "reference",
      "license": "CC0-1.0",
      "redistribute_raw": true,
      "noai": false,
      "sensitivity": "normal"
    }
  },
  "describe": {"title": "Synthetic input example"}
}
```

`content` is required; `files` is inside `content`, not at the top level.
`asset_type` is a type name advertised by `types`, not a MIME type. File `role`
must be appropriate for that installed type. Omit `sha256` and `size` to have
the client hash actual local bytes; if supplied, both must match. `describe`
is initial current metadata, distinct from frozen `content.metadata`.

```sh
lantai types --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
lantai push --input inputs/push.json --directory source --state state/push.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

Keep the original input and state. Inspect the complete structured response,
including `description_pending`/description errors even with exit 0. Save exact
accepted asset/version/operation identifiers. An unknown response is reconciled
using original state/operation; it does not authorize a new upload. For a new
version of an existing asset use actual `asset_id` and `base_version_id` at the
top level instead of choosing a new slug; do not guess the base. Optional
`task`, `uses`, `producer` and frozen metadata need their public contracts and
are deliberately absent from this minimal example.

## WorksetInput: fixed versions and explicit local paths

Save as `inputs/workset.json`. This minimal example requires the actual chosen
version to contain `sample.txt`. Replace it with a path from that exact frozen
manifest when selecting other material.

```json
{
  "schema": "lantai.client-workset/v1",
  "entries": [{
    "asset_id": "REPLACE_ASSET_ID_FROM_RECEIPT",
    "version_id": "REPLACE_VERSION_ID_FROM_RECEIPT",
    "destination": "selected"
  }],
  "entrypoints": ["selected/sample.txt"]
}
```

`destination` is a nonoverlapping relative directory, not an output filename.
All paths in `entrypoints` and optional `dependencies` are relative to the whole
working copy, including the destination prefix. A dependency declaration has
the shape `{"from":"model/mesh.obj","requires":["model/body.mtl"]}`. Add it
only when both paths are selected and mapped to actual fixed-version files;
the declaration does not resolve/download another asset. Do not add fabricated
dependencies to the minimal sample above.

```sh
lantai asset workset plan --input inputs/workset.json --directory work/fixed --output reports/workset-plan.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
lantai asset workset pull --input inputs/workset.json --directory work/fixed --purpose archive_review --output reports/workset-pull.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

Use the original **WorksetInput** for pull, not the generated WorksetPlan or
receipt. Plan reads exact metadata without transfer or native execution. Pull
rechecks current purpose/rights and actual bytes; existing different files are
not overwritten. A completed workset is not proof that a native app opens it.

## DependencySelection: selected local text files

For files actually present at `source/scene/mesh.obj`,
`source/scene/body.mtl` and `source/workflow.json`, save
`inputs/dependencies.json`:

```json
{
  "paths": ["scene/mesh.obj", "scene/body.mtl", "workflow.json"],
  "comfy_input_root": "inputs"
}
```

```sh
lantai asset dependencies --input inputs/dependencies.json --directory source --output reports/dependencies.json
```

There is no `schema` or asset ID in DependencySelection. Select only files the
user authorized; omit nonexistent paths instead of assuming they are fetched.
`comfy_input_root` is the explicitly chosen ComfyUI input root relative to the
selected directory, not the directory containing the graph. Omit it when
unknown and retain unresolved results. Only OBJ/MTL and supported ComfyUI JSON
text is analyzed; it neither opens referenced files automatically nor executes
nodes or fetches models/URLs. Limits: 128 selected files, 1 MiB each, 4 MiB total.
Inspect `complete`, `notices`, dependencies and `missing_selected_files`.
Dependency candidates need explicit fixed-version mapping before workset use.

## DerivationInput: compare, image, OBJ and TAR

Every example below is its own private input file. Derive obtains fresh remote
authorization and checks actual selected bytes; `--directory` is a local work
root which may receive those fixed-version files. Keep it separate for each
action. Existing cached bytes do not replace current authorization. Never
change purpose merely to bypass a denial. Artifacts are immutable, private and
outside the work root, input and receipt paths.

`inputs/compare.json` compares two actual selected fixed versions (which may be
two versions of the same asset). Replace all four IDs from actual receipts or
authorized version reads:

```json
{
  "schema": "lantai.asset-derivation-input/v1",
  "action": "compare",
  "purpose": "archive_review",
  "before": {"asset_id": "REPLACE_BEFORE_ASSET_ID", "version_id": "REPLACE_BEFORE_VERSION_ID"},
  "after": {"asset_id": "REPLACE_AFTER_ASSET_ID", "version_id": "REPLACE_AFTER_VERSION_ID"}
}
```

```sh
lantai asset derive --input inputs/compare.json --directory work/compare --artifact artifacts/compare.json --output reports/compare.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

`inputs/thumbnail.json` selects an image path from its exact version manifest:

```json
{
  "schema": "lantai.asset-derivation-input/v1",
  "action": "thumbnail",
  "purpose": "archive_review",
  "source": {"asset_id": "REPLACE_IMAGE_ASSET_ID", "version_id": "REPLACE_IMAGE_VERSION_ID", "path": "image.webp"},
  "edge": 320
}
```

```sh
lantai asset derive --input inputs/thumbnail.json --directory work/thumbnail --artifact artifacts/thumbnail.png --output reports/thumbnail.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

For `proxy`, use the same source structure, `action:"proxy"`, an appropriate
edge (for example 1024), a separate input/work root and PNG artifact. Both
actions decode supported PNG/JPEG/WebP and produce PNG; no AI service is used.
`edge` is one of 320, 512, 1024, 2048. Thumbnail defaults to 320, others to 1024.

`inputs/obj.json` selects a real OBJ path in the frozen manifest:

```json
{
  "schema": "lantai.asset-derivation-input/v1",
  "action": "obj_structure",
  "purpose": "archive_review",
  "source": {"asset_id": "REPLACE_OBJ_ASSET_ID", "version_id": "REPLACE_OBJ_VERSION_ID", "path": "scene/mesh.obj"}
}
```

```sh
lantai asset derive --input inputs/obj.json --directory work/obj --artifact artifacts/obj.json --output reports/obj.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

For wireframe, use a separate input with `action:"obj_wireframe"` and optional
`edge:1024`, a separate work root and `.svg` artifact. Structure produces JSON;
wireframe produces a bounded SVG projection. Neither loads materials/textures
or runs a native application.

`inputs/tar.json` embeds the original fixed WorksetInput (not its receipt):

```json
{
  "schema": "lantai.asset-derivation-input/v1",
  "action": "workset_tar",
  "purpose": "raw_export",
  "workset": {
    "schema": "lantai.client-workset/v1",
    "entries": [{"asset_id": "REPLACE_ASSET_ID_FROM_RECEIPT", "version_id": "REPLACE_VERSION_ID_FROM_RECEIPT", "destination": "selected"}],
    "entrypoints": ["selected/sample.txt"]
  }
}
```

```sh
lantai asset derive --input inputs/tar.json --directory work/tar --artifact artifacts/selected.tar --output reports/tar.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

Compare/TAR accept neither `source` nor `edge`; image/OBJ accept neither
`before`, `after` nor `workset`. Compare/TAR have a combined selected budget of
256 files/64 MiB; other media have bounded decoding/geometry limits. Check
receipt `inputs`, artifact SHA/size and current verification; these are not
human approval, native continuation or source retirement authorization.

## Conditional metadata plan/apply

Save `inputs/metadata-selection.json` with actual selected asset IDs:

```json
{
  "schema": "lantai.metadata-batch-input/v1",
  "items": [{"asset_id": "REPLACE_ASSET_ID_FROM_RECEIPT", "patch": {"title": "Selected title"}}]
}
```

```sh
lantai asset metadata plan --input inputs/metadata-selection.json --output inputs/metadata-plan.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
lantai asset metadata apply --input inputs/metadata-plan.json --state state/metadata.json --output reports/metadata.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

Review the frozen plan before the authorized apply. Keep original plan/state
for partial, conflict or unknown responses. Do not regenerate a plan or key to
override a stale revision. Inspect every row and fresh verification; this is
not a batch transaction. The minimal patch does not write unknown `extra` back.
Explicit `extra` replaces that whole field; profile updates have the dedicated
profile semantics. Sensitivity/default rights require their special commands.

## ManagementRecordSpec: prepare and save a private observation

Save `inputs/management-spec.json`. This is an assistant observation record,
not a migration checker input or core acceptance claim. The payload is selected
ordinary JSON; this example's field names are payload data, not new protocol
fields. Use actual accepted IDs as the recorded observation:

```json
{
  "project_id": "REPLACE_PROJECT_ID",
  "kind": "assistant",
  "title": "Selected fixed-version observation",
  "payload": {
    "observed_asset_id": "REPLACE_ASSET_ID_FROM_RECEIPT",
    "observed_version_id": "REPLACE_VERSION_ID_FROM_RECEIPT",
    "note": "Historical observation only; consult current core state before reuse."
  }
}
```

```sh
lantai management prepare --input inputs/management-spec.json --output inputs/management-prepared.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
lantai management save --input inputs/management-prepared.json --directory records/assistant --state state/management.json --server "$LANTAI_SERVER" --session-file "$LANTAI_SESSION"
```

Prepare reads and creates an immutable local preparation. Save uses that exact
preparation and original Push state, writes `record.json` and commits a config
asset. Do not handcraft its envelope, hash or generated Push input. Kinds are
`migration`, `workset`, `recovery`, `assistant`, but specialized workflows need
their own preparation contracts. For revision, supply both actual original
record `asset_id` and exact `base_version_id` in the spec, prepare separately,
then use `management revise` with the resulting preparation and original state
for that intent. Never insert credentials, grants, transfer URLs or tokens in
payload. Saving an observation does not make it a current core check.


These minimal examples do not define import/reference batches, task/review/Job
requests, workset return/adoption, recovery factors or rights relaxation. Follow
their installed tool schemas and specific public contract; do not infer those
inputs or permissions from a superficially similar example.
