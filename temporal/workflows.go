package temporal

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/ingest"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Queues splits work between two task queues: everything on Main, NAS
// copies on NAS so one worker setting caps concurrent NAS writes.
type Queues struct {
	Main string
	NAS  string
}

// QueuesFor derives the pair from the configured base name.
func QueuesFor(base string) Queues { return Queues{Main: base, NAS: base + "-nas"} }

// Workflow IDs. Fixed per subject so the same card or asset is never worked
// on twice at once and a second start joins the first.
func ImportWorkflowID(source string) string { return "import:" + source }
func FlushWorkflowID(spool string) string   { return "flush:" + spool }
func BacklogWorkflowID() string             { return "archive-backlog" }
func SpoolWorkflowID(tag string) string     { return "spool:" + tag }

// ArchiveWorkflowID names an archive by the asset's path rather than its
// identity, so a list of running workflows reads as filenames. The path is
// unique per asset (the catalog enforces it per kind) and, for video and
// audio, already carries the identity.
func ArchiveWorkflowID(ref ingest.AssetRef) string { return "archive:" + ref.Path }

// Sizing. Temporal caps a payload at 2 MiB and a workflow history at
// ~50K events, and a stills card can hold thousands of files, so:
const (
	// refPage is how many asset IDs one Refs activity expands.
	refPage = 200
	// batchSize is how many small assets one SpoolBatch or ArchiveBatch
	// activity handles.
	batchSize = 50
	// batchBelow is the size under which an asset goes through a batch
	// rather than its own activity and archive workflow. Above it, the
	// per-file workflow with its own ID, heartbeat and result is worth
	// the history it costs.
	batchBelow = 1 << 30
	// maxListed bounds the per-file lists carried in a result.
	maxListed = 50
)

// ImportResult is what ImportSource returns; it is the Temporal form of
// ingest.Summary. Per-file lists are capped at maxListed entries; the
// counts are exact.
type ImportResult struct {
	Source       string
	Shape        string
	Assets       int
	New          int
	Spooled      int
	Archived     int
	SpoolFull    []string
	Failed       int
	Failures     []string
	Unrouted     int
	Orphans      int
	Unrecognised int
	Examples     []string
	SafeToFormat bool
}

func (r *ImportResult) fail(msg string) {
	r.Failed++
	if len(r.Failures) < maxListed {
		r.Failures = append(r.Failures, msg)
	}
}

var (
	quickOpts = workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute, // a scan of a big card over USB can take a while
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 5 * time.Second, MaximumInterval: time.Minute, MaximumAttempts: 5},
	}
	copyOpts = workflow.ActivityOptions{
		StartToCloseTimeout: 7 * 24 * time.Hour, // one copy of one file, however large
		HeartbeatTimeout:    10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:        10 * time.Second,
			BackoffCoefficient:     2,
			MaximumInterval:        10 * time.Minute,
			MaximumAttempts:        20,
			NonRetryableErrorTypes: []string{ErrTypeSpoolFull, ErrTypeConflict},
		},
	}
)

// idPage is how many IDs one paging activity returns.
const idPage = 1000

