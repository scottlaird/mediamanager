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
func ImportWorkflowID(source string) string   { return "import:" + source }
func ArchiveWorkflowID(assetID string) string { return "asset:" + assetID }
func FlushWorkflowID(spool string) string     { return "flush:" + spool }
func BacklogWorkflowID() string               { return "archive-backlog" }

// ImportResult is what ImportSource returns; it is the Temporal form of
// ingest.Summary.
type ImportResult struct {
	Source       string
	Shape        string
	Assets       int
	New          int
	Spooled      int
	Archived     int
	SpoolFull    []string
	Failures     []string
	Unrouted     []string
	Orphans      []string
	Unrecognised int
	SafeToFormat bool
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

// ImportSource is one card or camera, start to finish: audit, register,
// link, spool each asset in turn (one reader per source), and hand each
// to its own ArchiveAsset workflow. Those children outlive this workflow
// if it is cancelled, so pulling a card never stops a NAS copy already
// under way. Anything already needing archive from earlier runs is handed
// off too. The result mirrors ingest.Summary.
func ImportSource(ctx workflow.Context, root string, q Queues) (*ImportResult, error) {
	var acts *Activities
	quick := workflow.WithActivityOptions(ctx, quickOpts)
	spool := workflow.WithActivityOptions(ctx, copyOpts)

	if err := workflow.ExecuteActivity(quick, acts.Relink).Get(ctx, nil); err != nil {
		return nil, err
	}
	var src ingest.Source
	if err := workflow.ExecuteActivity(quick, acts.ScanSource, root).Get(ctx, &src); err != nil {
		return nil, err
	}
	res := &ImportResult{
		Source: src.Loc.Name, Shape: src.Shape.String(), Assets: len(src.Assets), New: len(src.New),
		Unrouted: src.Unrouted, Orphans: src.Orphans, Unrecognised: len(src.Unrecognised),
	}
	if err := workflow.ExecuteActivity(quick, acts.Relink).Get(ctx, nil); err != nil {
		return nil, err
	}

	children := map[string]workflow.ChildWorkflowFuture{}
	handoff := func(ref ingest.AssetRef) {
		if _, done := children[ref.ID]; done {
			return
		}
		children[ref.ID] = startArchive(ctx, ref, q)
	}
	refs := src.Refs
	for i, ref := range refs {
		if _, seen := children[ref.ID]; seen {
			continue // same content under two names on one card
		}
		workflow.SetCurrentDetails(ctx, fmt.Sprintf("spooling %d/%d: %s (%s)", i+1, len(refs), ref.Path, fmtBytes(ref.Size)))
		var archived bool
		if err := workflow.ExecuteActivity(quick, acts.IsArchived, ref.ID).Get(ctx, &archived); err != nil {
			res.Failures = append(res.Failures, ref.Path+": "+err.Error())
			continue
		}
		if !archived {
			var r ingest.CopyResult
			err := workflow.ExecuteActivity(withSummary(spool, ref.Path), acts.Spool, ref).Get(ctx, &r)
			switch {
			case isType(err, ErrTypeSpoolFull):
				res.SpoolFull = append(res.SpoolFull, ref.Path)
			case err != nil:
				res.Failures = append(res.Failures, ref.Path+": spool: "+err.Error())
				continue
			case !r.Skipped:
				res.Spooled++
			}
		}
		handoff(ref) // a no-op copy for archived assets, but it refreshes sidecars
	}
	var leftovers []ingest.AssetRef
	if err := workflow.ExecuteActivity(quick, acts.NeedsArchive).Get(ctx, &leftovers); err == nil {
		for _, ref := range leftovers {
			handoff(ref)
		}
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("waiting for %d archive workflows", len(children)))
	for id, f := range children {
		var n int
		if err := f.Get(ctx, &n); err != nil {
			if !alreadyRunning(err) {
				res.Failures = append(res.Failures, id+": archive: "+err.Error())
			}
			continue
		}
		res.Archived += n
	}

	res.SafeToFormat = len(src.Assets) > 0
	for _, id := range src.Assets {
		var archived bool
		if err := workflow.ExecuteActivity(quick, acts.IsArchived, id).Get(ctx, &archived); err != nil || !archived {
			res.SafeToFormat = false
			break
		}
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("%d assets, %d new, %d spooled, %d archived, %d failed; safe to format: %v",
		res.Assets, res.New, res.Spooled, res.Archived, len(res.Failures), res.SafeToFormat))
	return res, nil
}

// startArchive starts (or joins) the ArchiveAsset workflow for an asset.
// The path is the workflow's summary in the UI, so a list of them reads
// as filenames rather than identities.
func startArchive(ctx workflow.Context, ref ingest.AssetRef, q Queues) workflow.ChildWorkflowFuture {
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            ArchiveWorkflowID(ref.ID),
		TaskQueue:             q.Main,
		ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_ABANDON,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		StaticSummary:         ref.Path,
		StaticDetails:         fmt.Sprintf("%s, asset %s", fmtBytes(ref.Size), ref.ID),
	})
	return workflow.ExecuteChildWorkflow(cctx, ArchiveAsset, ref, q)
}

// ArchiveAsset copies one asset to every NAS lacking it, on the NAS queue,
// and returns how many copies it made. It is its own workflow so that it
// survives the import that started it and so two imports of the same
// content share one copy.
func ArchiveAsset(ctx workflow.Context, ref ingest.AssetRef, q Queues) (int, error) {
	var acts *Activities
	opts := copyOpts
	opts.TaskQueue = q.NAS
	opts.Summary = ref.Path
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("copying %s (%s) to the NAS", ref.Path, fmtBytes(ref.Size)))
	var results []ingest.CopyResult
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), acts.Archive, ref).Get(ctx, &results); err != nil {
		workflow.SetCurrentDetails(ctx, "failed: "+err.Error())
		return 0, err
	}
	n := 0
	var where []string
	for _, r := range results {
		if !r.Skipped {
			n++
			where = append(where, r.Location)
		}
	}
	switch n {
	case 0:
		workflow.SetCurrentDetails(ctx, "already on the NAS; sidecars refreshed")
	default:
		workflow.SetCurrentDetails(ctx, fmt.Sprintf("copied to %s", strings.Join(where, ", ")))
	}
	return n, nil
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

// BacklogResult is what ArchiveBacklog returns.
type BacklogResult struct {
	Assets   int
	Archived int
	Failures []string
}

// ArchiveBacklog hands every asset lacking a NAS copy to ArchiveAsset and
// waits for them: `mm archive`.
func ArchiveBacklog(ctx workflow.Context, q Queues) (*BacklogResult, error) {
	var acts *Activities
	quick := workflow.WithActivityOptions(ctx, quickOpts)
	var refs []ingest.AssetRef
	if err := workflow.ExecuteActivity(quick, acts.NeedsArchive).Get(ctx, &refs); err != nil {
		return nil, err
	}
	res := &BacklogResult{Assets: len(refs)}
	var total int64
	futures := map[string]workflow.ChildWorkflowFuture{}
	for _, ref := range refs {
		futures[ref.ID] = startArchive(ctx, ref, q)
		total += ref.Size
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("waiting for %d archive workflows, %s", len(futures), fmtBytes(total)))
	for id, f := range futures {
		var n int
		if err := f.Get(ctx, &n); err != nil {
			if !alreadyRunning(err) {
				res.Failures = append(res.Failures, id+": "+err.Error())
			}
			continue
		}
		res.Archived += n
	}
	workflow.SetCurrentDetails(ctx, fmt.Sprintf("%d of %d archived, %d failed", res.Archived, res.Assets, len(res.Failures)))
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
