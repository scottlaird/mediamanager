# Mediamanager build brief

> **Status.** This is the build brief written from `DESIGN.md` on 2026-09-05, before any code existed, updated through the first review rounds. The code on `main` follows it with these deviations, all deliberate:
>
> - `naming` and `media.Kind` are their own packages rather than living in `identity`; `volume` and `config` are separate.
> - Locations are shared across kinds: each tree is a `subdir` under every location (`"."` for a volume that is the tree), rather than per-kind location roots.
> - Proxies became one case of *companions* (`proxy`, `sidecar`, `proxy-sidecar`); sidecars are the one mutable file class and are kept current on the NAS. Editor-created sidecars and generated proxies found in the link tree are swept into storage rather than failing the audit.
> - Q10 was answered with two `mm adopt` modes, in-place and migrate; the migrate mode is a deliberate one-off exception to R3.
> - `mm spool` (the reverse of flush), `mm ls`, `mm archive --dry-run` and copy timing in results were added on request.
> - Phase 2 uses two task queues (`<base>`, `<base>-nas`) with a worker concurrency cap for NAS writes instead of a device-gate mutex workflow; per-device serialisation comes from `ImportSource` spooling sequentially. `ArchiveAsset` workflows are named by path.
> - Resumed copies do not re-hash the reused prefix by default.
>
> Q4 (project names) remains open. `docs/nas-tuning.md` covers the NAS side that this brief takes as given.

A Go library and CLI that ingests video, audio and stills from cameras and cards, stages them through fast local spools onto a NAS, and keeps a symlink tree pointing at the best available copy so the editor's paths never change. **Repo** github.com/scottlaird/mediamanager **Language** Go, no cgo **Targets** macOS first, Linux, other Unix **Status** draft 4 · 2026-09-05 · Q4 and Q10 open

## Summary

The problem is latency and capacity at the same time. A shoot day can produce several terabytes in one or two `.braw` files; the desktop can't hold them for long, and the NAS can't feed the editor at full speed. The tool's job is to make new footage editable the moment a card is mounted, move it through local spool storage onto the NAS in the background, and later free the spool, all without the editor ever noticing a path change.

Four properties define success, in order of importance:

1. Never delete the last copy of an original.
2. Never mistake a partial copy for a complete one.
3. Minimal delay between mounting media and having it at its permanent path.
4. Migration between storage tiers costs as little I/O as possible, and never a full re-read of a multi-terabyte file just to prove it's intact.

Everything else in this document is in service of those four. The one structural idea that makes it work: **video, audio and still originals are immutable**. They can be copied, truncated by failure, and deleted, but never modified in place.

> **Assumption this whole design rests on:** Resolve stores the path it was handed (the symlink) rather than the resolved real path. Since it runs on Windows as well as Unix, resolving symlinks would be work for no gain, so this is assumed true; Phase 0 confirms it in ten minutes rather than gating on it.

## Storage tiers

Three kinds of storage hold real bytes; a fourth holds only symlinks. The link tree is a *derived view*: for each asset it points at the highest-priority complete copy that currently exists. Nothing writes to the link tree imperatively; a reconciler regenerates it from the catalog whenever a copy appears or disappears.

- Link tree symlinks and directories only · the only path the editor sees · `~/Video/2026/…`

- **Source** — flash card, camera over USB mass storage, someday a network share · Priority: 3 (fallback only) · Writes: never · Lifetime: until unplugged

- **Spool** — one or more local fast volumes, ordered by configured priority · Priority: 1 · Writes: copy in, delete after NAS verified · Lifetime: until flushed

- **NAS** — permanent archive, SMB/NFS mount, slow for editing · Priority: 2 · Writes: copy in only, never delete · Lifetime: forever

Spool and NAS share one directory layout, so the spool is a pure overlay. Sources do not: the tool synthesizes a permanent name and directory for each source file at discovery time, and the link tree uses that synthesized location from the first second.

Three parallel trees exist for the three media kinds, each with its own link, spool and NAS roots: `video`, `audio`, `still`. One import run feeds all three, routing per file.