// pageIDs drains a paged ID activity into one slice held in workflow
// memory (never in one payload).
func pageIDs(ctx workflow.Context, fetch func(offset, limit int) workflow.Future) ([]string, error) {
	var all []string
	for offset := 0; ; offset += idPage {
		var page []string
		if err := fetch(offset, idPage).Get(ctx, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < idPage {
			return all, nil
		}
	}
}

// refsFor expands asset IDs into refs, a page at a time.
func refsFor(ctx workflow.Context, ids []string) ([]ingest.AssetRef, error) {
	var acts *Activities
	quick := workflow.WithActivityOptions(ctx, quickOpts)
	out := make([]ingest.AssetRef, 0, len(ids))
	for i := 0; i < len(ids); i += refPage {
		end := min(i+refPage, len(ids))
		var page []ingest.AssetRef
		if err := workflow.ExecuteActivity(quick, acts.Refs, ids[i:end]).Get(ctx, &page); err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	return out, nil
}

// split separates refs into those that get their own workflow and those
// handled in batches.
func split(refs []ingest.AssetRef) (big, small []ingest.AssetRef) {
	for _, r := range refs {
		if r.Size >= batchBelow {
			big = append(big, r)
		} else {
			small = append(small, r)
		}
	}
	return big, small
}

func chunks(refs []ingest.AssetRef) [][]ingest.AssetRef {
	var out [][]ingest.AssetRef
	for i := 0; i < len(refs); i += batchSize {
		out = append(out, refs[i:min(i+batchSize, len(refs))])
	}
	return out
}

// ImportSource is one card or camera, start to finish: audit, register,
// link, spool (one reader per source), and hand everything to archive
// workflows that outlive this one, so pulling a card never stops a NAS
// copy already under way. Large files get a Spool activity and an
// ArchiveAsset workflow each; small ones go through batches. Anything
// already needing archive from earlier runs is handed off too.
func ImportSource(ctx workflow.Context, root string, q Queues) (*ImportResult, error) {
	var acts *Activities
	quick := workflow.WithActivityOptions(ctx, quickOpts)
	spool := workflow.WithActivityOptions(ctx, copyOpts)

	if err := workflow.ExecuteActivity(quick, acts.Relink).Get(ctx, nil); err != nil {
		return nil, err
	}
	var scan ingest.ScanSummary
	if err := workflow.ExecuteActivity(quick, acts.ScanSource, root).Get(ctx, &scan); err != nil {
		return nil, err
	}
	res := &ImportResult{
		Source: scan.Source, Shape: scan.Shape, Assets: scan.Assets, New: scan.New,
		Unrouted: scan.Unrouted, Orphans: scan.Orphans, Unrecognised: scan.Unrecognised, Examples: scan.Examples,
	}
	if err := workflow.ExecuteActivity(quick, acts.Relink).Get(ctx, nil); err != nil {
		return nil, err
	}
	ids, err := pageIDs(ctx, func(offset, limit int) workflow.Future {
		return workflow.ExecuteActivity(quick, acts.SourceAssetIDs, scan.Source, offset, limit)
	})
	if err != nil {
		return nil, err
	}
	refs, err := refsFor(ctx, dedupe(ids))
	if err != nil {
		return nil, err
	}
	big, small := split(refs)

	children := map[string]workflow.ChildWorkflowFuture{}
	handoff := func(ref ingest.AssetRef) {
		if _, done := children[ref.ID]; !done {
			children[ref.ID] = startArchive(ctx, ref, q)
		}
	}
	// Large files first, one at a time: a card reader is one stream.
	for i, ref := range big {
		workflow.SetCurrentDetails(ctx, fmt.Sprintf("spooling %d/%d large: %s (%s)", i+1, len(big), ref.Path, fmtBytes(ref.Size)))
		var archived bool
		if err := workflow.ExecuteActivity(quick, acts.IsArchived, ref.ID).Get(ctx, &archived); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			res.fail(ref.Path + ": " + err.Error())
			continue
		}
		if !archived {
			var r ingest.CopyResult
			err := workflow.ExecuteActivity(withSummary(spool, ref.Path), acts.Spool, ref).Get(ctx, &r)
			switch {
			case isType(err, ErrTypeSpoolFull):
				if len(res.SpoolFull) < maxListed {
					res.SpoolFull = append(res.SpoolFull, ref.Path)
				}
			case err != nil:
				if c := stopIfCancelled(ctx); c != nil {
					return nil, c
				}
				res.fail(ref.Path + ": spool: " + err.Error())
				continue
			case !r.Skipped:
				res.Spooled++
			}
		}
		handoff(ref) // a no-op copy for archived assets, but it refreshes sidecars
	}
	// Small files in batches: one activity spools fifty, one child
	// workflow archives them.
	batches := chunks(small)
	var batchFutures []workflow.ChildWorkflowFuture
	for i, batch := range batches {
		workflow.SetCurrentDetails(ctx, fmt.Sprintf("spooling batch %d/%d (%d files)", i+1, len(batches), len(batch)))
		var items []ingest.BatchItem
		if err := workflow.ExecuteActivity(withSummary(spool, batchLabel(batch)), acts.SpoolBatch, batch).Get(ctx, &items); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			res.fail(fmt.Sprintf("batch %d: spool: %v", i+1, err))
			continue
		}
		var toArchive []ingest.AssetRef
		for j, it := range items {
			switch {
			case it.SpoolFull:
				if len(res.SpoolFull) < maxListed {
					res.SpoolFull = append(res.SpoolFull, it.Path)
				}
			case it.Err != "":
				res.fail(it.Path + ": spool: " + it.Err)
				continue
			case !it.Skipped:
				res.Spooled++
			}
			toArchive = append(toArchive, batch[j])
		}
		if len(toArchive) > 0 {
			batchFutures = append(batchFutures, startArchiveBatch(ctx, toArchive, q))
		}
	}

	// Leftovers from earlier runs, wherever their copy is now.
	leftoverIDs, err := pageIDs(ctx, func(offset, limit int) workflow.Future {
		return workflow.ExecuteActivity(quick, acts.NeedsArchive, offset, limit)
	})
	if err == nil {
		seen := map[string]bool{}
		for _, r := range refs {
			seen[r.ID] = true
		}
		var extra []string
		for _, id := range leftoverIDs {
			if !seen[id] {
				extra = append(extra, id)
			}
		}
		if lrefs, err := refsFor(ctx, extra); err == nil {
			lbig, lsmall := split(lrefs)
			for _, ref := range lbig {
				handoff(ref)
			}
			for _, batch := range chunks(lsmall) {
				batchFutures = append(batchFutures, startArchiveBatch(ctx, batch, q))
			}
		}
	}

	workflow.SetCurrentDetails(ctx, fmt.Sprintf("waiting for %d archive workflows and %d batches", len(children), len(batchFutures)))
	for id, f := range children {
		var ar ArchiveResult
		if err := f.Get(ctx, &ar); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c // the children are abandoned to finish on their own
			}
			if !alreadyRunning(err) {
				res.fail(id + ": archive: " + err.Error())
			}
			continue
		}
		res.Archived += ar.Copies
	}
	for i, f := range batchFutures {
		var br BatchResult
		if err := f.Get(ctx, &br); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			res.fail(fmt.Sprintf("archive batch %d: %v", i+1, err))
			continue
		}
		res.Archived += br.Archived
		res.Failed += br.Failed
		for _, m := range br.Failures {
			if len(res.Failures) < maxListed {
				res.Failures = append(res.Failures, m)
			}
		}
	}

	var safe bool
	if scan.Assets > 0 {
		if err := workflow.ExecuteActivity(quick, acts.AllArchivedOn, scan.Source).Get(ctx, &safe); err != nil {
			safe = false
		}
	}
	res.SafeToFormat = safe
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("%d assets, %d new, %d spooled, %d archived, %d failed; safe to format: %v",
		res.Assets, res.New, res.Spooled, res.Archived, res.Failed, res.SafeToFormat))
	return res, nil
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func batchLabel(batch []ingest.AssetRef) string {
	if len(batch) == 0 {
		return "empty batch"
	}
	return fmt.Sprintf("%s +%d", batch[0].Path, len(batch)-1)
}

