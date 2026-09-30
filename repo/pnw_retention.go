package repo

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kopia/kopia/internal/epoch"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/content/indexblob"
	"github.com/kopia/kopia/repo/format"
)

// WHAT "RetentionPeriod" ACTUALLY MEANS (kopia does not document this anywhere -- the CLI
// flag help is circular, "Set the blob retention-period", and the struct field in
// repo/format/blobcfg_blob.go carries no doc comment at all).
//
// It is NOT how long a blob is retained. It is the offset from *now* that retain-until is
// set to, EVERY time it is set:
//
//	retainUntilDate := clock.Now().Add(opts.RetentionPeriod).UTC()   // s3_storage.go:236
//
// The identical expression runs at PUT and at every extend-blob-retention-time pass -- an
// absolute reset, never an increment. So the real relationship is:
//
//	effective retention = (time the blob stays current and keeps being extended) + RetentionPeriod
//
// which for most blobs is far longer than the parameter suggests. Measured on this repo: a
// data pack stays referenced ~30d until its last snapshot ages out, then its final 720h
// stamp carries it ~30d more -- a ~60-day physical lifetime from a parameter labelled 30
// days.
//
// The useful mental model is a FLOOR ON REMAINING PROTECTION, refreshed each cycle: "from
// any moment after a stamp, this blob is guaranteed undeletable at least this long." That
// is exactly why the same number serves as both the abandonment tail (cost) and the
// post-compromise recovery grace window -- both are "guaranteed remaining", not "total".
//
// The name is inherited from S3 Object Lock, where it IS accurate: DefaultRetention sets
// retain-until = creation + period, applied ONCE, so period == total protected lifetime.
// Kopia breaks that correspondence by re-applying it on a schedule. Note upstream keeps
// extension OFF by default and half-admits the consequence in maintenance_params.go:49-53
// ("may cause data to be kept longer than desired if the retention period is relatively
// long") -- so the name is only misleading BECAUSE this deployment enables extension.
//
// "MetadataRetentionPeriod" and KOPIA_PNW_METADATA_RETENTION_HOURS below inherit the same
// imprecision deliberately: matching upstream vocabulary is worth more in a patch than
// being locally more accurate. Read them as "metadata retention EXTENSION time".
//
// PNWSoft patch: two-tier object-lock retention.
//
// Upstream kopia applies a single RetentionPeriod (from kopia.blobcfg) to every blob
// matching GetLockingStoragePrefixes(), both at PUT time (wrapLockingStorage in open.go)
// and on every extend-blob-retention-time pass. That single period ends up controlling
// two unrelated things at once:
//
//  1. How long an UNREFERENCED blob lingers in the bucket before its lock lapses and it
//     can finally be purged. For data packs ("p") this is pure cost -- with a 30d period
//     a superseded pack sits hidden-but-locked for ~30 days after it stops being useful,
//     which on this repo is several TB of dead weight.
//
//  2. How far back a point-in-time (PIT) view can still be reconstructed. PIT is built on
//     bucket object VERSIONS (repo/blob/s3/s3_pit.go); a superseded version survives only
//     until its lock lapses, after which the bucket lifecycle rule purges it. Index/epoch/
//     format blobs are superseded constantly by epoch compaction, so THEIR period is what
//     sets the PIT horizon -- measured at ~30-40d against a 720h period.
//
// Those two wants pull in opposite directions, and upstream cannot express both. But note
// what actually protects a blob you still need: a REFERENCED blob is kept alive by being
// re-stamped every maintenance cycle, not by the length of any single lock. So the period
// only governs the useless tail. That makes the tiers separable:
//
//	data packs ("p")  -> blobcfg.RetentionPeriod (short; the cost lever)
//	metadata          -> MetadataRetentionPeriod (long; the PIT-horizon lever)
//
// Setting the repo's retention-period to e.g. 336h (14d) while metadata stays at 720h
// (30d) halves the pack tail without moving the PIT horizon, because the packs needed to
// restore the newest snapshot as-of T (T <= snapshot retention) were still referenced --
// and therefore still being extended -- right up to the present.
//
// The metadata tier is a build/env constant rather than a new kopia.blobcfg field on
// purpose: adding a field would change the on-disk repository format and require an
// upgrade path, whereas this is local policy that can be reverted by rebuilding.
const (
	// MetadataRetentionEnvVar overrides the metadata retention tier, in hours.
	//
	// This exists so the tier can be tuned without a rebuild: on this deployment every
	// new kopia.exe hash invalidates the Halcyon Monitor override that whitelists it, so
	// rebuilds carry an operational cost that a config knob avoids.
	MetadataRetentionEnvVar = "KOPIA_PNW_METADATA_RETENTION_HOURS"

	// DefaultMetadataRetentionPeriod is the retention tier applied to index, epoch,
	// format and metadata-pack blobs when the env var is unset. It should be >= the
	// desired PIT horizon, which in turn should be >= snapshot retention.
	DefaultMetadataRetentionPeriod = 30 * 24 * time.Hour
)