## Hard rules

These are invariants, not preferences. Each gets a test. Code refers to them by ID.

- **R1** **Never delete the last complete copy.** A delete requires a *verified* complete copy on the NAS to exist first, and the file being deleted must not be the one that was just verified.
- **R2** **Partial and complete are distinguishable on disk, not just in the database.** In-progress copies live at `NAME.ext.partial` and are renamed into place only after fsync and verification. A file with its final name is complete, by construction, on every tier.
- **R3** **The NAS is append-only at the file level.** The tool may create directories, write new files, and manage its own `.partial` files. It never deletes, truncates, overwrites or renames a completed file on the NAS. (Directory renames for project naming are addressed in Q4 and are off by default.)
- **R4** **Spool deletion needs a verified NAS copy.** "Verified" means size matches and the sparse checksum recomputed *from the NAS copy* matches the catalog identity. The full-content hash is compared too if it was recorded at copy time.
- **R5** **Only symlinks and directories in the link tree.** An audit runs at the start of every import and flush. Any regular file (after filtering platform junk) halts the run and is reported loudly. The tool never moves or deletes it.
- **R6** **Sources are read-only.** No file on a card or camera is ever written, renamed or deleted. "Safe to format" is a report, not an action.
- **R7** **Identity is content-derived and copy-stable.** The same bytes produce the same identity on every tier, on every machine, regardless of path. The catalog can be rebuilt from the filesystems alone.
- **R8** **Every catalog write that records progress happens after the corresponding fsync.** Rename-into-place, then commit. A crash between the two is recovered by the reconciler seeing a complete file the catalog doesn't know about, which is a safe direction to be wrong in.

## Identity and naming

### Sparse checksum (video and audio)

The identity of a large original must cost roughly zero I/O to compute and must be stable across true copies. Reading the whole file is out of the question, so:

```
id = hex(SHA-256( u64be(size) ‖ bytes[0 : 1 MiB) ‖ bytes[size-1 MiB : size) ))[0:16]
small = if size ≤ 2 MiB: hex(SHA-256( u64be(size) ‖ whole file ))[0:16]
scheme = "s1" // versioned; stored alongside so the algorithm can change later
```

Including the size makes a truncated copy fail identity by construction rather than by luck. Sixteen hex characters (64 bits) is plenty for a personal archive of tens of thousands of files and keeps names readable. Reading two 1 MiB extents from an exFAT card or an SMB share takes milliseconds.

What it does *not* catch: corruption in the middle of the file. That is a deliberate trade. To narrow it cheaply, every copy the tool performs also computes a **full SHA-256 as a side effect of streaming the bytes**, since it is reading them anyway. The full hash is recorded in the catalog and used opportunistically: the spool→NAS copy re-reads the spool file and so verifies it for free, and the hash of what was sent to the NAS is kept for any future full verification the user chooses to run. Only *re-verification by re-reading* is expensive, and the sparse checksum is what stands in for it.

### Permanent names

| Kind | Layout (spool and NAS, mirrored in link tree) | Example |
|---|---|---|
| Video | `$YEAR/$MM/$DD/$ORIGBASE-$ID.$ext` | `2026/09/05/a001_09051412_c003-3f9a1c2e7b4d5a60.braw` |
| Audio | same as video, in the audio tree | `2026/09/05/zoom0012-91ab77e0c2d3f410.wav` |
| Still | `$YEAR/$MM/$DD/$ORIGBASE.$ext`, `_1`, `_2`… on name clash with different content | `2026/09/05/l1004821.dng` |
| Proxy | `…/Proxy/$ORIGBASE-$ID.$proxyext`, next to its original | `2026/09/05/Proxy/a001_09051412_c003-3f9a1c2e7b4d5a60.mp4` |

