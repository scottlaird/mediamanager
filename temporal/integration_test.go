package temporal

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/ingest"
	"go.temporal.io/sdk/client"
)

// TestAgainstDevServer runs the real client, worker and both task queues
// against `temporal server start-dev`. It is skipped when the temporal CLI
// is not installed, which is the case on CI today.
func TestAgainstDevServer(t *testing.T) {
	bin, err := exec.LookPath("temporal")
	if err != nil {
		t.Skip("temporal CLI not installed")
	}
	port := freePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "server", "start-dev", "--headless", "--port", fmt.Sprint(port),
		"--db-filename", filepath.Join(t.TempDir(), "temporal.db"), "--log-level", "error")
	srv.Stdout, srv.Stderr = os.Stderr, os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); srv.Wait() }()

	f := newFixture(t, true)
	f.env.Config.Temporal = &config.Temporal{Address: fmt.Sprintf("127.0.0.1:%d", port), Namespace: "default", TaskQueue: "mm-test"}
	cl, err := dialUntilUp(ctx, f.env.Config.Temporal)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	q := QueuesFor("mm-test")
	wctx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunWorker(wctx, cl, f.env, q, 2) }()

	card := f.card(t, map[string]int{"A001_C001.braw": 2 * mib, "A001_C001.sidecar": 100, "B001_C001.braw": mib})
	run, err := StartImport(ctx, cl, q, card)
	if err != nil {
		t.Fatal(err)
	}
	// Starting the same import again joins the running one.
	again, err := StartImport(ctx, cl, q, card)
	if err != nil {
		t.Fatal(err)
	}
	if again.GetRunID() != run.GetRunID() {
		t.Errorf("second start got a new run: %s vs %s", again.GetRunID(), run.GetRunID())
	}
	var res ImportResult
	if err := run.Get(ctx, &res); err != nil {
		t.Fatal(err)
	}
	if res.Assets != 2 || res.Spooled != 2 || res.Archived != 2 || !res.SafeToFormat || len(res.Failures) != 0 {
		t.Fatalf("import result %+v", res)
	}
	if !exists(filepath.Join(f.nas, "video", "2026", "09", "05")) {
		t.Error("nothing archived")
	}

	fr, err := StartFlush(ctx, cl, q, "fast", ingest.FlushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var rep ingest.FlushReport
	if err := fr.Get(ctx, &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Flushed) != 2 {
		t.Errorf("flush %+v", rep)
	}

	br, err := StartArchiveBacklog(ctx, cl, q)
	if err != nil {
		t.Fatal(err)
	}
	var bres BacklogResult
	if err := br.Get(ctx, &bres); err != nil {
		t.Fatal(err)
	}
	if bres.Assets != 0 {
		t.Errorf("backlog after flush %+v", bres)
	}

	stopWorker()
	if err := <-workerDone; err != nil {
		t.Errorf("worker: %v", err)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func dialUntilUp(ctx context.Context, cfg *config.Temporal) (client.Client, error) {
	deadline := time.Now().Add(45 * time.Second)
	for {
		c, err := Dial(cfg)
		if err == nil {
			// Dial is lazy; make a call to know the server is really serving.
			if _, herr := c.CheckHealth(ctx, nil); herr == nil {
				return c, nil
			}
			c.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dev server never came up: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