// startArchive starts (or joins) the ArchiveAsset workflow for an asset.
// The path is the workflow's summary in the UI, so a list of them reads
// as filenames rather than identities.
func startArchive(ctx workflow.Context, ref ingest.AssetRef, q Queues) workflow.ChildWorkflowFuture {
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            ArchiveWorkflowID(ref),
		TaskQueue:             q.Main,
		ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_ABANDON,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		StaticSummary:         ref.Path,
		StaticDetails:         fmt.Sprintf("%s, asset %s", fmtBytes(ref.Size), ref.ID),
	})
	return workflow.ExecuteChildWorkflow(cctx, ArchiveAsset, ref, q)
}

// startArchiveBatch starts an ArchiveBatch child for a group of small
// assets. Its ID carries the first path and the count; unlike a per-asset
// archive it is not deduplicated, since each batch is a one-off grouping.
func startArchiveBatch(ctx workflow.Context, batch []ingest.AssetRef, q Queues) workflow.ChildWorkflowFuture {
	var total int64
	for _, r := range batch {
		total += r.Size
	}
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        "archive-batch:" + batchLabel(batch) + ":" + workflow.Now(ctx).UTC().Format("20060102T150405.000Z"),
		TaskQueue:         q.Main,
		ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
		StaticSummary:     batchLabel(batch),
		StaticDetails:     fmt.Sprintf("%d files, %s", len(batch), fmtBytes(total)),
	})
	return workflow.ExecuteChildWorkflow(cctx, ArchiveBatch, batch, q)
}

