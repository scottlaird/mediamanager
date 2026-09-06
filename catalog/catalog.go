// Package catalog is mediamanager's index of assets and where their copies
// live.
//
// It is an index, not the source of truth. Identity is in filenames and
// completeness is in the absence of a .partial suffix (rules R2 and R7), so
// everything here can be rebuilt by walking the spool and NAS roots. What
// the catalog adds is speed: answering "which copies of this asset exist"
// and "which assets still need archiving" without touching a disk.
package catalog

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/scottlaird/mediamanager/media"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// schemaVersion is stored in PRAGMA user_version. Bump it with any migration.
const schemaVersion = 1

var (
	ErrNotFound = errors.New("catalog: not found")
	// ErrPathTaken means a different asset already owns the relpath in that
	// kind's tree. The caller picks another name (naming.WithSuffix).
	ErrPathTaken = errors.New("catalog: path already belongs to another asset")
)

// DB is an open catalog. It is safe for concurrent use from one process;
// the file itself is not meant to be shared between machines.
type DB struct {
	db *sql.DB
}

// Open opens or creates the catalog at path. ":memory:" gives a throwaway
// catalog for tests.
func Open(path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: SQLite serialises writers anyway, and a single
	// connection makes ":memory:" behave and keeps WAL checkpoints simple.
	db.SetMaxOpenConns(1)
	c := &DB{db: db}
	if err := c.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *DB) Close() error { return c.db.Close() }

func (c *DB) migrate(ctx context.Context) error {
	var v int
	if err := c.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > schemaVersion {
		return fmt.Errorf("catalog: schema version %d is newer than this build (%d)", v, schemaVersion)
	}
	if _, err := c.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("catalog: creating schema: %w", err)
	}
	if v < schemaVersion {
		if _, err := c.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return err
		}
	}
	return nil
}

// LocationKind is a storage tier.
type LocationKind string

const (
	Source LocationKind = "source"
	Spool  LocationKind = "spool"
	NAS    LocationKind = "nas"
)

// Location is a place bytes can live. Name is the stable handle; Root is
// the mount point last seen, which on macOS can change between runs.
type Location struct {
	ID         int64
	Kind       LocationKind
	Name       string
	VolumeUUID string
	Label      string
	// Priority orders spools for link resolution; lower wins.
	Priority int
	Root     string
}

// UpsertLocation inserts or updates a location by Name and returns it with
// its ID filled in.
func (c *DB) UpsertLocation(ctx context.Context, l Location) (Location, error) {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO locations (kind, name, volume_uuid, label, priority, root)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			kind = excluded.kind, volume_uuid = excluded.volume_uuid,
			label = excluded.label, priority = excluded.priority, root = excluded.root`,
		l.Kind, l.Name, l.VolumeUUID, l.Label, l.Priority, l.Root)
	if err != nil {
		return Location{}, err
	}
	return c.LocationByName(ctx, l.Name)
}

func (c *DB) LocationByName(ctx context.Context, name string) (Location, error) {
	return scanLocation(c.db.QueryRowContext(ctx,
		`SELECT id, kind, name, volume_uuid, label, priority, root FROM locations WHERE name = ?`, name))
}

func (c *DB) LocationByID(ctx context.Context, id int64) (Location, error) {
	return scanLocation(c.db.QueryRowContext(ctx,
		`SELECT id, kind, name, volume_uuid, label, priority, root FROM locations WHERE id = ?`, id))
}

// Locations lists locations of one kind, best priority first.
func (c *DB) Locations(ctx context.Context, kind LocationKind) ([]Location, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, kind, name, volume_uuid, label, priority, root FROM locations
		 WHERE kind = ? ORDER BY priority, id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		l, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanLocation(s scanner) (Location, error) {
	var l Location
	err := s.Scan(&l.ID, &l.Kind, &l.Name, &l.VolumeUUID, &l.Label, &l.Priority, &l.Root)
	if errors.Is(err, sql.ErrNoRows) {
		return Location{}, ErrNotFound
	}
	return l, err
}

// Asset is one original. ID is the sparse identity for video and audio and
// the full SHA-256 for stills; Scheme says which.
type Asset struct {
	ID          string
	Scheme      string
	Kind        media.Kind
	Size        int64
	FullSHA256  string
	OrigName    string
	CaptureTime time.Time
	// RelPath is the permanent path relative to the root of Kind's tree,
	// shared by every tier and by the link tree.
	RelPath string
	Project string
	Pinned  bool
}

// PutAsset records a new asset. It is idempotent for an identical ID and
// returns ErrPathTaken when another asset owns RelPath.
func (c *DB) PutAsset(ctx context.Context, a Asset) error {
	if a.ID == "" || a.RelPath == "" {
		return errors.New("catalog: asset needs an ID and a RelPath")
	}
	res, err := c.db.ExecContext(ctx, `
		INSERT INTO assets (id, scheme, kind, size, full_sha256, orig_name, capture_time, relpath, project, pinned, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		a.ID, a.Scheme, a.Kind.String(), a.Size, a.FullSHA256, a.OrigName,
		formatTime(a.CaptureTime), a.RelPath, a.Project, a.Pinned, formatTime(time.Now()))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %s", ErrPathTaken, a.RelPath)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Same ID already present; make sure it is the same asset.
		have, err := c.Asset(ctx, a.ID)
		if err != nil {
			return err
		}
		if have.RelPath != a.RelPath || have.Kind != a.Kind {
			return fmt.Errorf("catalog: asset %s already recorded at %s, not %s", a.ID, have.RelPath, a.RelPath)
		}
	}
	return nil
}

