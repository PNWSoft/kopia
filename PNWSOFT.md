# PNWSoft kopia fork

A fork of [kopia/kopia](https://github.com/kopia/kopia) carrying four patches needed to run
kopia against **S3-compatible object storage with Object Lock in COMPLIANCE mode** as an
immutable, ransomware-resistant backup target.

Upstream base: `775fadcfcf0e7d67804edf55595a10ffb33f24c1`. Branch: `pnwsoft/production`.
`master` tracks pristine upstream so rebasing stays simple.

Licensed Apache 2.0, same as upstream. Modified files carry `PNWSoft patch:` comments per §4(b).

## Why this fork exists

Three of the four patches exist because **Object Lock and several of kopia's assumptions are in
tension**. Upstream keeps retention extension *off by default*, so these interactions are
largely unexercised there and no fix is expected. The fourth is an upstream PR that has sat
unmerged.

| # | Patch | Path | Ours? |
|---|---|---|---|
| 1 | 24h checkpoint interval | snapshot (write) | yes |
| 2 | Chunk-parallel restore ([PR #5183](https://github.com/kopia/kopia/pull/5183)) | restore (read) | no — cherry-picked |
| 3 | Orphan-skip + list-walk retry | maintenance | yes |
| 4 | Two-tier retention period | write + maintenance | yes |

---

## 1. `DefaultCheckpointInterval` 45 min → 24h

`snapshot/upload/upload.go` — one constant.

**What.** Effectively disables mid-upload checkpointing, since no normal run lasts a day.

**Why.** Checkpointing reorganises indexes as it goes. Under COMPLIANCE-mode Object Lock those
indexes are already retention-locked and immutable, so the reorganisation is **refused by the
storage backend and the snapshot fails.** At the 45-minute default, any snapshot large enough to
cross a checkpoint boundary cannot complete — in our case an ~800 GB VM disk could not be
snapshotted at all.

**Caveat.** This is a workaround, not a fix. The underlying incompatibility is that *anything*
rewriting an already-locked index fails the same way. **Do not lower this while Object Lock is
enabled**, and suspect it first if a long-running snapshot starts failing on index writes.

**A `--checkpoint-interval 0` on the command line does not substitute for this patch.** That flag
is a no-op: `cli/command_snapshot_create.go` guards the assignment with `if interval != 0`, so
passing 0 never overwrites anything and `CheckpointInterval` keeps its constructor default — which
is the constant above. On stock kopia the same command line therefore yields 45 minutes and still
fails. 0 cannot mean "disabled" because `getTicker` is `time.Tick`, which rejects a zero duration;
hence the guard. The flag's help text also still reads *"must be <= 45 minutes"*, stale here —
validation compares against `DefaultCheckpointInterval`, now 24h.

Upstream has never touched `snapshot/upload/` — 0 of the 200 commits between `775fadcfcf0e` and
`87d15ded` — so this mechanism has been stable across rebases so far.

## 2. Chunk-parallel restore — `--parallel-chunks`

`cli/command_restore.go`, `repo/object/parallel_reader.go` (+test),
`snapshot/restore/local_fs_output.go`, `snapshot/snapshotfs/repofs.go`.
Cherry-picked from [PR #5183](https://github.com/kopia/kopia/pull/5183) — **not PNWSoft work.**
Vendored because the PR has been open since February 2026 with no upstream activity.

**What.** Adds blob-level (chunk-level) parallelism *within a single file*. Defaults to 8
workers, so the improvement applies without passing the flag.

**Why.** kopia's restore worker pool is **file-level**. Restoring one very large file — a VM
disk image — engages exactly one worker fetching blobs sequentially. HTTP/2 multiplexes those
requests over ~2 TCP connections, but with no concurrent demand there is nothing to multiplex,
so throughput is bound by per-request latency rather than by bandwidth.

**Measured** restoring one large VM disk from S3-compatible storage, 2026-05-10:

| Configuration | Throughput |
|---|---|
| stock, default | 64 Mbps |
| stock + `GODEBUG=http2client=0 --parallel 16` | 94 Mbps |
| patched + `GODEBUG=http2client=0 --parallel-chunks 32` | **~911 Mbps** |
| patched + `--parallel-chunks 64` | 945 Mbps (diminishing) |

Also tested and found **unnecessary**: raising `MaxIdleConnsPerHost`/`MaxConnsPerHost` in the s3
transport. With chunk parallelism the default Go pool suffices — the bottleneck was demand, not
supply. For aggregate beyond this, run one `kopia restore` per file in parallel; chunk
parallelism stacks across processes.

## 3. Orphan-skip, and retrying the list walks

`repo/maintenance/blob_retain.go`, `repo/maintenance/pack_gc.go`,
`repo/maintenancestats/*`.

### 3a. Skip unreferenced packs during `extend-blob-retention-time`

**What.** `IterateUnreferencedPacks` — the same call `pack_gc.go` uses, so exactly the blobs
`full-delete-blobs` targets — now builds a skip set before extension. Still-referenced packs and
all metadata blobs continue to be extended. A `SkippedUnreferencedBlobCount` stat makes the skip
visible in `kopia maintenance info`.

**Why.** Upstream extends retention on **every blob in storage every cycle**, including orphaned
packs that `full-delete-blobs` is concurrently trying to delete. Their `retain-until` is
therefore renewed forever and they can **never expire**, so storage grows without bound. Measured
here at ~198,899 orphaned packs / **~4.5 TB** that could not be reclaimed.

Related upstream discussion, no fix expected:
[forum thread](https://kopia.discourse.group/t/bucket-size-when-using-object-lock-ransomware-protection/4567),
feature requests #4894 (open) and #3427 (closed wontfix).

### 3b. Retry the streamed list walks

**What.** `IterateUnreferencedPacks` and `IterateAllPrefixesInParallel` are wrapped in
`retry.WithExponentialBackoffMaxRetries(ctx, 5, …)`.

**Why.** `retrying.Storage` wraps Get/Put/Delete/GetMetadata but **deliberately not `ListBlobs`**,
because a stream cannot resume mid-way. A single transient network drop during a ~417k-blob walk
therefore failed the entire maintenance phase — observed three times in June 2026. Because the
failing auto-maintenance made `snapshot create` exit non-zero, it surfaced as a **false backup
failure**, which is worse than the underlying fault.

**Design notes.** Restart is idempotent — the set/slice is rebuilt per attempt. In `pack_gc.go`
the collected packs are fed to delete workers only *after* the walk fully succeeds, because
delete, unlike retention-extend, is **not** idempotent on re-send. Giving up after 5 attempts
returns the error and falls back to prior behaviour (redo next cycle), so there is no new worst
case. Per-blob `ExtendBlobRetention` calls are intentionally **not** wrapped: they already get
minio-go's per-request retry, and wrapping them would risk a retry storm during a real outage.

## 4. Two-tier retention period

`repo/pnw_retention.go` (new), `repo/pnw_retention_test.go` (new), `repo/open.go`,
`repo/maintenance/blob_retain.go`.

**What.** Metadata blobs (`n`, `x*`, `kopia.repository`, `kopia.blobcfg`, `q`) take a separate,
longer retention period set by `KOPIA_PNW_METADATA_RETENTION_HOURS`; data packs (`p`) keep the
repository's configured `retention-period`. Applied at **both** PUT (`wrapLockingStorage`) and
extend time.

**Why.** One retention period has to serve two purposes that want opposite values:

- the **tail** an abandoned blob lingers for after it stops being referenced — pure cost
- the **window** in which a compromised credential cannot delete anything — protection

For data packs the tail is almost the entire bill. But for index/epoch/format/manifest blobs,
which epoch compaction supersedes constantly, the period *alone* determines how far back a
point-in-time view can still reconstruct — a superseded blob survives only until its lock lapses.
Upstream applies one period to both, so the two goals collide. Splitting them resolves it.

The split is worth it because the sizes are wildly asymmetric. Measured on our repository: data
packs are **99.97%** of live bytes, all metadata prefixes together **0.03%**. So a generous
metadata tier costs cents while the pack tier is where any saving would come from.

**Design notes.**

- **Allowlist, fails closed.** An unrecognised prefix gets the **short** tier. Compliance locks
  can only ever be *extended*, so wrongly promoting a blob to the long tier is unfixable except
  by waiting it out, while the opposite error is recoverable. Default to the recoverable
  direction.
- `MetadataRetentionPeriod` never returns less than the base period, so a bad env value degrades
  to upstream behaviour rather than weakening protection.
- **Env var, not a `kopia.blobcfg` field.** A field would change the on-disk repository format
  and need an upgrade path. See "format compatibility" below.
- **Lowering the pack period produces refusals, and that is correct.** Extension then requests an
  *earlier* `retain-until` on blobs still holding the old longer lock, which Compliance refuses.
  A refusal means the blob is **more** protected than requested, never less, so
  `isRetentionShortenRefusal` counts it instead of failing the phase; it clears itself within one
  old period. Guard against masking real faults: if **nothing at all** extended, refusals are
  treated as fatal.

### The coupling this creates — read before changing any retention number

```
pack horizon     = snapshot retention + pack object-lock period
metadata horizon = metadata object-lock period
EFFECTIVE PIT    = min(pack horizon, metadata horizon)
```

A pack stays *referenced*, and therefore re-stamped every cycle, until its last snapshot ages
out — so snapshot retention is the first term, and only then does its final lock start running.
Metadata is superseded within days, so its lock alone is its reach.

**These are not independent.** Dropping snapshot retention moves the pack horizon one-for-one,
with nothing in the lock configuration looking wrong. Treat snapshot retention and pack lock as a
coupled pair.

### Why we run both tiers long, despite the cost

Non-obvious, and worth stating so nobody "optimises" it: **we deliberately do not take the short
pack tier.** Because kopia re-stamps referenced blobs to `now + period` every cycle, that one
number is simultaneously the cost tail *and* the window during which an attacker holding the
storage credentials cannot delete anything. Those cannot be decoupled — decoupling would require
*shortening* a lock when a blob becomes unreferenced, which Compliance forbids by design, and
Governance mode permits but is bypassable by a privileged user, destroying the guarantee.

So the storage "overhead" is not waste; it is the purchased immutability window, priced per day.
A 30-day window that survives full credential compromise is the reason this storage tier exists,
which makes the cost a requirement rather than an inefficiency. The two-tier mechanism is still
worth having — it removes the metadata horizon as a constraint for free — but the pack half stays
long on purpose.

---

## Format compatibility

**Nothing here touches the on-disk repository format.** No file under `repo/format/`,
`repo/content/`, `repo/manifest/`, or the object read/write core is modified. Repositories report
entirely stock format:

```
Format version: 3    Hash: BLAKE2B-256-128
Encryption: AES256-GCM-HMAC-SHA256    Splitter: DYNAMIC-4M-BUZHASH
```

So **stock upstream kopia can read, list and restore these repositories without conversion.** The
two-tier period becomes an S3 `retain-until` date on the object — storage-layer metadata enforced
by the provider, entirely outside kopia's archive format.

Consequence worth knowing: stock kopia is fine for **reading**. Using it for the
**maintenance/snapshot** side would regress all three of our patches — orphaned packs re-locked
forever, one period for all blobs, and large snapshots failing on the locked-index rewrite in
patch 1.

## Building

```
go build -o kopia_test.exe .
```

Plain `go build` — no `-trimpath`, no `-ldflags`, no Makefile or goreleaser. That is exactly how
the deployed binaries were produced, confirmed from their embedded build settings.

**The build is not reproducible.** Go embeds build-path and line-table data, so rebuilding
identical source yields a different SHA-256. Go also records only `vcs.modified=true` for a dirty
tree, never a hash of the uncommitted diff — which is precisely why these patches now live in git
instead of as working-tree changes. Verify a binary's patch content by probing marker strings
rather than by hash:

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

Expect conflicts in `repo/maintenance/blob_retain.go` first — the most heavily patched file,
carrying parts of patches 3 and 4. `repo/pnw_retention.go` is a new file and will not conflict.

Afterwards run `go test ./repo/... ./snapshot/...` and confirm `repo/pnw_retention_test.go` still
passes — it pins the blob classification, which is the part that would silently misbehave if
upstream changes the locking prefix set.

## Operational notes

- **Do not lower the checkpoint interval while Object Lock is enabled** (patch 1).
- **`KOPIA_PNW_METADATA_RETENTION_HOURS` must be set wherever blobs are written *or* maintenance
  runs** — the tier applies at PUT as well as at extend time, so setting it in only one place
  silently half-applies.
- Promoting a rebuilt binary changes its hash, which silently invalidates any AV/EDR trust rule
  keyed on SHA-256. Recompute and update it as part of promotion — an unsigned custom build that
  reads every disk image and writes encrypted output looks exactly like ransomware to a scanner.
