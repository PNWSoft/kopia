package maintenance

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	"github.com/kopia/kopia/internal/blobparam"
	"github.com/kopia/kopia/internal/contentlog"
	"github.com/kopia/kopia/internal/contentlog/logparam"
	"github.com/kopia/kopia/internal/impossible"
	"github.com/kopia/kopia/internal/retry"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/maintenancestats"
)

const parallelBlobRetainCPUMultiplier = 2

const minRetentionMaintenanceDiff = time.Duration(24) * time.Hour

// maintEnumMaxRetries bounds how many times we restart a streamed blob enumeration
// (which the retrying.Storage wrapper does NOT cover -- it only retries
// Get/Put/Delete/GetMetadata, not ListBlobs, because a stream can't resume mid-way)
// before giving up and letting the next maintenance cycle retry the whole phase.
const maintEnumMaxRetries = 5

// ExtendBlobRetentionTimeOptions provides options for extending blob retention algorithm.
type ExtendBlobRetentionTimeOptions struct {
	Parallel int
}

// extendBlobRetentionTime extends the retention time of all relevant blobs managed by storage engine with Object Locking enabled.
func extendBlobRetentionTime(ctx context.Context, rep repo.DirectRepositoryWriter, opt ExtendBlobRetentionTimeOptions) (*maintenancestats.ExtendBlobRetentionStats, error) {
	ctx = contentlog.WithParams(ctx,
		logparam.String("span:blob-retain", contentlog.RandomSpanID()))
	log := rep.LogManager().NewLogger("maintenance-blob-retain")

	blobCfg, err := rep.FormatManager().BlobCfgBlob(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "blob configuration")
	}

	if !blobCfg.IsRetentionEnabled() {
		// Blob retention is disabled
		contentlog.Log(ctx, log, "Object lock retention is disabled.")

		return nil, nil
	}

	const extendQueueSize = 100

	extend := make(chan blob.Metadata, extendQueueSize)

	// PNWSoft patch: the retention period is now chosen PER BLOB -- data packs get the
	// repository-configured period, metadata blobs get the longer tier that sets the
	// point-in-time horizon. Resolved once here so every worker sees the same value.
	// See repo/pnw_retention.go for why the tiers are separable.
	metadataPeriod := repo.MetadataRetentionPeriod(blobCfg.RetentionPeriod)

	var (
		wg                                                 errgroup.Group
		extendedCount, toExtend, failedCount, skippedCount atomic.Uint64
		metadataCount, refusedCount                        atomic.Uint64
	)

	if opt.Parallel == 0 {
		opt.Parallel = runtime.NumCPU() * parallelBlobRetainCPUMultiplier
	}

	// PNWSoft patch: build the set of unreferenced pack blob IDs and skip extending
	// their object-lock retention. Upstream kopia extends retention on every blob in
	// storage every full-maintenance cycle, which means orphaned pack blobs are
	// continuously re-locked and full-delete-blobs can never delete them. Skipping
	// orphans here lets them age out on their original retention clock and become
	// deletable; still-referenced packs and all index/metadata blobs continue to
	// receive extension as before. The unreferenced set is built by the same
	// IterateUnreferencedPacks call that pack_gc.go uses, so we filter exactly the
	// blobs that full-delete-blobs would target.
	var (
		unrefMu  sync.Mutex
		unrefSet = make(map[blob.ID]struct{})
	)

	packPrefixes := []blob.ID{
		content.PackBlobIDPrefixRegular,
		content.PackBlobIDPrefixSpecial,
		content.BlobIDPrefixSession,
	}

	// Both this enumeration and the IterateAllPrefixesInParallel walk further down are
	// streamed list operations that the retrying.Storage wrapper does not cover, so a
	// single transient B2 network drop (wsarecv) during the multi-hundred-thousand-blob
	// walk otherwise fails the whole extend-blob-retention-time phase and loses the cycle.
	// Re-running an enumeration from scratch is idempotent (we rebuild the set each
	// attempt), so the worst case is unchanged from today -- give up after a bounded
	// number of tries and let the next maintenance cycle retry the phase -- but we get a
	// few chances to ride out a blip first. retry.Always is safe here: the retry loop
	// checks ctx.Err() each iteration, so a cancelled/killed maintenance still stops.
	if _, err := retry.WithExponentialBackoffMaxRetries(ctx, maintEnumMaxRetries,
		"identify unreferenced packs",
		retry.NoValueFn(func() error {
			unrefMu.Lock()
			unrefSet = make(map[blob.ID]struct{}) // reset so a partial prior attempt doesn't linger
			unrefMu.Unlock()

			if e := rep.ContentManager().IterateUnreferencedPacks(ctx, packPrefixes, opt.Parallel,
				func(bm blob.Metadata) error {
					unrefMu.Lock()
					unrefSet[bm.BlobID] = struct{}{}
					unrefMu.Unlock()
					return nil
				}); e != nil {
				// The retry package logs intermediate failures only at Debug, which the
				// maintenance output suppresses; log here so a retry is actually visible.
				contentlog.Log1(ctx, log, "unreferenced-pack enumeration failed, will retry if attempts remain",
					logparam.Error("error", e))

				return e
			}

			return nil
		}), retry.Always); err != nil {
		return nil, errors.Wrap(err, "identifying unreferenced packs for retention skip")
	}

	contentlog.Log1(ctx, log, "unreferenced pack blobs to skip extension",
		logparam.Int("count", len(unrefSet)))

	// start goroutines to extend blob retention as they come.
	for range opt.Parallel {
		wg.Go(func() error {
			for bm := range extend {
				// PNWSoft patch: select the tier for this blob.
				isMeta := repo.IsMetadataLockingBlob(bm.BlobID)

				period := blobCfg.RetentionPeriod
				if isMeta {
					period = metadataPeriod

					metadataCount.Add(1)
				}

				if err1 := rep.BlobStorage().ExtendBlobRetention(ctx, bm.BlobID, blob.ExtendOptions{
					RetentionMode:   blobCfg.RetentionMode,
					RetentionPeriod: period,
				}); err1 != nil {
					// PNWSoft patch: while the data-pack tier is being LOWERED, blobs still
					// carrying a longer lock from the previous period refuse the new, earlier
					// retain-until -- Compliance-mode locks only ever move forward. Such a
					// refusal means the blob is MORE protected than requested, never less, so
					// it must not fail the phase; it clears itself as the old locks age out.
					// Genuine breakage is still caught: if nothing at all extended, the check
					// after wg.Wait() treats these as fatal rather than a transition artifact.
					if isRetentionShortenRefusal(err1) {
						refusedCount.Add(1)

						continue
					}

					contentlog.Log2(ctx, log,
						"Failed to extend blob",
						blobparam.BlobID("blobID", bm.BlobID),
						logparam.Error("error", err1))

					failedCount.Add(1)

					continue
				}

				if currentCount := extendedCount.Add(1); currentCount%100 == 0 {
					contentlog.Log1(ctx, log, "extended blobs", logparam.UInt64("count", currentCount))
				}
			}

			return nil
		})
	}

	// Enumerate all relevant (active, extendable) blobs into a slice, then feed the extend
	// workers. Collecting first (rather than sending straight to the channel) is what lets
	// us retry the enumeration cleanly: each attempt resets the slice and the skip counter,
	// so a mid-stream restart never double-sends a blob to the workers or inflates the
	// extended/skipped stats. See maintEnumMaxRetries comment above for why this needs a
	// retry the storage layer doesn't provide.
	contentlog.Log(ctx, log, "Extending retention time for blobs...")

	var (
		toExtendList []blob.Metadata
		listMu       sync.Mutex
	)

	_, err = retry.WithExponentialBackoffMaxRetries(ctx, maintEnumMaxRetries,
		"enumerate lockable blobs",
		retry.NoValueFn(func() error {
			listMu.Lock()
			toExtendList = toExtendList[:0]
			listMu.Unlock()
			skippedCount.Store(0)

			if e := blob.IterateAllPrefixesInParallel(ctx, opt.Parallel, rep.BlobStorage(), repo.GetLockingStoragePrefixes(), func(bm blob.Metadata) error {
				// PNWSoft patch: skip unreferenced packs so they can age out and be deleted.
				if _, skip := unrefSet[bm.BlobID]; skip {
					skippedCount.Add(1)
					return nil
				}

				listMu.Lock()
				toExtendList = append(toExtendList, bm)
				listMu.Unlock()

				return nil
			}); e != nil {
				contentlog.Log1(ctx, log, "lockable-blob enumeration failed, will retry if attempts remain",
					logparam.Error("error", e))

				return e
			}

			return nil
		}), retry.Always)

	// Hand the collected blobs to the extend workers only if enumeration ultimately
	// succeeded; on give-up we close the channel empty and surface the error below.
	if err == nil {
		for _, bm := range toExtendList {
			extend <- bm
			toExtend.Add(1)
		}
	}

	close(extend)

	contentlog.Log1(ctx, log, "Found blobs to extend", logparam.UInt64("count", toExtend.Load()))
	contentlog.Log1(ctx, log, "Skipped unreferenced blobs", logparam.UInt64("count", skippedCount.Load()))

	errWait := wg.Wait() // wait for all extend workers to finish.
	impossible.PanicOnError(errWait)

	if count := failedCount.Load(); count > 0 {
		return nil, errors.Errorf("Failed to extend %v blobs", count)
	}

	// PNWSoft patch: refusals are only benign as a transition artifact of lowering the
	// data-pack tier. If not one blob extended, they are not transitional -- something is
	// actually wrong (credentials, permissions, a backend that stopped honouring locks)
	// and silently reporting success would hide the loss of object-lock protection.
	if refused := refusedCount.Load(); refused > 0 {
		if extendedCount.Load() == 0 {
			return nil, errors.Errorf("all %v retention extensions were refused by the storage backend", refused)
		}

		contentlog.Log1(ctx, log,
			"Retention extensions refused because the existing lock is longer (expected while lowering the data-pack tier)",
			logparam.UInt64("count", refused))
	}

	if err != nil {
		return nil, errors.Wrap(err, "error iterating packs")
	}

	result := &maintenancestats.ExtendBlobRetentionStats{
		ToExtendBlobCount:            toExtend.Load(),
		ExtendedBlobCount:            extendedCount.Load(),
		SkippedUnreferencedBlobCount: skippedCount.Load(),
		RetentionPeriod:              blobCfg.RetentionPeriod.String(),
		MetadataBlobCount:            metadataCount.Load(),
		MetadataRetentionPeriod:      metadataPeriod.String(),
		RefusedShortenCount:          refusedCount.Load(),
	}

	contentlog.Log1(ctx, log, "Extended retention time for blobs", result)

	return result, nil
}