Naming is a pluggable `Scheme` interface: given an asset's metadata it returns a relative path. The date-based scheme above is the default because it needs no human input at import time. A `$YEAR/$YYYYMMDD-$PROJECT/` scheme is also implemented so historical directories resolve, and so a project-named layout is available if Q4 lands that way. Output names are forced to lowercase, base name and extension alike: FAT-derived card filesystems and default APFS are both case-insensitive, so no two source files can differ only by case and nothing is lost, while the NAS and link tree stop depending on case at all. The single exception is the `Proxy/` directory, which keeps the capitalisation Blackmagic writes and Resolve looks for. Matching on input is always case-insensitive.

**Legacy files** already on the NAS without an `-$ID` suffix are catalogued by path with a lazily computed identity and are never renamed (R3). They appear in the link tree under their existing names.

### Proxies

A proxy is a regenerable derivative, not an asset. It shares its parent's base name (including the parent's `-$ID`) so the editor can pair them, differs only in extension and the `Proxy/` directory, and its own content hash is recorded but never used for naming. Consequences:

- Camera-generated proxies found on a source are imported *after* their parent's identity is known; a proxy with no parent on the same source is reported and skipped.
- Proxies follow the parent through the tiers but with relaxed rules: a spool proxy may be deleted once the parent is archived even if no NAS proxy exists, because it can be regenerated. This is the one carve-out from R1 and it applies to proxies only.
- Proxy generation is a later workflow step; phase 1 only imports and links existing ones.

### Stills

Stills get a full SHA-256 at import (cheap even for 200 MB X2D files) as their identity, but the hash stays out of the filename. Dedup on rescan is a two-stage check: first `(volume id, path, size, mtime)` against the catalog's source cache, and only on a miss is the file hashed. Two different files that want the same name on the same day get `NAME_1.ext`, `NAME_2.ext`. Raw+JPEG pairs are two independent assets that happen to share a base name.

## The copy primitive

One function does every byte-moving operation in the system, and it is the most carefully tested code in the repo.

1. **Open**: source for read, `dest.partial` for append
2. **Resume check**: if a partial exists, compare its last 1 MiB with the source at that offset; on match continue, on mismatch truncate to zero
3. **Stream**: large buffered reads, SHA-256 of every byte as it passes, progress callback every N MiB for heartbeats
4. **Fsync**: the file, then the directory
5. **Verify**: size, then sparse checksum recomputed *from the destination* against the expected identity
6. **Preserve**: mtime from source; no chmod games
7. **Rename**: `.partial` → final name (atomic on the same filesystem)
8. **Report**: return identity, full hash, bytes copied; caller commits to catalog

Idempotent by design: calling it again on a completed destination is a no-op after a cheap verify; calling it on an interrupted one resumes. That property is exactly what a Temporal activity retry needs in phase 2, and what a plain re-run of the CLI needs in phase 1.

Concurrency limits are enforced by the caller through two semaphores: one per source device (default 1, so a card reader is never asked to seek between two files) and one for NAS writes system-wide (default 3). Spool→NAS copies and card→spool copies for different devices can overlap.

## Asset lifecycle

An asset moves through these states. The "link points to" column is not stored; it is what the reconciler derives from the "complete copies" column plus priority. It is shown to make the behaviour concrete.

| State | Complete copies exist on | Link points to | Transition out |
|---|---|---|---|
| `discovered` | source | source | spool copy starts |
| `spooling` | source (+ spool `.partial`) | source | spool copy verified & renamed |
| `spooled` | source?, spool | spool | NAS copy starts |
| `archiving` | source?, spool (+ NAS `.partial`) | spool | NAS copy verified & renamed |
| `archived` | source?, spool, NAS | spool | flush deletes spool copy |
| `flushed` | NAS | NAS | terminal; can return to `archived` if re-spooled for editing |

Pulling the card mid-`spooling` leaves a dangling link and a `.partial`; nothing is lost, the editor shows the clip offline, and re-inserting the card resumes. Pulling it during `spooled` or later has no effect at all, which is the point. If a source file is larger than the free space on every spool, phase 1 fails that asset with a clear message and leaves its link pointing at the source; everything else on the card still imports. The long-term behaviour is for that condition to trigger a flush, which is not specified yet (Q7).

## Link tree reconciler

A pure function from catalog state to desired tree, applied with the minimum set of filesystem changes.

