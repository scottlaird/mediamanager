CREATE TABLE IF NOT EXISTS locations (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL CHECK (kind IN ('source', 'spool', 'nas')),
    name        TEXT    NOT NULL UNIQUE,
    volume_uuid TEXT    NOT NULL DEFAULT '',
    label       TEXT    NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 0,
    root        TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS assets (
    id           TEXT    PRIMARY KEY,
    scheme       TEXT    NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('video', 'audio', 'still')),
    size         INTEGER NOT NULL,
    full_sha256  TEXT    NOT NULL DEFAULT '',
    orig_name    TEXT    NOT NULL,
    capture_time TEXT    NOT NULL,
    relpath      TEXT    NOT NULL,
    project      TEXT    NOT NULL DEFAULT '',
    pinned       INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL,
    UNIQUE (kind, relpath)
);

CREATE TABLE IF NOT EXISTS copies (
    asset_id    TEXT    NOT NULL REFERENCES assets(id),
    location_id INTEGER NOT NULL REFERENCES locations(id),
    relpath     TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN ('partial', 'complete')),
    verified_at TEXT    NOT NULL DEFAULT '',
    full_sha256 TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (asset_id, location_id)
);
CREATE INDEX IF NOT EXISTS copies_by_location ON copies (location_id, state);

CREATE TABLE IF NOT EXISTS proxies (
    asset_id    TEXT    NOT NULL REFERENCES assets(id),
    location_id INTEGER NOT NULL REFERENCES locations(id),
    relpath     TEXT    NOT NULL,
    ext         TEXT    NOT NULL,
    sha256      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (asset_id, location_id, ext)
);

CREATE TABLE IF NOT EXISTS source_files (
    location_id INTEGER NOT NULL REFERENCES locations(id),
    path        TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    mtime       TEXT    NOT NULL,
    asset_id    TEXT    NOT NULL REFERENCES assets(id),
    PRIMARY KEY (location_id, path)
);

CREATE TABLE IF NOT EXISTS imports (
    id          INTEGER PRIMARY KEY,
    location_id INTEGER NOT NULL REFERENCES locations(id),
    started_at  TEXT    NOT NULL,
    finished_at TEXT    NOT NULL DEFAULT '',
    new_count     INTEGER NOT NULL DEFAULT 0,
    skipped_count INTEGER NOT NULL DEFAULT 0,
    failed_count  INTEGER NOT NULL DEFAULT 0
);
