package temporal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottlaird/mediamanager/catalog"
	"github.com/scottlaird/mediamanager/config"
	"github.com/scottlaird/mediamanager/ingest"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

const mib = 1 << 20

var shot = time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)

// fixture is a spool, a NAS and a link tree under a temp dir with an Env
// over them; withSpool=false configures a spool directory that does not
// exist, which is how "no spool has room" is exercised.
type fixture struct {
	base, spool, nas, links string
	env                     *ingest.Env
}

func newFixture(t *testing.T, withSpool bool) *fixture {
	t.Helper()
	f := &fixture{base: t.TempDir()}
	f.spool, f.nas, f.links = filepath.Join(f.base, "spool"), filepath.Join(f.base, "nas"), filepath.Join(f.base, "links")
	if withSpool {
		os.MkdirAll(f.spool, 0o755)
	}
	os.MkdirAll(f.nas, 0o755)
	yaml := fmt.Sprintf(`
catalog: %s/catalog.db
timezone: UTC
trees:
  video: {link: %s/video}
locations:
  - {name: nas, kind: nas, path: %s}
  - {name: fast, kind: spool, path: %s}
temporal: {}
`, f.base, f.links, f.nas, f.spool)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(cfg.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	f.env = &ingest.Env{Config: cfg, Catalog: cat}
	return f
}

func (f *fixture) card(t *testing.T, files map[string]int) string {
	t.Helper()
	root := filepath.Join(f.base, "card")
	for rel, n := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := make([]byte, n)
		var seed int64
		for _, c := range rel {
			seed = seed*31 + int64(c)
		}
		rand.New(rand.NewSource(seed)).Read(b)
		os.WriteFile(p, b, 0o644)
		os.Chtimes(p, shot, shot)
	}
	return root
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// quietSuite is a test suite whose SDK logger goes nowhere; the default
// prints every activity result at debug level.
func quietSuite() *testsuite.WorkflowTestSuite {
	var ts testsuite.WorkflowTestSuite
	ts.SetLogger(log.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	return &ts
}

// newEnv wires real activities over the fixture into a workflow test
// environment, so workflows are exercised end to end against temp dirs
// rather than against mocks.
func newEnv(t *testing.T, f *fixture) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := quietSuite().NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ImportSource)
	env.RegisterWorkflow(ArchiveAsset)
	env.RegisterWorkflow(ArchiveBacklog)
	env.RegisterWorkflow(FlushSpool)
	env.RegisterWorkflow(SpoolAssets)
	env.RegisterActivity(&Activities{Env: f.env})
	return env
}

func runImport(t *testing.T, f *fixture, card string) *ImportResult {
	t.Helper()
	env := newEnv(t, f)
	env.ExecuteWorkflow(ImportSource, card, QueuesFor("t"))
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res ImportResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	return &res
}

func TestImportSourceWorkflow(t *testing.T) {
	f := newFixture(t, true)
	card := f.card(t, map[string]int{"A001_C001.braw": 2 * mib, "A001_C001.sidecar": 200, "B001_C001.braw": mib})

	res := runImport(t, f, card)
	if res.Assets != 2 || res.New != 2 || res.Spooled != 2 || res.Archived != 2 || !res.SafeToFormat || len(res.Failures) != 0 || len(res.SpoolFull) != 0 {
		t.Fatalf("result %+v", res)
	}
	matches, _ := filepath.Glob(filepath.Join(f.nas, "video", "2026", "09", "05", "*"))
	if len(matches) != 3 { // two clips and a sidecar
		t.Errorf("NAS holds %v", matches)
	}
	links, _ := filepath.Glob(filepath.Join(f.links, "video", "2026", "09", "05", "*"))
	if len(links) != 3 {
		t.Errorf("links %v", links)
	}
	st, err := f.env.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range st.Assets {
		if a.State != "archived" {
			t.Errorf("%s: %s", a.Asset.RelPath, a.State)
		}
	}

	// Again: nothing to do, still safe.
	res = runImport(t, f, card)
	if res.New != 0 || res.Spooled != 0 || res.Archived != 0 || !res.SafeToFormat {
		t.Errorf("second run %+v", res)
	}
}

func TestImportSourceSpoolFull(t *testing.T) {
	f := newFixture(t, false) // spool directory absent: nothing has room
	card := f.card(t, map[string]int{"A001_C001.braw": mib})
	res := runImport(t, f, card)
	if len(res.SpoolFull) != 1 || res.Spooled != 0 || res.Archived != 1 || !res.SafeToFormat || len(res.Failures) != 0 {
		t.Fatalf("result %+v", res)
	}
	if exists(f.spool) {
		t.Error("spool directory appeared")
	}
}

func TestBacklogAndFlushWorkflows(t *testing.T) {
	f := newFixture(t, true)
	os.RemoveAll(f.nas) // NAS away: import spools only
	card := f.card(t, map[string]int{"A001_C001.braw": mib, "B001_C001.braw": mib})
	res := runImport(t, f, card)
	if res.Spooled != 2 || res.Archived != 0 || res.SafeToFormat {
		t.Fatalf("import without NAS %+v", res)
	}

	os.MkdirAll(f.nas, 0o755)
	env := newEnv(t, f)
	env.ExecuteWorkflow(ArchiveBacklog, QueuesFor("t"))
	var br BacklogResult
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.GetWorkflowResult(&br)
	if br.Assets != 2 || br.Archived != 2 || len(br.Failures) != 0 {
		t.Fatalf("backlog %+v", br)
	}

	env = newEnv(t, f)
	env.ExecuteWorkflow(FlushSpool, "fast", ingest.FlushOptions{})
	var fr ingest.FlushReport
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.GetWorkflowResult(&fr)
	if len(fr.Flushed) != 2 || len(fr.Refused) != 0 {
		t.Fatalf("flush %+v", fr)
	}
	if left, _ := filepath.Glob(filepath.Join(f.spool, "video", "*", "*", "*", "*")); len(left) != 0 {
		t.Errorf("spool still holds %v", left)
	}
}