```
for each asset:
 want = first complete copy in [spools in priority order…, NAS, sources currently mounted]
 have = readlink(linkTree / asset.relpath)
 if have != want: symlink(want, tmp); rename(tmp, linkTree / asset.relpath) // atomic swap
 for each proxy of asset: same, under …/Proxy/
create missing directories; leave empty ones alone
report: dangling links whose asset has no complete copy anywhere (should be impossible unless a card is unplugged)
```

Absolute symlink targets, since the link tree is machine-local and the spool and NAS mount points are per-machine config. macOS complicates this: a volume whose name is already taken mounts as `/Volumes/Video 1`, so a configured path can silently point at the wrong disk or at nothing. Spools and the NAS are therefore identified by volume UUID (label as fallback) and their current mount point is resolved at the start of every run; the reconciler treats a changed mount point as just another reason the tree is stale and rewrites the affected links, which are cheap. A location whose volume cannot be found is skipped for that run, never assumed to be empty. The audit (R5) runs first: walk the link tree, ignore the junk list below, and abort on any regular file.

**Junk list**, ignored everywhere and never counted as content: `.DS_Store`, `._*`, `.Spotlight-V100`, `.fseventsd`, `.Trashes`, `.TemporaryItems`, `Thumbs.db`, `desktop.ini`, and the tool's own `*.partial`.

## Catalog

SQLite via `modernc.org/sqlite` (pure Go), WAL mode, one file per machine. It is an *index*, not the source of truth: every table below can be rebuilt by walking the spool and NAS roots, because identity is in the filenames (R7) and completeness is in the absence of `.partial` (R2). `mm rebuild` does exactly that.

| Table | Key columns | Purpose |
|---|---|---|
| `assets` | id, scheme, kind (video/audio/still), size, full_sha256?, orig_name, capture_time, relpath, project? | one row per original |
| `copies` | asset_id, location_id, relpath, state (partial/complete), verified_at, full_sha256? | where bytes live; R1/R4 decisions read only rows with `state=complete` |
| `locations` | id, kind (source/spool/nas), root, priority, volume_uuid, label | spools and NAS from config; sources discovered at mount |
| `proxies` | asset_id, location_id, relpath, ext, sha256 | derivatives, tracked separately from copies so R1 never counts them |
| `source_files` | volume_uuid, path, size, mtime, asset_id | rescan cache: lets a re-inserted card be skipped with a `stat` per file |
| `imports` | id, source location, started, finished, counts | run history for `mm status` |

The catalog is owned by whichever process is running; phase 1 is single-process with a lock file. In phase 2 only the Temporal worker writes it.

## Sources and scanning

A source is any mounted directory. Two shapes are recognised, and the shape only changes *where* the scanner looks, never how a file is routed:

#### DCIM (Design rule for Camera File system)

Lumix, Hasselblad X2D II, Leica Q3. Walk `DCIM/*/`. Lumix also writes `PRIVATE/AVCHD/` for AVCHD, which is skipped unless configured. Sidecars (`.XML`, `.THM`, `.LRV`) are ignored in phase 1.

#### Flat

Blackmagic cameras (Pyxis 12K and friends). Media files at the volume root, optional `Proxy/` subdirectory alongside, matched case-insensitively.

Routing is per file, by extension, case-insensitively. A Q3 card with DNGs and MOVs feeds two trees from one run.

| Kind | Extensions (initial, configurable) | Identity | Tree |
|---|---|---|---|
| Video | `braw mov mp4` | sparse | video |
| Audio | `wav flac mp3` | sparse | audio |
| Still | `jpg jpeg heif heic dng rw2 3fr tif tiff` | full hash | still |
| Proxy | video extensions found under a `Proxy/` dir | parent's | video, under parent |

Sources are identified by volume UUID where the OS exposes one (`diskutil` on macOS, `blkid`/`/dev/disk/by-uuid` on Linux), falling back to volume label plus a fingerprint of the first few directory entries. This only drives the rescan cache; a misidentified card costs a few extra 2 MiB reads, never a wrong decision. In scope: anything that appears as a mounted filesystem. Out of scope: PTP/MTP devices that only show up in Image Capture; cards go in a reader.