func (c *DB) Asset(ctx context.Context, id string) (Asset, error) {
	return scanAsset(c.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM assets WHERE id = ?`, id))
}

func (c *DB) AssetByPath(ctx context.Context, kind media.Kind, relpath string) (Asset, error) {
	return scanAsset(c.db.QueryRowContext(ctx,
		`SELECT `+assetCols+` FROM assets WHERE kind = ? AND relpath = ?`, kind.String(), relpath))
}

// SetFullSHA256 records a full hash learned later, typically from a copy.
func (c *DB) SetFullSHA256(ctx context.Context, id, sum string) error {
	return c.update(ctx, `UPDATE assets SET full_sha256 = ? WHERE id = ?`, sum, id)
}

func (c *DB) SetProject(ctx context.Context, id, project string) error {
	return c.update(ctx, `UPDATE assets SET project = ? WHERE id = ?`, project, id)
}

func (c *DB) SetPinned(ctx context.Context, id string, pinned bool) error {
	return c.update(ctx, `UPDATE assets SET pinned = ? WHERE id = ?`, pinned, id)
}

func (c *DB) update(ctx context.Context, q string, args ...any) error {
	res, err := c.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const assetCols = `id, scheme, kind, size, full_sha256, orig_name, capture_time, relpath, project, pinned`

func scanAsset(s scanner) (Asset, error) {
	var a Asset
	var kind, capture string
	err := s.Scan(&a.ID, &a.Scheme, &kind, &a.Size, &a.FullSHA256, &a.OrigName, &capture, &a.RelPath, &a.Project, &a.Pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	a.Kind = parseKind(kind)
	a.CaptureTime, err = parseTime(capture)
	return a, err
}

// CopyState is whether a copy is usable. Only complete copies count for any
// decision about deleting anything (rule R1).
type CopyState string

const (
	Partial  CopyState = "partial"
	Complete CopyState = "complete"
)

// Copy is one asset on one location.
type Copy struct {
	AssetID    string
	LocationID int64
	// RelPath is relative to the location's root. It equals the asset's
	// RelPath on spools and the NAS and is the camera's path on a source.
	RelPath    string
	State      CopyState
	VerifiedAt time.Time
	FullSHA256 string
	// Location is filled in by queries that join it.
	Location Location
}

// PutCopy inserts or replaces the record of asset on location.
func (c *DB) PutCopy(ctx context.Context, cp Copy) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO copies (asset_id, location_id, relpath, state, verified_at, full_sha256)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(asset_id, location_id) DO UPDATE SET
			relpath = excluded.relpath, state = excluded.state,
			verified_at = excluded.verified_at, full_sha256 = excluded.full_sha256`,
		cp.AssetID, cp.LocationID, cp.RelPath, cp.State, formatTime(cp.VerifiedAt), cp.FullSHA256)
	return err
}