// isRetentionShortenRefusal reports whether an ExtendBlobRetention error is the storage
// backend refusing to move an existing Compliance-mode retain-until date EARLIER.
//
// PNWSoft patch. S3-compatible backends surface this as a 403 whose message names the
// object lock / retention rather than as a distinct typed error, so this has to match on
// the message. It is deliberately narrow, and narrowness is not the only guard: a
// credential or permission fault would refuse EVERY blob, and the caller treats
// "refused everything, extended nothing" as fatal. So a false positive here cannot
// silently disable the phase.
func isRetentionShortenRefusal(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	// Must be about retention/object-lock at all, or it is some unrelated failure.
	if !strings.Contains(msg, "retention") &&
		!strings.Contains(msg, "object lock") &&
		!strings.Contains(msg, "objectlock") {
		return false
	}

	for _, marker := range []string{
		"access denied",
		"accessdenied",
		"invalid request",
		"invalidrequest",
		"cannot be decreased",
		"cannot be shortened",
		"shorten",
		"earlier",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}

	return false
}

// CheckExtendRetention verifies if extension can be enabled due to maintenance and blob parameters.
//
// PNWSoft note: this validates blobCfg.RetentionPeriod, which under two-tier retention is
// the SHORTER data-pack tier. That is the correct one to gate on -- the metadata tier is
// never shorter (see repo.MetadataRetentionPeriod), so a config satisfying the 24h margin
// for packs satisfies it for metadata too.
func CheckExtendRetention(ctx context.Context, blobCfg format.BlobStorageConfiguration, p *Params) error {
	if !p.ExtendObjectLocks {
		return nil
	}

	if !p.FullCycle.Enabled {
		userLog(ctx).Warn("Object Lock extension will not function because Full-Maintenance is disabled")
	}

	if blobCfg.RetentionPeriod > 0 && blobCfg.RetentionPeriod-p.FullCycle.Interval < minRetentionMaintenanceDiff {
		return errors.Errorf("The repo RetentionPeriod must be %v greater than the Full Maintenance interval %v %v", minRetentionMaintenanceDiff, blobCfg.RetentionPeriod, p.FullCycle.Interval)
	}

	return nil
}