**Capture time** for the `$YEAR/$MM/$DD` path comes from embedded metadata when it can be read from the first few KB (EXIF `DateTimeOriginal` for stills, the `mvhd` creation time for MP4/MOV, the BRAW header), otherwise file mtime, interpreted in the importing machine's local timezone. Local time keeps an evening session on one day, which matters more at month and year boundaries than exact UTC would.

## Flows

### Import

1. **Audit**: link tree contains only links and dirs (R5)
2. **Scan**: walk the source, classify each file, compute identity (stat-cache hit skips this)
3. **Register**: new assets in the catalog; state `discovered`; source copy recorded complete
4. **Reconcile**: link tree now points every new asset at the source: editable immediately
5. **Spool**: copy primitive per asset, per-device semaphore; reconcile after each completion
6. **Archive**: copy primitive spool→NAS, NAS semaphore; reconcile after each completion
7. **Report**: what was new, what was skipped, what is still in flight, whether the card is safe to format

Steps 5 and 6 pipeline: asset B can be spooling while asset A archives. A re-run of import on the same card skips complete assets, resumes partial ones, and is otherwise harmless.

### Flush

1. **Select**: candidates in state `archived`, oldest first, until the requested space is freed
2. **Verify NAS**: size + sparse checksum recomputed from the NAS copy; full hash if the catalog has one for both
3. **Verify spool**: same check on the spool copy, so the two are known to agree, not just each to match the catalog
4. **Delete**: the spool file; mark that copy row gone
5. **Reconcile**: link moves to the next-best copy (another spool, or the NAS)

Phase 1 flush is manual (`mm flush --free 2T` or `--older-than 30d`), oldest-archived first, pinned assets skipped. Phase 2 adds a per-spool low-water mark that runs the same policy; anything smarter would need to know about editing workflows, which the tool does not.

### Field import *[later]*

Importing on a laptop to an external drive while away, then attaching that drive to the desktop, is a stated future need. Nothing in phase 1 is built for it, and two things keep the door open:

- The zero-code version works from day one: copy cards verbatim onto the external drive, one directory per card, and the desktop treats that drive as a *source*. Identity is content-derived (R7), so nothing about the copy step matters except that it was complete.
- The proper version is a NAS-less profile of the same tool running on the laptop, writing a spool in permanent names. On arrival the desktop adopts the drive as a low-priority spool: `mm rebuild` already knows how to catalogue a spool root from filenames and `.partial` markers, so this is a config entry plus a catalog merge, not a redesign.

### Other operations

- `mm status`: per-asset state, in-flight copies with progress, spool fill level, dangling links.
- `mm verify [--full]`: sparse (or full) re-verification of chosen copies; reports, never deletes.
- `mm rebuild`: reconstruct the catalog from spool and NAS roots, then reconcile.
- `mm pin / unpin`: keep an asset in the spool through flushes; re-spool a flushed asset from the NAS for editing.
- `mm adopt <nas-dir>`: catalogue legacy files in place.

## Phase 0 *[before any code]*

Two quick checks that confirm what the design assumes. Neither is expected to fail, so they are a morning's sanity check rather than a gate, but the second one settles a naming rule that is expensive to change later.

1. Create a symlink to a clip on a card, import the symlink path into the editor, swap the symlink to a local copy with `ln -sfn` via a rename, and eject the card. Does the clip stay online? Does the project file record the symlink path or the real one?
2. Put a `proxy/` (and separately `Proxy/`) directory next to a renamed original containing a same-base-name proxy and see whether the editor auto-links it. This fixes the proxy naming and casing rules.

## Phase 1 *[library + CLI]*

The library is written so that every step of the flows above is already an idempotent, resumable, heartbeat-capable function with plain arguments and results, meaning a Temporal activity in everything but registration. The phase 1 driver is a small Go orchestrator with goroutines and the two semaphores; phase 2 replaces only the driver.

