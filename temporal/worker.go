package temporal

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/ingest"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
)

// Dial connects to the Temporal service named in cfg. The error names the
// address, since "connection refused" almost always means the dev server
// is not running.
func Dial(cfg *config.Temporal) (client.Client, error) {
	c, err := client.Dial(client.Options{
		HostPort:  cfg.Address,
		Namespace: cfg.Namespace,
		Logger:    log.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	})
	if err != nil {
		return nil, fmt.Errorf("temporal at %s: %w (is `temporal server start-dev` running?)", cfg.Address, err)
	}
	return c, nil
}

// RunWorker serves both task queues from one process until ctx is done:
// the main queue for workflows and spool/scan activities, and the NAS
// queue with nasSlots concurrent activities, which is what bounds
// simultaneous NAS writes.
func RunWorker(ctx context.Context, c client.Client, env *ingest.Env, q Queues, nasSlots int) error {
	acts := &Activities{Env: env}
	main := worker.New(c, q.Main, worker.Options{})
	main.RegisterWorkflow(ImportSource)
	main.RegisterWorkflow(ArchiveAsset)
	main.RegisterWorkflow(ArchiveBacklog)
	main.RegisterWorkflow(FlushSpool)
	main.RegisterActivity(acts)

	if nasSlots <= 0 {
		nasSlots = 1
	}
	nas := worker.New(c, q.NAS, worker.Options{MaxConcurrentActivityExecutionSize: nasSlots})
	nas.RegisterActivity(acts)

	if err := main.Start(); err != nil {
		return err
	}
	if err := nas.Start(); err != nil {
		main.Stop()
		return err
	}
	<-ctx.Done()
	nas.Stop()
	main.Stop()
	return nil
}

// StartImport starts, or joins, the import workflow for a source root.
func StartImport(ctx context.Context, c client.Client, q Queues, root string) (client.WorkflowRun, error) {
	return c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       ImportWorkflowID(idSafe(root)),
		TaskQueue:                q.Main,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		StaticSummary:            "import " + root,
	}, ImportSource, root, q)
}

// StartArchiveBacklog starts, or joins, the backlog workflow.
func StartArchiveBacklog(ctx context.Context, c client.Client, q Queues) (client.WorkflowRun, error) {
	return c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       BacklogWorkflowID(),
		TaskQueue:                q.Main,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		StaticSummary:            "archive everything not yet on the NAS",
	}, ArchiveBacklog, q)
}

// StartFlush starts, or joins, the flush workflow for a spool.
func StartFlush(ctx context.Context, c client.Client, q Queues, spool string, opts ingest.FlushOptions) (client.WorkflowRun, error) {
	return c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                       FlushWorkflowID(spool),
		TaskQueue:                q.Main,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		StaticSummary:            "flush " + spool,
	}, FlushSpool, spool, opts)
}

// idSafe makes a path usable inside a workflow ID.
func idSafe(p string) string {
	return strings.Trim(strings.ReplaceAll(p, "/", ":"), ":")
}
