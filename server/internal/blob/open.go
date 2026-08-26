package blob

import (
	"fmt"
	"log/slog"
)

// Open builds the blob store a process should use, with or without a cold tier.
//
// It exists so the API, the worker and cloudctl cannot disagree about what the
// store IS. That is not a tidiness argument: fsck decides whether an absence is
// data loss by asking the store whether a cold tier exists, so a cloudctl that
// built a plain FSStore while the API ran a tiered one would report every cold
// object as missing — and `--repair` acts on that judgement. Three call sites
// constructing this by hand is exactly how those three drift apart.
//
// A nil cold configuration returns the local store unwrapped, which is the
// state every deployment without a cold tier is in and the one every read path
// was written against.
//
// This package takes S3Config rather than the config package's settings so the
// dependency does not run the wrong way: blob is the bottom of the stack, and
// the translation from environment variables belongs to whoever read them.
func Open(path string, cold *S3Config, log *slog.Logger) (*FSStore, Store, error) {
	hot, err := NewFSStore(path)
	if err != nil {
		return nil, nil, err
	}
	if cold == nil {
		return hot, hot, nil
	}
	coldStore, err := NewS3Store(*cold)
	if err != nil {
		// Refused rather than degraded to hot-only. A process that silently ran
		// without the tier would answer "not here" for content the database says
		// is cold, and the caller least able to tell the difference is fsck.
		return nil, nil, fmt.Errorf("cold tier: %w", err)
	}
	tiered := NewTieredStore(hot, coldStore, log)
	return hot, tiered, nil
}