```
github.com/scottlaird/mediamanager
├── identity/ sparse + full identity, Scheme interface, date and project schemes
├── scan/ source walking, DCIM/flat detection, classification, junk filter, capture-time extraction
├── copyfile/ the copy primitive, resume, verify, fsync discipline
├── catalog/ SQLite schema, migrations, queries, rebuild-from-filesystem
├── linktree/ reconciler and audit
├── policy/ config: roots per kind, spool priority, extension tables, concurrency limits
├── ingest/ step functions (activities) + phase-1 in-process driver
├── cmd/mm/ cobra CLI: import, flush, status, verify, rebuild, adopt, pin
└── internal/testfs/ fixture builders: fake cards with DCIM/flat layouts, truncated files, junk
```

Config is a single YAML file (`~/.config/mediamanager/config.yaml`) read with `go.yaml.in/yaml/v4`, the maintained home of go-yaml, chosen over stdlib JSON because it is editable by hand without fighting syntax. Tests are stdlib `testing`, table-driven, got/want. The copy primitive and reconciler get the heaviest coverage: crash between fsync and rename, crash between rename and catalog commit, truncated source, resumed partial with mismatched tail, card removed mid-copy, real file planted in the link tree, two spools holding the same asset.

Phase 1 is done when: a Blackmagic card and a Lumix card can be imported concurrently from a cold start; the editor sees clips within seconds of mounting; killing the process at any point and re-running completes the import; a flush frees space and the editor still sees every clip; and `rm catalog.db && mm rebuild` reproduces the same link tree.

## Phase 2 *[Temporal]*

Temporal takes over the orchestration layer. The catalog, the copy primitive, the reconciler and the CLI stay; the in-process driver is replaced by workflows, and the CLI becomes a client that starts them and queries their state.

### Shape

| Workflow | Started by | Does |
|---|---|---|
| `ImportSource(sourceID, mount)` | `mm import`, later a mount watcher | Audit → Scan → Register, then starts one `IngestAsset` child per new or incomplete asset with parent-close policy `ABANDON`, so ejecting a card, or the import workflow finishing, never cancels an in-flight NAS copy. Waits for children only to report. |
| `IngestAsset(assetID)` | `ImportSource` | Reconcile → acquire device slot → CopyToSpool → release → Reconcile → acquire NAS slot → CopyToNAS → release → Reconcile. One long-running activity per copy with byte-offset heartbeats; retry resumes from the partial. |
| `FlushSpool(spoolID, target)` | `mm flush`, later a fill-level trigger | Select → per asset: VerifyNAS, VerifySpool, Delete, Reconcile. Continue-as-new every few hundred assets. |
| `DeviceGate(deviceID)` | lazily, by the first requester | The Temporal mutex pattern: a long-lived workflow that hands out one slot at a time via update/signal. Per-device concurrency is not something task queues express well, and this keeps the limit in one place. |
| `GenerateProxy(assetID)` (later) | `IngestAsset` or on demand | ffmpeg activity on a dedicated task queue. |

### Things Temporal does not change

- Workers run on the machine with the mounts. There is exactly one worker process on the Mac; the dev server (`temporal server start-dev --db-filename …`) runs beside it so state survives restarts.
- NAS-wide concurrency is the worker's activity concurrency on a dedicated `nas-copy` task queue. Device concurrency is `DeviceGate`.
- Payloads carry IDs, never file lists or byte offsets beyond the heartbeat. Per-asset child workflows keep every history small; a 2 TB copy is one activity with a multi-hour start-to-close timeout and a short heartbeat timeout.
- An unplugged card fails the activity with a non-retryable "source gone" error; `IngestAsset` then waits on a signal from the next `ImportSource` run that sees the same volume UUID, rather than retrying blindly for days.

### Real risks worth knowing before committing

- The Go SDK brings gRPC and protobuf into the module graph. Pure Go, no cgo, but a large dependency for a personal tool; it is the price of the experiment and it conflicts with nothing else here.
- Temporal is unaware of the filesystem. Every invariant in R1–R8 still has to hold inside activities; Temporal only guarantees the activity is retried, not that it was safe to.
- If the dev server's database is lost, in-flight workflows vanish. The design tolerates this because `mm import` re-run is idempotent and `mm rebuild` recovers the catalog, but it means Temporal state is treated as disposable, which is somewhat against its grain.

