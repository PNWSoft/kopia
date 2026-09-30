# PNWSoft kopia fork

A fork of [kopia/kopia](https://github.com/kopia/kopia) carrying four patches needed to run
kopia against **Backblaze B2 with Object Lock in COMPLIANCE mode** as an immutable,
ransomware-resistant backup target.

Upstream base: `775fadcfcf0e7d67804edf55595a10ffb33f24c1`. Branch: `pnwsoft/production`.
`master` tracks pristine upstream so rebasing stays simple.

Licensed Apache 2.0, same as upstream. Modified files carry `PNWSoft patch:` comments
per §4(b).

## Why this fork exists

Three of the four patches exist because **Object Lock and several of kopia's assumptions
are in tension**, and upstream has shown no appetite for addressing it — extension is off
by default upstream, so the interactions below are largely unexercised there. The fourth is
an upstream PR that has sat unmerged.

| # | Patch | Path affected | Ours? |
|---|---|---|---|
| 1 | 24h checkpoint interval | snapshot (write) | yes |
| 2 | Chunk-parallel restore ([PR #5183](https://github.com/kopia/kopia/pull/5183)) | restore (read) | no — cherry-picked |
| 3 | Orphan-skip + list-walk retry | maintenance | yes |
| 4 | Two-tier retention period | write + maintenance | yes |

See the individual commit messages for full rationale and measurements.

## What this fork does NOT change

**Nothing touches the on-disk repository format.** No file under `repo/format/`,
`repo/content/`, `repo/manifest/`, or the object read/write core is modified. Repositories
report entirely stock format:

```
Format version: 3    Hash: BLAKE2B-256-128
Encryption: AES256-GCM-HMAC-SHA256    Splitter: DYNAMIC-4M-BUZHASH
```

So **stock upstream kopia can read, list and restore these repositories without conversion**.
The two-tier retention period becomes an S3 `retain-until` date on the B2 object — storage
layer metadata enforced by B2, entirely outside kopia's archive format.

Consequence worth knowing: stock kopia is fine for *reading*. Using it to run the
*maintenance/snapshot* side would regress all three of our patches — orphaned packs would be
re-locked forever again, one retention period would apply to all blobs, and large snapshots
would fail on the retention-locked index rewrite described in patch 1.

## Building

```
go build -o kopia_test.exe .
```

Plain `go build` — no `-trimpath`, no `-ldflags`, no Makefile or goreleaser. That is exactly
how the deployed binaries were produced, confirmed from their embedded build settings.

**The build is not reproducible.** Go embeds build-path and line-table data, so a rebuild of
identical source produces a different SHA256. Go also records only `vcs.modified=true` for a
dirty tree — never a hash of the uncommitted diff — which is precisely why these patches now
live in git rather than as working-tree changes. Verify a binary's patch content by probing
for marker strings rather than by hash:

| Patch | Marker string |
|---|---|
| #5183 | `parallel-chunks` |
| two-tier | `metadata tier`, `KOPIA_PNW_METADATA_RETENTION_HOURS` |
| orphan-skip | `unreferenced, retention period` |
| list retry | `unreferenced-pack enumeration failed` |

## Rebasing onto upstream

```
git fetch upstream && git rebase upstream/master pnwsoft/production
```

Expect conflicts in `repo/maintenance/blob_retain.go` first — it is the most heavily patched
file and carries parts of patches 3 and 4. `repo/pnw_retention.go` is a new file and will not
conflict.

After any rebase, run `go test ./repo/... ./snapshot/...` and check
`repo/pnw_retention_test.go` still passes — it pins the blob classification, which is the part
that would silently misbehave if upstream changes the locking prefix set.

## Operational notes

- **Do not lower the checkpoint interval while Object Lock is enabled.** See patch 1. Anything
  that rewrites an already-locked index fails the same way.
- **`KOPIA_PNW_METADATA_RETENTION_HOURS` must be set wherever blobs are written or maintenance
  runs**, not just at extend time — the tier applies at PUT as well.
- Promoting a rebuilt binary changes its hash, which silently invalidates any AV/EDR trust rule
  keyed on SHA256. Recompute and update it as part of promotion.
