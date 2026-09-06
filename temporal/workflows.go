package temporal

import (
	"errors"
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
	handoff := func(id string) {
		if _, done := children[id]; done {
			return
		}
		children[id] = startArchive(ctx, id, q)
	}
	for _, id := range src.Assets {
		if _, seen := children[id]; seen {
			continue // same content under two names on one card
		}
		var archived bool
		if err := workflow.ExecuteActivity(quick, acts.IsArchived, id).Get(ctx, &archived); err != nil {
			res.Failures = append(res.Failures, id+": "+err.Error())
			continue
		}
		if !archived {
			var r ingest.CopyResult
			err := workflow.ExecuteActivity(spool, acts.Spool, id).Get(ctx, &r)
			switch {
			case isType(err, ErrTypeSpoolFull):
				res.SpoolFull = append(res.SpoolFull, id)
			case err != nil:
				res.Failures = append(res.Failures, id+": spool: "+err.Error())
				continue
			case !r.Skipped:
				res.Spooled++
			}
		}
		handoff(id) // a no-op copy for archived assets, but it refreshes sidecars
	}
	var leftovers []string
	if err := workflow.ExecuteActivity(quick, acts.NeedsArchive).Get(ctx, &leftovers); err == nil {
		for _, id := range leftovers {
			handoff(id)
		}
	}
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
	return res, nil
}

// startArchive starts (or joins) the ArchiveAsset workflow for an asset.
func startArchive(ctx workflow.Context, id string, q Queues) workflow.ChildWorkflowFuture {
	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:            ArchiveWorkflowID(id),
		TaskQueue:             q.Main,
		ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_ABANDON,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	})
	return workflow.ExecuteChildWorkflow(cctx, ArchiveAsset, id, q)
}

// ArchiveAsset copies one asset to every NAS lacking it, on the NAS queue,
// and returns how many copies it made. It is its own workflow so that it
// survives the import that started it and so two imports of the same
// content share one copy.
func ArchiveAsset(ctx workflow.Context, id string, q Queues) (int, error) {
	var acts *Activities
	opts := copyOpts
	opts.TaskQueue = q.NAS
	actx := workflow.WithActivityOptions(ctx, opts)
	var results []ingest.CopyResult
	if err := workflow.ExecuteActivity(actx, acts.Archive, id).Get(ctx, &results); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range results {
		if !r.Skipped {
			n++
		}
	}
	return n, nil
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
	var ids []string
	if err := workflow.ExecuteActivity(quick, acts.NeedsArchive).Get(ctx, &ids); err != nil {
		return nil, err
	}
	res := &BacklogResult{Assets: len(ids)}
	futures := map[string]workflow.ChildWorkflowFuture{}
	for _, id := range ids {
		futures[id] = startArchive(ctx, id, q)
	}
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