## Dependencies needing a yes

| Dependency | Phase | Why not stdlib |
|---|---|---|
| `github.com/spf13/cobra` | 1 | already approved |
| `modernc.org/sqlite` | 1 | pure-Go SQLite; the alternative is a JSON catalog file, which works for phase 1 but makes `mm status` queries and concurrent updates in phase 2 painful |
| an EXIF/TIFF reader (e.g. `github.com/dsoprea/go-exif/v3`) or a ~200-line internal parser for `DateTimeOriginal` | 1 | stdlib has no EXIF. The internal parser is realistic since only one tag is needed; DNG, RW2 and 3FR are all TIFF-shaped |
| `go.yaml.in/yaml/v4` | 1 | YAML config; pure Go, no transitive dependencies. v4 is at `v4.0.0-rc.6` (June 2026); the tool only calls `Unmarshal` into a struct, which is stable across the rc line. Drop to `go.yaml.in/yaml/v3` (v3.0.5) only if the rc misbehaves. |
| `go.temporal.io/sdk` | 2 | the point of phase 2 |

Everything else, including SHA-256, MP4 `mvhd` parsing and filesystem walking, is standard library.

## Questions

### Resolved

Answered in the first review; the rest of the brief already reflects these.

- **Q1** Does Resolve keep the given path? — **Yes** assumed; cross-platform, no reason to resolve symlinks. Phase 0 confirms.
- **Q2** Which timestamp and timezone for `$YEAR/$MM/$DD`? — **Decided** embedded capture time when cheap, else mtime; machine-local zone so a session stays on one day.
- **Q3** Portable spools? — **Decided** fixed local spools now; field import is a later requirement, see the Field import section for the two paths that stay open.
- **Q5** PTP/MTP devices? — **Decided** mass storage only.
- **Q6** Erase cards? — **Decided** never; erase in camera. R6 stands.
- **Q7** File larger than free spool space? — **Decided** fail that asset in phase 1; eventually it should trigger spool cleanup, which needs its own spec.
- **Q8** Flush trigger and policy? — **Decided** manual in phase 1, low-water mark in phase 2; oldest-archived first, pinned assets skipped. The spool-full case from Q7 hooks in here later.
- **Q9** Filename casing? — **Decided** force everything lowercase except the `Proxy/` directory.
- **Q11** Identity hash length in filenames? — **Decided** 16 hex characters.
- **Q12** Symlinks absolute or relative? — **Decided** absolute, with locations resolved by volume UUID at run time so macOS's `/Volumes/Name 1` collisions are detected rather than followed.
- **Q13** Config format? — **Decided** YAML via `go.yaml.in/yaml/v4`, falling back to v3 only if needed.
- **Q14** Dependency approval? — **Approved** `modernc.org/sqlite`, `go.yaml.in/yaml/v4`, cobra; EXIF via an internal one-tag TIFF reader. Temporal SDK to be confirmed again at phase 2.

### Still open

Two remain. Each carries the assumption the brief is written to; a different answer changes the design in the way noted. Neither blocks the identity, copy or catalog packages.

- **Q4** Project names: prompt at import, rename NAS directories later, or keep `$YYYYMMDD-$PROJECT` only in the link tree with `$YEAR/$MM/$DD` on the NAS? — **Recommended** the last. The editor sees only the link tree; the NAS stays boring and append-only; `mm project set 2026-09-05 "boat-launch"` restructures the link tree instantly and records the name in the catalog. Downside: the project name lives in the catalog, not the NAS. A one-line `.project` file per NAS day-directory would fix that at the cost of a small R3 carve-out.
- **Q10** Historical NAS content: adopt in place under existing names, or leave uncatalogued? — **Assumed** adopt in place via `mm adopt`, identity computed lazily, never renamed, existing casing left alone.
