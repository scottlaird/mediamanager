// Package temporal runs the ingest steps as Temporal workflows.
//
// The catalog stays the source of truth for where copies are; Temporal
// holds only in-flight state: which assets are mid-copy, what to retry,
// what to wait for. Every activity is one ingest step, idempotent and
// resumable on its own, so a retry after a crash costs at most one buffer.
//
// Payloads are kept small on purpose: activities exchange asset IDs and
// short refs, never file listings, because Temporal caps a payload at 2 MiB
// and a card can hold thousands of files.
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
	// ErrTypeInterrupted: the worker was stopping and cancelled the
	// activity's context mid-copy. Retryable: the next worker resumes the
	// copy from its .partial.
	ErrTypeInterrupted = "Interrupted"
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
	return classify(err)
}

// ScanSource registers everything on a mounted source without copying,
// and returns IDs and counts rather than the listing.
func (a *Activities) ScanSource(ctx context.Context, root string) (*ingest.ScanSummary, error) {
	src, err := a.Env.ScanSource(ctx, root)
	if err != nil {
		return nil, classify(err)
	}
	return src.Summary(), nil
}

// Refs describes a page of assets by ID.
func (a *Activities) Refs(ctx context.Context, ids []string) ([]ingest.AssetRef, error) {
	return a.Env.Refs(ctx, ids)
}

// IsArchived reports whether the asset has a complete copy on a mounted NAS.
func (a *Activities) IsArchived(ctx context.Context, id string) (bool, error) {
	return a.Env.IsArchived(ctx, id)
}

// AllArchivedOn reports whether every asset on a source is on a mounted NAS.
func (a *Activities) AllArchivedOn(ctx context.Context, source string) (bool, error) {
	return a.Env.AllArchivedOn(ctx, source)
}

// SourceAssetIDs pages the assets registered on a source.
func (a *Activities) SourceAssetIDs(ctx context.Context, source string, offset, limit int) ([]string, error) {
	return a.Env.SourceAssetIDs(ctx, source, offset, limit)
}

// Spool copies one asset to a spool, heartbeating as bytes move.
func (a *Activities) Spool(ctx context.Context, ref ingest.AssetRef) (ingest.CopyResult, error) {
	r, err := a.Env.SpoolAsset(heartbeating(ctx, ref), ref.ID)
	return r, classify(err)
}

// Archive copies one asset to every NAS lacking it, heartbeating as bytes
// move. Workflows run it on the NAS task queue, whose worker caps how many
// run at once.
func (a *Activities) Archive(ctx context.Context, ref ingest.AssetRef) ([]ingest.CopyResult, error) {
	r, err := a.Env.ArchiveAsset(heartbeating(ctx, ref), ref.ID)
	return r, classify(err)
}

// SpoolBatch spools a list of small assets in one activity, heartbeating
// per item, so a card of thousands of photos costs a few dozen activities
// rather than thousands. Per-item failures are in the result.
func (a *Activities) SpoolBatch(ctx context.Context, refs []ingest.AssetRef) ([]ingest.BatchItem, error) {
	items, err := a.Env.SpoolBatch(batchHeartbeating(ctx), refs)
	return items, classify(err)
}

// ArchiveBatch archives a list of small assets in one activity on the NAS
// queue; one batch holds one NAS slot.
func (a *Activities) ArchiveBatch(ctx context.Context, refs []ingest.AssetRef) ([]ingest.BatchItem, error) {
	items, err := a.Env.ArchiveBatch(batchHeartbeating(ctx), refs)
	return items, classify(err)
}

// Pin marks assets to be kept through flushes.
func (a *Activities) Pin(ctx context.Context, ids []string, pinned bool) error {
	for _, id := range ids {
		if err := a.Env.Catalog.SetPinned(ctx, id, pinned); err != nil {
			return err
		}
	}
	return nil
}

// NeedsArchive pages the IDs of assets lacking a NAS copy.
func (a *Activities) NeedsArchive(ctx context.Context, offset, limit int) ([]string, error) {
	return a.Env.NeedsArchivePage(ctx, offset, limit)
}

// Flush evicts verified spool copies.
func (a *Activities) Flush(ctx context.Context, spool string, opts ingest.FlushOptions) (ingest.FlushReport, error) {
	return a.Env.FlushSpool(ctx, spool, opts)
}

// Heartbeat is what a copy activity reports as it runs; it shows in the
// UI as the pending activity's heartbeat details. Index and Count are set
// by batch activities.
type Heartbeat struct {
	Path    string
	Done    int64
	Total   int64
	Percent int
	Index   int
	Count   int
}

// heartbeating forwards copy progress to Temporal so a stalled copy is
// noticed by the heartbeat timeout and a running one can be read at a
// glance.
func heartbeating(ctx context.Context, ref ingest.AssetRef) context.Context {
	return ingest.WithProgress(ctx, func(done, total int64) {
		activity.RecordHeartbeat(ctx, Heartbeat{Path: ref.Path, Done: done, Total: total, Percent: int(done * 100 / max(total, 1))})
	})
}

func batchHeartbeating(ctx context.Context) context.Context {
	return ingest.WithBatchProgress(ctx, func(p ingest.BatchProgress) {
		activity.RecordHeartbeat(ctx, Heartbeat{Path: p.Ref.Path, Done: p.Done, Total: p.Total,
			Percent: int(p.Done * 100 / max(p.Total, 1)), Index: p.Index + 1, Count: p.Count})
	})
}

// classify turns ingest errors that retrying cannot fix into typed,
// non-retryable application errors, and a worker-shutdown cancellation
// into a retryable one. Returning context.Canceled itself would record
// the activity as canceled, which Temporal does not retry, so a worker
// restart would fail every copy in flight.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return temporal.NewApplicationError("interrupted by worker shutdown; will resume", ErrTypeInterrupted, err)
	case errors.Is(err, ingest.ErrSpoolFull):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeSpoolFull, err)
	case errors.Is(err, copyfile.ErrExists), errors.Is(err, copyfile.ErrSourceMismatch), errors.Is(err, copyfile.ErrVerify):
		return temporal.NewNonRetryableApplicationError(err.Error(), ErrTypeConflict, err)
	}
	return err
}
