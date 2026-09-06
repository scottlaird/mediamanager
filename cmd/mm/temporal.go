package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/scottlaird/mediamanager/ingest"
	mmtemporal "github.com/scottlaird/mediamanager/temporal"
	"github.com/spf13/cobra"
	"go.temporal.io/sdk/client"
)

// local forces the in-process driver even when temporal is configured.
var local bool

func workerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Serve Temporal workflows and activities for this machine's locations",
		Long: `Worker runs until interrupted, executing imports, archives and flushes
started by other mm invocations. It needs a temporal: section in the config
and a reachable Temporal service; for one machine, that is
"temporal server start-dev --db-filename ~/.local/share/mediamanager/temporal.db".
Concurrent NAS copies are bounded by concurrency.nas.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			env, done, err := openEnv()
			if err != nil {
				return err
			}
			defer done()
			if env.Config.Temporal == nil {
				return errors.New("no temporal: section in the config")
			}
			c, err := mmtemporal.Dial(env.Config.Temporal)
			if err != nil {
				return err
			}
			defer c.Close()
			q := mmtemporal.QueuesFor(env.Config.Temporal.TaskQueue)
			fmt.Fprintf(os.Stderr, "mm worker: serving %s and %s at %s (%d NAS slots)\n", q.Main, q.NAS, env.Config.Temporal.Address, env.Config.Concurrency.NAS)
			return mmtemporal.RunWorker(cmd.Context(), c, env, q, env.Config.Concurrency.NAS)
		},
	}
}

// temporalClient returns a client and queues when the config asks for
// Temporal and --local was not given; ok=false means run in-process.
func temporalClient(env *ingest.Env) (client.Client, mmtemporal.Queues, bool, error) {
	if local || env.Config.Temporal == nil {
		return nil, mmtemporal.Queues{}, false, nil
	}
	c, err := mmtemporal.Dial(env.Config.Temporal)
	if err != nil {
		return nil, mmtemporal.Queues{}, false, err
	}
	return c, mmtemporal.QueuesFor(env.Config.Temporal.TaskQueue), true, nil
}

// importViaTemporal starts one ImportSource per source and, unless
// detached, waits for each and prints its result.
func importViaTemporal(ctx context.Context, c client.Client, q mmtemporal.Queues, sources []string, detach bool) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		fail bool
	)
	for _, src := range sources {
		run, err := mmtemporal.StartImport(ctx, c, q, src)
		if err != nil {
			return err
		}
		fmt.Printf("%s: workflow %s run %s\n", src, run.GetID(), run.GetRunID())
		if detach {
			continue
		}
		wg.Add(1)
		go func(src string, run client.WorkflowRun) {
			defer wg.Done()
			var res mmtemporal.ImportResult
			err := run.Get(ctx, &res)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", src, err)
				fail = true
				return
			}
			printImportResult(os.Stdout, src, &res)
			if len(res.Failures) > 0 {
				fail = true
			}
		}(src, run)
	}
	wg.Wait()
	if fail {
		return errors.New("import finished with errors")
	}
	return nil
}

func printImportResult(w *os.File, src string, r *mmtemporal.ImportResult) {
	fmt.Fprintf(w, "%s (%s): %d assets, %d new, %d spooled, %d archived\n", src, r.Shape, r.Assets, r.New, r.Spooled, r.Archived)
	for _, rel := range r.Unrouted {
		fmt.Fprintf(w, "  skipped (no tree for its kind): %s\n", rel)
	}
	for _, rel := range r.Orphans {
		fmt.Fprintf(w, "  proxy or sidecar without an original: %s\n", rel)
	}
	if r.Unrecognised > 0 {
		fmt.Fprintf(w, "  %d unrecognised files ignored\n", r.Unrecognised)
	}
	for _, id := range r.SpoolFull {
		fmt.Fprintf(w, "  spool full, archived from source: %s\n", id)
	}
	for _, f := range r.Failures {
		fmt.Fprintf(w, "  FAILED %s\n", f)
	}
	if r.SafeToFormat {
		fmt.Fprintf(w, "  every file is on the NAS; safe to format in camera\n")
	} else {
		fmt.Fprintf(w, "  NOT safe to format: some files are not on the NAS yet\n")
	}
}