// metadataLockingPrefixes returns the subset of GetLockingStoragePrefixes() that the
// longer metadata tier applies to.
//
// This is an explicit ALLOWLIST rather than "everything except p" by design. Compliance-mode
// locks can only ever be extended, never shortened or removed -- so a bug that granted the
// long tier to data packs would be unfixable except by waiting it out. Failing closed here
// means an unrecognised prefix falls back to the short tier, which is the recoverable
// direction.
//
// "q" (PackBlobIDPrefixSpecial) carries manifest contents. Its packs follow the same
// referenced-lifecycle as "p" and would in principle survive on the short tier too, but a
// missing manifest is what turns a PIT view into "0 snapshots" -- the whole view stops
// resolving rather than degrading. Metadata packs are a small fraction of repo bytes, so
// they get the long tier as cheap insurance.
func metadataLockingPrefixes() []blob.ID {
	return []blob.ID{
		blob.ID(indexblob.V0IndexBlobPrefix),       // "n"  -- legacy index blobs
		blob.ID(epoch.EpochManagerIndexUberPrefix), // "x"  -- umbrella: xe/xn/xs/xr/xw
		blob.ID(format.KopiaRepositoryBlobID),      // kopia.repository
		blob.ID(format.KopiaBlobCfgBlobID),         // kopia.blobcfg
		content.PackBlobIDPrefixSpecial,            // "q"  -- metadata/manifest packs
	}
}

// IsMetadataLockingBlob reports whether the blob belongs to the longer metadata
// retention tier.
func IsMetadataLockingBlob(id blob.ID) bool {
	for _, prefix := range metadataLockingPrefixes() {
		if strings.HasPrefix(string(id), string(prefix)) {
			return true
		}
	}

	return false
}

// MetadataRetentionPeriod returns the retention period for metadata blobs, given the
// repository-configured (data pack) period.
//
// The result is never shorter than base: the tiering may only ever lengthen retention
// relative to what upstream would have applied, so a misconfigured env var degrades to
// stock behaviour instead of silently weakening protection.
func MetadataRetentionPeriod(base time.Duration) time.Duration {
	period := DefaultMetadataRetentionPeriod

	if v := os.Getenv(MetadataRetentionEnvVar); v != "" {
		if hours, err := strconv.ParseFloat(v, 64); err == nil && hours > 0 {
			period = time.Duration(hours * float64(time.Hour))
		}
	}

	if period < base {
		return base
	}

	return period
}

// RetentionPeriodForBlob returns the object-lock retention period to apply to a blob,
// selecting between the data-pack tier and the metadata tier.
//
// Callers must already have established that retention is enabled and that the blob
// matches GetLockingStoragePrefixes(); this only chooses the period.
func RetentionPeriodForBlob(id blob.ID, cfg format.BlobStorageConfiguration) time.Duration {
	if IsMetadataLockingBlob(id) {
		return MetadataRetentionPeriod(cfg.RetentionPeriod)
	}

	return cfg.RetentionPeriod
}