func TestSpoolActivityHeartbeats(t *testing.T) {
	f := newFixture(t, true)
	card := f.card(t, map[string]int{"A001_C001.braw": 3 * mib})
	src, err := f.env.ScanSource(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	env := quietSuite().NewTestActivityEnvironment()
	acts := &Activities{Env: f.env}
	env.RegisterActivity(acts)
	var beats int
	var last Heartbeat
	env.SetOnActivityHeartbeatListener(func(info *activity.Info, details converter.EncodedValues) {
		beats++
		details.Get(&last)
	})
	val, err := env.ExecuteActivity(acts.Spool, src.Refs[0])
	if err != nil {
		t.Fatal(err)
	}
	var r ingest.CopyResult
	val.Get(&r)
	if r.Location != "fast" || r.Bytes != 3*mib {
		t.Errorf("result %+v", r)
	}
	if beats == 0 || last.Done != last.Total || last.Total != 3*mib || last.Percent != 100 || last.Path != src.Refs[0].Path {
		t.Errorf("heartbeats %d, last %+v", beats, last)
	}
}

func TestArchiveAssetResult(t *testing.T) {
	f := newFixture(t, true)
	card := f.card(t, map[string]int{"A001_C001.braw": 2 * mib})
	src, err := f.env.ScanSource(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	env := newEnv(t, f)
	env.ExecuteWorkflow(ArchiveAsset, src.Refs[0], QueuesFor("t"))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var ar ArchiveResult
	env.GetWorkflowResult(&ar)
	if ar.Copies != 1 || ar.Path != src.Refs[0].Path || ar.Bytes != 2*mib || ar.Copied != 2*mib || ar.Duration <= 0 || ar.MiBPerSecond <= 0 || len(ar.Locations) != 1 {
		t.Errorf("result %+v", ar)
	}
	// Second time: nothing copied, rate stays zero rather than dividing by nothing.
	env = newEnv(t, f)
	env.ExecuteWorkflow(ArchiveAsset, src.Refs[0], QueuesFor("t"))
	env.GetWorkflowResult(&ar)
	if ar.Copies != 0 || len(ar.Skipped) != 1 || ar.Copied != 0 || ar.MiBPerSecond != 0 {
		t.Errorf("second result %+v", ar)
	}
}

func TestSpoolAssetsWorkflow(t *testing.T) {
	f := newFixture(t, true)
	card := f.card(t, map[string]int{"A001_C001.braw": 2 * mib, "B001_C001.braw": mib})
	runImport(t, f, card)
	env := newEnv(t, f)
	env.ExecuteWorkflow(FlushSpool, "fast", ingest.FlushOptions{})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	refs, err := f.env.Select(context.Background(), []string{"2026/09/05"})
	if err != nil || len(refs) != 2 {
		t.Fatalf("select: %v, %v", refs, err)
	}
	env = newEnv(t, f)
	env.ExecuteWorkflow(SpoolAssets, refs, true, QueuesFor("t"))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var res SpoolResult
	env.GetWorkflowResult(&res)
	if res.Assets != 2 || res.Spooled != 2 || res.Skipped != 0 || len(res.Failures) != 0 || res.Copied != 3*mib || res.MiBPerSecond <= 0 {
		t.Fatalf("result %+v", res)
	}
	st, _ := f.env.Status(context.Background())
	for _, a := range st.Assets {
		if a.State != "archived" || !a.Asset.Pinned {
			t.Errorf("%s: %s pinned=%v", a.Asset.RelPath, a.State, a.Asset.Pinned)
		}
	}
	// Pinned: a flush leaves them alone; a second spool is all skips.
	env = newEnv(t, f)
	env.ExecuteWorkflow(FlushSpool, "fast", ingest.FlushOptions{})
	var fr ingest.FlushReport
	env.GetWorkflowResult(&fr)
	if len(fr.Flushed) != 0 {
		t.Errorf("flushed pinned assets: %+v", fr)
	}
	env = newEnv(t, f)
	env.ExecuteWorkflow(SpoolAssets, refs, false, QueuesFor("t"))
	env.GetWorkflowResult(&res)
	if res.Skipped != 2 || res.Spooled != 0 {
		t.Errorf("second spool %+v", res)
	}
}

func TestArchiveWorkflowID(t *testing.T) {
	ref := ingest.AssetRef{ID: "6b09f8b22a45eb03", Path: "video/2026/07/10/a021_07100435_c001-6b09f8b22a45eb03.braw"}
	if got := ArchiveWorkflowID(ref); got != "archive:video/2026/07/10/a021_07100435_c001-6b09f8b22a45eb03.braw" {
		t.Errorf("id = %q", got)
	}
}

func TestClassify(t *testing.T) {
	err := classify(fmt.Errorf("wrapped: %w", ingest.ErrSpoolFull))
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != ErrTypeSpoolFull || !app.NonRetryable() {
		t.Errorf("spool full -> %v", err)
	}
	if classify(nil) != nil {
		t.Error("nil classified")
	}
	plain := errors.New("disk on fire")
	if classify(plain) != plain {
		t.Error("ordinary error rewritten")
	}
	if !isType(err, ErrTypeSpoolFull) || isType(plain, ErrTypeSpoolFull) {
		t.Error("isType")
	}
}