// ArchiveResult is what ArchiveAsset returns: how many copies it made and
// how the copying went, so the workflow result reads as a report.
type ArchiveResult struct {
	Path string
	// Copies is the number of NAS locations that received the asset.
	Copies    int
	Locations []string
	// Skipped lists NAS locations that already had it.
	Skipped []string
	// Bytes is the size of the asset; Copied is what actually moved,
	// summed over locations; Duration and MiBPerSecond cover the copies.
	Bytes        int64
	Copied       int64
	Duration     time.Duration
	MiBPerSecond float64
}

// ArchiveAsset copies one asset to every NAS lacking it, on the NAS queue.
// It is its own workflow so that it survives the import that started it
// and so two imports of the same content share one copy.
func ArchiveAsset(ctx workflow.Context, ref ingest.AssetRef, q Queues) (*ArchiveResult, error) {
	var acts *Activities
	opts := copyOpts
	opts.TaskQueue = q.NAS
	opts.Summary = ref.Path
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("copying %s (%s) to the NAS", ref.Path, fmtBytes(ref.Size)))
	var results []ingest.CopyResult
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), acts.Archive, ref).Get(ctx, &results); err != nil {
		workflow.SetCurrentDetails(ctx, "failed: "+err.Error())
		return nil, err
	}
	res := &ArchiveResult{Path: ref.Path, Bytes: ref.Size}
	for _, r := range results {
		if r.Skipped {
			res.Skipped = append(res.Skipped, r.Location)
			continue
		}
		res.Copies++
		res.Locations = append(res.Locations, r.Location)
		res.Copied += r.Copied
		res.Duration += r.Duration
	}
	if res.Duration > 0 {
		res.MiBPerSecond = float64(res.Copied) / (1 << 20) / res.Duration.Seconds()
	}
	switch res.Copies {
	case 0:
		workflow.SetCurrentDetails(ctx, "already on the NAS; sidecars refreshed")
	default:
		workflow.SetCurrentDetails(ctx, fmt.Sprintf("copied to %s: %s in %s (%.0f MiB/s)",
			strings.Join(res.Locations, ", "), fmtBytes(res.Copied), res.Duration.Round(time.Second), res.MiBPerSecond))
	}
	return res, nil
}

// BatchResult is what ArchiveBatch returns.
type BatchResult struct {
	Files        int
	Archived     int
	Skipped      int
	Failed       int
	Failures     []string
	Copied       int64
	Duration     time.Duration
	MiBPerSecond float64
}

// ArchiveBatch archives a group of small assets in one activity on the
// NAS queue, holding one NAS slot for the whole group.
func ArchiveBatch(ctx workflow.Context, batch []ingest.AssetRef, q Queues) (*BatchResult, error) {
	var acts *Activities
	opts := copyOpts
	opts.TaskQueue = q.NAS
	opts.Summary = batchLabel(batch)
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("archiving %d files", len(batch)))
	var items []ingest.BatchItem
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), acts.ArchiveBatch, batch).Get(ctx, &items); err != nil {
		workflow.SetCurrentDetails(ctx, "failed: "+err.Error())
		return nil, err
	}
	res := &BatchResult{Files: len(batch)}
	for _, it := range items {
		switch {
		case it.Err != "":
			res.Failed++
			if len(res.Failures) < maxListed {
				res.Failures = append(res.Failures, it.Path+": "+it.Err)
			}
		case it.Skipped:
			res.Skipped++
		default:
			res.Archived += it.Copies
			res.Copied += it.Copied
			res.Duration += it.Duration
		}
	}
	if res.Duration > 0 {
		res.MiBPerSecond = float64(res.Copied) / (1 << 20) / res.Duration.Seconds()
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("%d archived, %d already there, %d failed; %s at %.0f MiB/s",
		res.Archived, res.Skipped, res.Failed, fmtBytes(res.Copied), res.MiBPerSecond))
	return res, nil
}