func (c *DB) DeleteCopy(ctx context.Context, assetID string, locationID int64) error {
	return c.update(ctx, `DELETE FROM copies WHERE asset_id = ? AND location_id = ?`, assetID, locationID)
}

const copyCols = `c.asset_id, c.location_id, c.relpath, c.state, c.verified_at, c.full_sha256,
	l.id, l.kind, l.name, l.volume_uuid, l.label, l.priority, l.root`

// Copies lists every recorded copy of an asset, partial ones included,
// ordered spool, NAS, source and by priority within each. The first
// Complete entry is where the link tree should point.
func (c *DB) Copies(ctx context.Context, assetID string) ([]Copy, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT `+copyCols+` FROM copies c JOIN locations l ON l.id = c.location_id
		WHERE c.asset_id = ?
		ORDER BY CASE l.kind WHEN 'spool' THEN 0 WHEN 'nas' THEN 1 ELSE 2 END, l.priority, l.id`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Copy
	for rows.Next() {
		var cp Copy
		var verified string
		if err := rows.Scan(&cp.AssetID, &cp.LocationID, &cp.RelPath, &cp.State, &verified, &cp.FullSHA256,
			&cp.Location.ID, &cp.Location.Kind, &cp.Location.Name, &cp.Location.VolumeUUID,
			&cp.Location.Label, &cp.Location.Priority, &cp.Location.Root); err != nil {
			return nil, err
		}
		if cp.VerifiedAt, err = parseTime(verified); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// NeedsArchive lists assets with a complete copy somewhere but none on any
// NAS, oldest capture first: the archive worklist.
func (c *DB) NeedsArchive(ctx context.Context) ([]Asset, error) {
	return c.assets(ctx, `
		SELECT `+assetCols+` FROM assets a
		WHERE EXISTS (SELECT 1 FROM copies c WHERE c.asset_id = a.id AND c.state = 'complete')
		  AND NOT EXISTS (SELECT 1 FROM copies c JOIN locations l ON l.id = c.location_id
		                  WHERE c.asset_id = a.id AND c.state = 'complete' AND l.kind = 'nas')
		ORDER BY capture_time, id`)
}

// Flushable lists unpinned assets with a complete copy on the given spool
// and a complete copy on some NAS, oldest capture first: the flush worklist.
// Callers must still verify both copies before deleting (rule R4).
func (c *DB) Flushable(ctx context.Context, spoolID int64) ([]Asset, error) {
	return c.assets(ctx, `
		SELECT `+assetCols+` FROM assets a
		WHERE a.pinned = 0
		  AND EXISTS (SELECT 1 FROM copies c WHERE c.asset_id = a.id AND c.location_id = ? AND c.state = 'complete')
		  AND EXISTS (SELECT 1 FROM copies c JOIN locations l ON l.id = c.location_id
		              WHERE c.asset_id = a.id AND c.state = 'complete' AND l.kind = 'nas')
		ORDER BY capture_time, id`, spoolID)
}

func (c *DB) assets(ctx context.Context, q string, args ...any) ([]Asset, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Proxy is a regenerable derivative of an asset on one location. Proxies are
// tracked apart from copies so nothing ever counts one as a copy of the
// original.
type Proxy struct {
	AssetID    string
	LocationID int64
	RelPath    string
	Ext        string
	SHA256     string
}

func (c *DB) PutProxy(ctx context.Context, p Proxy) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO proxies (asset_id, location_id, relpath, ext, sha256) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(asset_id, location_id, ext) DO UPDATE SET relpath = excluded.relpath, sha256 = excluded.sha256`,
		p.AssetID, p.LocationID, p.RelPath, p.Ext, p.SHA256)
	return err
}

func (c *DB) DeleteProxy(ctx context.Context, assetID string, locationID int64, ext string) error {
	return c.update(ctx, `DELETE FROM proxies WHERE asset_id = ? AND location_id = ? AND ext = ?`, assetID, locationID, ext)
}

