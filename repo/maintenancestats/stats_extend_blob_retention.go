package maintenancestats

import (
	"fmt"

	"github.com/kopia/kopia/internal/contentlog"
)

const extendBlobRetentionStatsKind = "extendBlobRetentionStats"

// ExtendBlobRetentionStats are the stats for extending blob retention time.
type ExtendBlobRetentionStats struct {
	ToExtendBlobCount            uint64 `json:"toExtendBlobCount"`
	ExtendedBlobCount            uint64 `json:"extendedBlobCount"`
	SkippedUnreferencedBlobCount uint64 `json:"skippedUnreferencedBlobCount"`
	RetentionPeriod              string `json:"retentionPeriod"`

	// PNWSoft patch: two-tier retention. RetentionPeriod above is the data-pack tier;
	// these report the metadata tier and how many blobs received it, so the split is
	// visible in `kopia maintenance info` and can be tracked by Validate-B2Cleanup.ps1.
	MetadataBlobCount       uint64 `json:"metadataBlobCount"`
	MetadataRetentionPeriod string `json:"metadataRetentionPeriod"`

	// RefusedShortenCount counts blobs whose existing lock was longer than the requested
	// retain-until, so the backend refused to move it earlier. Expected and self-clearing
	// while the data-pack tier is being lowered; should decay to 0 within one old period.
	RefusedShortenCount uint64 `json:"refusedShortenCount"`
}

// WriteValueTo writes the stats to JSONWriter.
func (es *ExtendBlobRetentionStats) WriteValueTo(jw *contentlog.JSONWriter) {
	jw.BeginObjectField(es.Kind())
	jw.UInt64Field("toExtendBlobCount", es.ToExtendBlobCount)
	jw.UInt64Field("extendedBlobCount", es.ExtendedBlobCount)
	jw.UInt64Field("skippedUnreferencedBlobCount", es.SkippedUnreferencedBlobCount)
	jw.StringField("retentionPeriod", es.RetentionPeriod)
	jw.UInt64Field("metadataBlobCount", es.MetadataBlobCount)
	jw.StringField("metadataRetentionPeriod", es.MetadataRetentionPeriod)
	jw.UInt64Field("refusedShortenCount", es.RefusedShortenCount)
	jw.EndObject()
}

// Summary generates a human readable summary for the stats.
func (es *ExtendBlobRetentionStats) Summary() string {
	return fmt.Sprintf("Blob retention extension found %v blobs, extended %v, skipped %v unreferenced, retention period %v (metadata tier %v on %v blobs, %v refused as already-longer)",
		es.ToExtendBlobCount, es.ExtendedBlobCount, es.SkippedUnreferencedBlobCount, es.RetentionPeriod,
		es.MetadataRetentionPeriod, es.MetadataBlobCount, es.RefusedShortenCount)
}

// Kind returns the kind name for the stats.
func (es *ExtendBlobRetentionStats) Kind() string {
	return extendBlobRetentionStatsKind
}