// withSummary labels an activity with the file it works on.
func withSummary(ctx workflow.Context, summary string) workflow.Context {
	opts := workflow.GetActivityOptions(ctx)
	opts.Summary = summary
	return workflow.WithActivityOptions(ctx, opts)
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// BacklogResult is what ArchiveBacklog returns. Duration is summed over
// copies, so with several running at once it exceeds wall time and the
// rate is per copy, not aggregate.
type BacklogResult struct {
	Assets       int
	Archived     int
	Failed       int
	Failures     []string
	Copied       int64
	Duration     time.Duration
	MiBPerSecond float64
}

// ArchiveBacklog hands every asset lacking a NAS copy to ArchiveAsset or
// an ArchiveBatch and waits for them: `mm archive`.
func ArchiveBacklog(ctx workflow.Context, q Queues) (*BacklogResult, error) {
	var acts *Activities
	quick := workflow.WithActivityOptions(ctx, quickOpts)
	ids, err := pageIDs(ctx, func(offset, limit int) workflow.Future {
		return workflow.ExecuteActivity(quick, acts.NeedsArchive, offset, limit)
	})
	if err != nil {
		return nil, err
	}
	refs, err := refsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	res := &BacklogResult{Assets: len(refs)}
	big, small := split(refs)
	var total int64
	futures := map[string]workflow.ChildWorkflowFuture{}
	for _, ref := range big {
		futures[ref.ID] = startArchive(ctx, ref, q)
		total += ref.Size
	}
	var batches []workflow.ChildWorkflowFuture
	for _, batch := range chunks(small) {
		batches = append(batches, startArchiveBatch(ctx, batch, q))
		for _, r := range batch {
			total += r.Size
		}
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("waiting for %d archive workflows and %d batches, %s", len(futures), len(batches), fmtBytes(total)))
	for id, f := range futures {
		var ar ArchiveResult
		if err := f.Get(ctx, &ar); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			if !alreadyRunning(err) {
				res.Failed++
				if len(res.Failures) < maxListed {
					res.Failures = append(res.Failures, id+": "+err.Error())
				}
			}
			continue
		}
		res.Archived += ar.Copies
		res.Copied += ar.Copied
		res.Duration += ar.Duration
	}
	for i, f := range batches {
		var br BatchResult
		if err := f.Get(ctx, &br); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			res.Failed++
			if len(res.Failures) < maxListed {
				res.Failures = append(res.Failures, fmt.Sprintf("batch %d: %v", i+1, err))
			}
			continue
		}
		res.Archived += br.Archived
		res.Failed += br.Failed
		res.Copied += br.Copied
		res.Duration += br.Duration
		for _, m := range br.Failures {
			if len(res.Failures) < maxListed {
				res.Failures = append(res.Failures, m)
			}
		}
	}
	if res.Duration > 0 {
		res.MiBPerSecond = float64(res.Copied) / (1 << 20) / res.Duration.Seconds()
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("%d of %d archived, %d failed; %s at %.0f MiB/s per copy",
		res.Archived, res.Assets, res.Failed, fmtBytes(res.Copied), res.MiBPerSecond))
	return res, nil
}

// FlushSpool evicts verified spool copies: `mm flush`. One activity, since
// the work is deletes and small reads.
func FlushSpool(ctx workflow.Context, spool string, opts ingest.FlushOptions) (ingest.FlushReport, error) {
	var acts *Activities
	var rep ingest.FlushReport
	fo := quickOpts
	fo.StartToCloseTimeout = 12 * time.Hour // sparse checks over SMB for many assets
	fo.HeartbeatTimeout = 0
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, fo), acts.Flush, spool, opts).Get(ctx, &rep)
	return rep, err
}

