package repo

import (
	"testing"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/format"
)

// PNWSoft patch tests. The tier split is an allowlist guarding an IRREVERSIBLE action:
// Compliance-mode object locks can only ever be extended, so a blob wrongly promoted to
// the long tier cannot be walked back -- you wait out the period. These tests pin the
// classification so a future prefix change cannot silently widen it.

func TestIsMetadataLockingBlob(t *testing.T) {
	cases := []struct {
		id   blob.ID
		want bool
	}{
		// Metadata tier -- sets the point-in-time horizon.
		{"n0123456789abcdef", true},  // legacy index
		{"xn0123456789abcdef", true}, // epoch: uncompacted index
		{"xe0123456789abcdef", true}, // epoch: marker
		{"xs0123456789abcdef", true}, // epoch: single-epoch compaction
		{"xr0123456789abcdef", true}, // epoch: range checkpoint
		{"xw0123456789abcdef", true}, // epoch: deletion watermark
		{"q0123456789abcdef", true},  // metadata/manifest packs
		{blob.ID(format.KopiaRepositoryBlobID), true},
		{blob.ID(format.KopiaBlobCfgBlobID), true},

		// Data-pack tier -- the cost lever. Must NOT be promoted.
		{"p0123456789abcdef", false},
		{"pdeadbeef", false},

		// Not in the locking set at all; must fall through to the short tier.
		{"s0123456789abcdef", false}, // session blobs
		{"_log_0123", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := IsMetadataLockingBlob(tc.id); got != tc.want {
			t.Errorf("IsMetadataLockingBlob(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestMetadataRetentionPeriodNeverShorterThanBase(t *testing.T) {
	// A base longer than the metadata default must win: the tiering may only ever
	// lengthen retention relative to stock kopia, never weaken it.
	base := 90 * 24 * time.Hour

	if got := MetadataRetentionPeriod(base); got != base {
		t.Errorf("MetadataRetentionPeriod(%v) = %v, want %v (must not shorten)", base, got, base)
	}
}

func TestMetadataRetentionPeriodDefault(t *testing.T) {
	t.Setenv(MetadataRetentionEnvVar, "")

	base := 14 * 24 * time.Hour

	if got := MetadataRetentionPeriod(base); got != DefaultMetadataRetentionPeriod {
		t.Errorf("MetadataRetentionPeriod(%v) = %v, want default %v", base, got, DefaultMetadataRetentionPeriod)
	}
}

func TestMetadataRetentionPeriodEnvOverride(t *testing.T) {
	base := 14 * 24 * time.Hour

	t.Setenv(MetadataRetentionEnvVar, "1440") // 60 days

	if got, want := MetadataRetentionPeriod(base), 60*24*time.Hour; got != want {
		t.Errorf("MetadataRetentionPeriod with env=1440 = %v, want %v", got, want)
	}

	// Garbage and non-positive values must fall back to the default, not to zero --
	// a zero period would disable object locking entirely.
	for _, bad := range []string{"abc", "0", "-5", " "} {
		t.Setenv(MetadataRetentionEnvVar, bad)

		if got := MetadataRetentionPeriod(base); got != DefaultMetadataRetentionPeriod {
			t.Errorf("MetadataRetentionPeriod with env=%q = %v, want default %v", bad, got, DefaultMetadataRetentionPeriod)
		}
	}
}

func TestRetentionPeriodForBlobSelectsTier(t *testing.T) {
	t.Setenv(MetadataRetentionEnvVar, "")

	cfg := format.BlobStorageConfiguration{
		RetentionMode:   "COMPLIANCE",
		RetentionPeriod: 14 * 24 * time.Hour,
	}

	if got := RetentionPeriodForBlob("p0123", cfg); got != cfg.RetentionPeriod {
		t.Errorf("data pack got %v, want configured %v", got, cfg.RetentionPeriod)
	}

	if got := RetentionPeriodForBlob("xn0123", cfg); got != DefaultMetadataRetentionPeriod {
		t.Errorf("metadata blob got %v, want metadata tier %v", got, DefaultMetadataRetentionPeriod)
	}
}
