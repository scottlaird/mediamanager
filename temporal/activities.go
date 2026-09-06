// Package temporal runs the ingest steps as Temporal workflows.
//
// The catalog stays the source of truth for where copies are; Temporal
// holds only in-flight state: which assets are mid-copy, what to retry,
// what to wait for. Every activity is one ingest step, idempotent and
// resumable on its own, so a retry after a crash costs at most one buffer.
package temporal

import (
	"context"
	"errors"

	"github.com/scottlaird/mediamanager/copyfile"
	"github.com/scottlaird/mediamanager/ingest"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// Error types workflows branch on. Anything else is retried per policy.
const (
	// ErrTypeSpoolFull: no spool has room; archive from the source instead.
	ErrTypeSpoolFull = "SpoolFull"
	// ErrTypeConflict: the destination holds different content, or the
	// source is not the file the catalog says. Retrying cannot help.
	ErrTypeConflict = "Conflict"
)

// Activities wraps an ingest.Env. One value is registered per worker.
type Activities struct {
	Env *ingest.Env
}

// Relink audits and reconciles every link tree.
func (a *Activities) Relink(ctx context.Context) error {
	_, err := a.Env.Relink(ctx)
	return err
}

// ScanSource registers everything on a mounted source without copying.
func (a *Activities) ScanSource(ctx context.Context, root string) (*ingest.Source, error) {
	return a.Env.ScanSource(ctx, root)
}

// IsArchived reports whether the asset has a complete copy on a mounted NAS.
func (a *Activities) IsArchived(ctx context.Context, id string) (bool, error) {
	return a.Env.IsArchived(ctx, id)
}

// Spool copies one asset to a spool, heartbeating as bytes move.
func (a *Activities) Spool(ctx context.Context, id string) (ingest.CopyResult, error) {
	r, err := a.Env.SpoolAsset(heartbeating(ctx), id)
	return r, classify(err)
}

// Archive copies one asset to every NAS lacking it, heartbeating as bytes
// move. Workflows run it on the NAS task queue, whose worker caps how many
// run at once.
func (a *Activities) Archive(ctx context.Context, id string) ([]ingest.CopyResult, error) {
	r, err := a.Env.ArchiveAsset(heartbeating(ctx), id)
	return r, classify(err)
}

// NeedsArchive lists assets lacking a NAS copy.
func (a *Activities) NeedsArchive(ctx context.Context) ([]string, error) {
	return a.Env.NeedsArchiveIDs(ctx)
}

// Flush evicts verified spool copies.
func (a *Activities) Flush(ctx context.Context, spool string, opts ingest.FlushOptions) (ingest.FlushReport, error) {
	return a.Env.FlushSpool(ctx, spool, opts)
}

// heartbeating forwards copy progress to Temporal so a stalled copy is
// noticed by the heartbeat timeout and a retried one shows where it was.
func heartbeating(ctx context.Context) context.Context {
	return ingest.WithProgress(ctx, func(done, total int64) {
		activity.RecordHeartbeat(ctx, done, total)
	})
}

// classify turns ingest errors that retrying cannot fix into typed,
// non-retryable application errors.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ingest.ErrSpoolFull):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeSpoolFull, err)
	case errors.Is(err, copyfile.ErrExists), errors.Is(err, copyfile.ErrSourceMismatch), errors.Is(err, copyfile.ErrVerify):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeConflict, err)
	}
	return err
}