// stopIfCancelled returns the workflow's cancellation, if any, so a loop
// that tolerates per-asset failures still ends promptly and as cancelled
// when the workflow itself is cancelled, rather than recording every
// remaining asset as failed.
func stopIfCancelled(ctx workflow.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func isType(err error, typ string) bool {
	var app *temporal.ApplicationError
	return errors.As(err, &app) && app.Type() == typ
}

// alreadyRunning reports the error a child start returns when another
// workflow already owns the ID: someone else is archiving that asset, and
// that is fine.
func alreadyRunning(err error) bool {
	var already *temporal.ChildWorkflowExecutionAlreadyStartedError
	return errors.As(err, &already)
}

// SpoolResult is what SpoolAssets returns.
type SpoolResult struct {
	Assets       int
	Spooled      int
	Skipped      int
	Failed       int
	Failures     []string
	Copied       int64
	Duration     time.Duration
	MiBPerSecond float64
}

// SpoolAssets brings assets back onto local storage from the NAS, on the
// NAS queue (whose worker bounds how many run together), and optionally
// pins them first so a flush cannot undo the work: `mm spool`. Large
// files get an activity each, small ones a batch.
func SpoolAssets(ctx workflow.Context, refs []ingest.AssetRef, pin bool, q Queues) (*SpoolResult, error) {
	var acts *Activities
	res := &SpoolResult{Assets: len(refs)}
	if pin {
		ids := make([]string, 0, len(refs))
		for _, r := range refs {
			ids = append(ids, r.ID)
		}
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, quickOpts), acts.Pin, ids, true).Get(ctx, nil); err != nil {
			return nil, err
		}
	}
	opts := copyOpts
	opts.TaskQueue = q.NAS
	big, small := split(refs)
	var total int64
	for _, r := range refs {
		total += r.Size
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("spooling %d assets, %s", len(refs), fmtBytes(total)))
	fail := func(msg string) {
		res.Failed++
		if len(res.Failures) < maxListed {
			res.Failures = append(res.Failures, msg)
		}
	}
	futures := make([]workflow.Future, 0, len(big))
	for _, ref := range big {
		o := opts
		o.Summary = ref.Path
		futures = append(futures, workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, o), acts.Spool, ref))
	}
	batches := chunks(small)
	bfutures := make([]workflow.Future, 0, len(batches))
	for _, batch := range batches {
		o := opts
		o.Summary = batchLabel(batch)
		bfutures = append(bfutures, workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, o), acts.SpoolBatch, batch))
	}
	for i, f := range futures {
		var r ingest.CopyResult
		if err := f.Get(ctx, &r); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			fail(big[i].Path + ": " + err.Error())
			continue
		}
		if r.Skipped {
			res.Skipped++
			continue
		}
		res.Spooled++
		res.Copied += r.Copied
		res.Duration += r.Duration
	}
	for i, f := range bfutures {
		var items []ingest.BatchItem
		if err := f.Get(ctx, &items); err != nil {
			if c := stopIfCancelled(ctx); c != nil {
				return nil, c
			}
			fail(fmt.Sprintf("batch %d: %v", i+1, err))
			continue
		}
		for _, it := range items {
			switch {
			case it.Err != "" || it.SpoolFull:
				msg := it.Err
				if it.SpoolFull {
					msg = "no spool has room"
				}
				fail(it.Path + ": " + msg)
			case it.Skipped:
				res.Skipped++
			default:
				res.Spooled++
				res.Copied += it.Copied
				res.Duration += it.Duration
			}
		}
	}
	if res.Duration > 0 {
		res.MiBPerSecond = float64(res.Copied) / (1 << 20) / res.Duration.Seconds()
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("%d spooled, %d already local, %d failed; %s at %.0f MiB/s per copy",
		res.Spooled, res.Skipped, res.Failed, fmtBytes(res.Copied), res.MiBPerSecond))
	return res, nil
}
