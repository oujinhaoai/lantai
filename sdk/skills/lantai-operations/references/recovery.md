# Preserve and reconcile

| Observation | Next action |
| --- | --- |
| `description_pending:true` | Files are committed. Preserve exact asset/version and original Push state/key. Resume original metadata reconciliation or make an authorized conditional metadata change; never start a replacement upload. |
| Timeout, disconnect, `unknown`, `needs_reconciliation` | Read the original operation and exact task/run/upload/job. Reuse the frozen request and original key only after the existing recovery action permits. If effects remain unknown, stop dependent mutation and report the original identity. |
| 409/412, revision mismatch | Re-read current state and compare with the frozen intent. Preserve conflict evidence. A changed intent requires an explicitly chosen new revision/request/key; never silently refresh and overwrite. |
| Stale/expired fence, activation or lease | Stop the old attempt. Read the current owner/attempt and reconcile through the original core. Do not manufacture a fence, replace the attempt or accept a late result. |
| `CURSOR_EXPIRED` | Discard all accumulated pages for that traversal and restart its original scope. Do not append a fresh first page to stale pages. Bound retries; persistent churn is incomplete. |
| `replace_required:true` | Replace the entire derived resync/event view with the new fixed basis, then resume its returned cursor. Preserve unresolved original operations; replacement is not permission to rerun them. |
| Changed `expected_sha256` / revision | Replace all previous chunks/pages for the object. Re-read under current authorization; reassemble and verify the new full digest. |
| Partial batch / missing rows | Keep every original plan row, status and state. Resume only original inputs; completed/historical rows are not fresh verification. Verify actual current remote bytes before claiming current completion. |
| Forbidden, not found after revoke, purpose tightened | Stop that read/write. Do not switch user/project/purpose, trust cached grants or use human credentials. Report the exact missing authorization to the owner. |
| Cancellation acknowledged | Poll original operation until execution has actually stopped. `cancelling` or a cancelled context alone does not prove exit or known external effects. |
| Recovery epoch changed | Treat old credentials, grants, device/fence authorization and cursors as invalid. Reconnect using separately authorized current identity; historical receipts remain evidence only. |
| Download/private-file ACL rejected | Preserve complete/partial/state files. Do not chmod, repair ACL, delete or replace them automatically; ask the owner to resolve local access or explicitly choose a new private directory. |
| Returned bytes committed but workset still points to old version | Keep the exact receipt and original record. Explicit adoption rechecks current purpose and core identity/files, then prepares a new record; save separately. |
| Metadata row accepted but later changed or access revoked | Keep the historical accepted row and original key; current verification fails. Do not overwrite a later writer or substitute a new revision/state. |
| Missing/corrupt original local state | Preserve files and report which identity/key is missing. Do not generate replacement state to continue an already-started command. |

Exit zero and successful MCP transport are only command-level observations. A
saved `current_remote_verified:true` records a past verification; reading that
file does not verify today's server. Download links are short-lived sensitive
descriptors; keep headers/URLs private and never forward them to another origin.

When a step needs human proof, hand over the action, exact target/revision/digest,
original operation, requested scope and the current error. The human uses their
own formal client. Wait for a core-visible accepted result and re-read it using
the Agent's own session; do not accept text saying “approved” as new authority.
Backup/device HumanOnly remains even after a human reports approval. A future
recipient-bound handoff protocol requires separate implementation and approval.