func (c *DB) Proxies(ctx context.Context, assetID string) ([]Proxy, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT asset_id, location_id, relpath, ext, sha256 FROM proxies WHERE asset_id = ? ORDER BY location_id, ext`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proxy
	for rows.Next() {
		var p Proxy
		if err := rows.Scan(&p.AssetID, &p.LocationID, &p.RelPath, &p.Ext, &p.SHA256); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SourceFile caches what a path on a source turned out to be, so a
// re-inserted card is recognised with a stat per file instead of a read.
type SourceFile struct {
	LocationID int64
	Path       string
	Size       int64
	ModTime    time.Time
	AssetID    string
}

func (c *DB) PutSourceFile(ctx context.Context, sf SourceFile) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO source_files (location_id, path, size, mtime, asset_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(location_id, path) DO UPDATE SET size = excluded.size, mtime = excluded.mtime, asset_id = excluded.asset_id`,
		sf.LocationID, sf.Path, sf.Size, formatTime(sf.ModTime), sf.AssetID)
	return err
}

// LookupSourceFile returns the asset previously recorded for this path if
// its size and mtime are unchanged, and ok=false otherwise.
func (c *DB) LookupSourceFile(ctx context.Context, locationID int64, path string, size int64, mtime time.Time) (assetID string, ok bool, err error) {
	var haveSize int64
	var haveMtime string
	err = c.db.QueryRowContext(ctx,
		`SELECT size, mtime, asset_id FROM source_files WHERE location_id = ? AND path = ?`, locationID, path).
		Scan(&haveSize, &haveMtime, &assetID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	t, err := parseTime(haveMtime)
	if err != nil {
		return "", false, err
	}
	if haveSize != size || !t.Equal(mtime) {
		return "", false, nil
	}
	return assetID, true, nil
}

// Import is one run against one source.
type Import struct {
	ID         int64
	LocationID int64
	StartedAt  time.Time
	FinishedAt time.Time
	New        int
	Skipped    int
	Failed     int
}

func (c *DB) BeginImport(ctx context.Context, locationID int64) (int64, error) {
	res, err := c.db.ExecContext(ctx, `INSERT INTO imports (location_id, started_at) VALUES (?, ?)`,
		locationID, formatTime(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (c *DB) FinishImport(ctx context.Context, id int64, newCount, skipped, failed int) error {
	return c.update(ctx, `UPDATE imports SET finished_at = ?, new_count = ?, skipped_count = ?, failed_count = ? WHERE id = ?`,
		formatTime(time.Now()), newCount, skipped, failed, id)
}

func (c *DB) Import(ctx context.Context, id int64) (Import, error) {
	var im Import
	var started, finished string
	err := c.db.QueryRowContext(ctx,
		`SELECT id, location_id, started_at, finished_at, new_count, skipped_count, failed_count FROM imports WHERE id = ?`, id).
		Scan(&im.ID, &im.LocationID, &started, &finished, &im.New, &im.Skipped, &im.Failed)
	if errors.Is(err, sql.ErrNoRows) {
		return Import{}, ErrNotFound
	}
	if err != nil {
		return Import{}, err
	}
	if im.StartedAt, err = parseTime(started); err != nil {
		return Import{}, err
	}
	im.FinishedAt, err = parseTime(finished)
	return im, err
}

// Times are stored as RFC 3339 with their offset, so a capture time keeps
// the zone it was interpreted in. The zero time is stored as "".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func parseKind(s string) media.Kind {
	switch s {
	case "video":
		return media.Video
	case "audio":
		return media.Audio
	case "still":
		return media.Still
	}
	return media.Unknown
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// AssetsByKind lists every asset in one tree, ordered by relpath, which is
// what the link tree reconciler walks.
func (c *DB) AssetsByKind(ctx context.Context, kind media.Kind) ([]Asset, error) {
	return c.assets(ctx, `SELECT `+assetCols+` FROM assets WHERE kind = ? ORDER BY relpath`, kind.String())
}

// AllLocations lists every location of every kind.
func (c *DB) AllLocations(ctx context.Context) ([]Location, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, kind, name, volume_uuid, label, priority, root FROM locations ORDER BY kind, priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		l, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
