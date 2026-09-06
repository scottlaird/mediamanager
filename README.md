# mediamanager

Tool for managing import of video, audio and photo files from cameras.

`mm` scans a mounted card or camera, gives every file a permanent,
content-derived name, links it into a symlink tree the editor can open
immediately, copies it to a fast local spool and then on to the NAS, and
later frees the spool once the NAS copy is verified. The editor only ever
sees the link tree, so files move between tiers without a path changing.

See `DESIGN.md` for the requirements and the rules the code enforces.

## Install

    go install github.com/scottlaird/mediamanager/cmd/mm@latest

## Configure

`~/.config/mediamanager/config.yaml`:

```yaml
trees:
  video: {link: ~/Video}
  audio: {link: ~/Audio}
  still: {subdir: stills, link: ~/Pictures/Import}
locations:
  - name: nas
    kind: nas
    path: /Volumes/video/mediamanager
  - name: fast
    kind: spool
    volume_uuid: 1234ABCD-0000-4000-8000-000000000000   # from `mm volume /Volumes/Fast`
    path: mediamanager                                   # relative to that volume
    priority: 1
```

Each location holds one subdirectory per tree, so spools and the NAS
share a layout. Pin spools to a volume UUID rather than a path: macOS
mounts a second disk with the same name as `/Volumes/Name 1`.

## Use

    mm import /Volumes/CARD            # register, link, spool, archive
    mm status --assets                 # where everything is
    mm flush fast --free 2T            # free spool space, oldest first
    mm spool 2026/07/10 --pin          # bring a day back from the NAS, keep it
    mm relink                          # repoint links after mounts change
    mm archive                         # finish any NAS copies left over

`mm import` prints whether every file on the card has reached the NAS. It
never erases a card; format it in the camera.

## Proxies and sidecars

Editors write beside the file they opened, and the file they opened is a
link. Sidecars (`.sidecar`, `.xmp`) and proxies that appear in the link tree
are swept into storage beside the original, recorded, copied to the NAS
with it, and linked back, so pointing Blackmagic Proxy Generator's watch
folder at `~/Video` produces proxies that follow their clips through
spool, NAS and flush.

## Temporal (optional)

With a `temporal:` section in the config, `mm import`, `mm archive` and
`mm flush` run as Temporal workflows instead of in-process, which gives
them crash recovery, retries with backoff, and a history you can read in
the Temporal UI. For one machine:

    temporal server start-dev --db-filename ~/.local/share/mediamanager/temporal.db
    mm worker                                 # in another terminal, keep running

```yaml
temporal: {}        # defaults: localhost:7233, namespace default, task_queue mediamanager
```

Then the usual commands start workflows and wait for them; `--detach`
returns immediately and `--local` runs in-process regardless. Copies to
the NAS run on their own task queue bounded by `concurrency.nas`. An
import that is interrupted resumes where it was the next time the worker
runs; a pulled card never stops a NAS copy already under way.
