Initial design for "mediamanager"


# Purpose

This is a tool for importing and managing video and still image files
from cameras, either directly (via USB-mountable cameras) or via flash
card readers.  It needs to be able to quickly recognize files that
have already been imported and skip them without attempting to read
large (multi-terabyte) files more times than absolutely required.

Our goals are:

- Make it possible to import and manage video files with minimal
  latency between inserting media containing new media into the system
  and it being available in its "permanant" filesystem location.
- Minimize the cost of migrating files between different storage
  locations.
- Never delete the last copy of an original file.
- Never confuse a partial copy of an original file with a complete
  copy.

# Video File Import Process

Its primary focus is on video files.  Video files can be large (I
routinely produce files >1 TB), which means that I don't have enough
local storage to keep them on my desktop for an extended period of
time.  This means that they need to get copied onto a NAS share.
However, my NAS isn't really fast enough to give full performance for
video editing.  So, I want to make a *local* copy first, then migrate
the files onto the NAS, *without changing the file path that my video
editor sees*.  Furthermore, actually copying files off of flash cards
and onto local disk is still a slow process, which means that I can't
really start editing until the copy completes, unless I fire up my
editor pointing to the flash card, which then means that once the card
is removed the video will show as "offline" until I re-point the
editor to the new location.

There's a relatively simple fix for both of these problems:

- Create a tree somewhere in the filesystem that consists of
  directories full of symlinks to video files.  No actual files are
  allowed in this tree, only symlinks.
- Then, several sources are used to populate this tree:
  - A video storage tree on the NAS, structured with one directory per
    project, or per day, or some other simple policy.  The link tree
    should mirror this structure.
  - One or more local spool directories that contains local copies of
    files from cameras.  These should *also* match the structure and
    naming of the NAS storage directory, and act as an overlay.  If a
    file exists on a local spool directory and on the NAS, then the
    symlink should point to the local copy.  If identical copies live
    on multiple spool directories, then the system should pick the
    first copy that it finds and symlink to it.
  - (Potentially) a copy on a flash card.  This will *not* have the
    same naming scheme as files for long-term storage, so we'll need
    to synthesize a "real" name and location, then create a symlink
    from the link tree directly to the local flash card.
- When a new media source is found (new flash card mounted on the
  system and an import operation is kicked off, etc), then
  `mediamanager` needs to scan it and identify all of the video files.
  For the moment, we'll just use the filename extension as a key, and
  treat `.mp4`, `.mov`, and `.braw` as video files.  The import needs
  to go something like this:
  - Each source file is given a permanant identifier that is static
    across any true copy of the file, but doesn't need a full read of
    the disk to generate.  The easiest way to do this is probably to
    compute a "sparse checksum" of the first and last 1 MB chunks of
    the file and then append this to the original filename plus maybe
    a date marker and use this as the permanant filename; this should
    take ~no time to do and should be stable in the face of *most*
    types of copy errors.  Alternatively, we could include additional
    1 MB chunks (one every GB, for example), but this will rapidly
    increase the cost of verifying files, especially when they live on
    HDDs.
  - Then, for each source file, we check a local store to see if we've
    seen it before.  This will probably end up being a SQLite DB, just
    because that's the least complicated way to do it, but we could
    probably also fall back to matching filenames in the filesystem.
  - If the file is new, then we go through this process:
    1. Create a symlink from the link tree (using the "permanant name"
       for the file) to the file on the flash card.
	2. Begin copying the file from the flash card into the local spool
       directory, using the permanant name for the file.
	3. Once step 2 completes, change the link tree to point to the
       local spool copy instead of the flash card.
	4. Once step 3 completes, copy the file from the spool directory
       onto the NAS.
  - If the file is not new *and the copy process completed
    previously*, then we can ignore it.  Otherwise we need to restart
    the copy.
- Then, periodically we'll need to flush files from the spool
  directory to free up space.  To do that, we'll need to verify that
  the "sparse checksum" from above matches both the spool copy and the
  copy on the NAS.  If the two copies match, and the files are the
  same size, then we can safely delete the locally spooled copy and
  update the link tree to point to the NAS copy.
- The only reason that this works is that video files are effectively
  read-only after creation.  It's possible to delete them and copy
  them from place to place, as well as deal with truncated copies, but
  nothing should ever modify them in place.
- No part of this system should ever delete any files from the NAS.
  The only operation allowed is to make a full copy of a file from a
  spool device onto the NAS.
- No part of this system should delete anything from a spool directory
  unless we have high confidence that there is a correct copy on the
  NAS.  Unfortunately, doing a multi-terabyte read is a very expensive
  way to do this, hence some sort of standardized sparse checksum that
  is designed to catch common copy failures at a reasonably (time)
  cost.  Triggering `sync` on each copy before updating a transaction
  in SQLite or similar may help prevent mid-file blocks from being
  lost due to write-back caches and power loss, but this isn't a major
  design worry of mine.
- No part of the system should attempt to put anything *except*
  symlinks and directories into the link tree.  If a "real" video file
  ends up in the link tree, then it needs to be flagged to the user's
  attention immediately, because something is seriously wrong.
- Note that system metadata files like .DS_Store and friends shouldn't
  count as local content in the link tree.  Macs love to spam this
  crap all over the place, and we should just ignore them and pretend
  that they don't exist.

The actual naming scheme needs to be at least semi-pluggable.  I'm
somewhat flexible and I've changed models a couple times in the past
when manually importing video.  As long as we can maintain a full
file-by-file link tree that matches the NAS and we're not in danger of
naming collisions, then I don't care if our scheme for importing
precisely matches the scheme for historical files.

I'm torn in two directions by the naming scheme.  Historically, I've
liked putting video into a tree in $YEAR subdirectories, and then
naming each logical project as
$YEAR/$YYYYMMDD-$PROJECTNAME/$ORIGINALFILENAME-$CHECKSUM.$EXTENSION.
The problem with this is that you can't actually import automatically
when media is imported, because $PROJECTNAME needs to come from
someplace, and it mostly needs to be provided by a human.  I only
shoot video on ~20 days per year, and rarely more than one project per
day, so this leaves me with a reasonable density of subdirectories
inside of each $YEAR directory.  However, without a project name,
we'll probably be better off with
`$YEAR/$MONTH/$DAY/$ORIGNALFILENAME-$CHECKSUM.$EXTENSION`.

We want to be able to support either importing or creating proxy video
files; these should live in a `proxy` subdir underneath the dir that
holds the video files, *and should match the name of the non-proxy
file*.  This poses a conflict with sparse checksum names, because if
we named proxy files using their own checksum, then video editors
would not be able to associate them with the non-proxy file.  Proxy
files are effectively just a generated, lower-resolution,
higher-compression copy of an original file, and can be re-created at
any point in time, as long as the original still exists.  So we'll
probably need special-case handling for proxies.  Also, we'll
eventually want to be able to add a proxy-generation step into the
import/video management workflow.

We'll probably also need to support audio files, imported from audio
recorders.  They'll have a different set of file extensions and will
be much smaller, but can otherwise be treated the same as video source
files.  We'll never need (or want) to deal with audio proxies.

# Still Photo Import Process

Still photos are *much* smaller than video files, and generally I care
less about copy latency.  A very busy photograph day may produce tens
of gigabytes of data across thousands of small-ish, while a busy video
day will produce multiple terabytes, possibly in as few as 1 or 2
files.  Also, still photos are managed via different software.

However, some cameras produce both stills and video, and I'd like to
have a single import process be able to handle both of them.  So, I'd
like to have still-specific link/spool/NAS directories, and then
populate them automatically during the import when importing from
something that looks like a still camera.

The still photo tree should just be
$YEAR/$MONTH/$DAY/$NAME.$EXTENSION.  Still photos don't need (or want)
checksums; the source filename plus size plus *maybe* date/time (or
EXIF data?) should be used for uniquifying them.  It is possible
(likely, eventually) for multiple images to end up with the same
filename, and we don't want to confuse them and lose one.  Generally,
no two cameras should produce files with identical names on the same
day, but we don't want to lose data if this happens.  I've seen
software rename on of the files to $NAME_1.$EXTENSION or similar.
This seems reasonable.

# Recognizing Input Devices

`mediamanager` needs to be able to handle both [Design rule for Camera
File
system](https://en.wikipedia.org/wiki/Design_rule_for_Camera_File_system)-structured
drives as well as "plain" devices that simply contain piles of video
content.  My four main devices today are:

- Assorted Panasonic Lumix cameras (either mounted via USB or by
  pulling their flash cards out and putting them in a reader).
- A Hasselblad X2D II (mounted via USB from its internal SSD)
- A Leica Q3 (either mounted directly or by pulling the flash card)
- Assorted Blackmagic Design video cameras (Pyxis 12k, etc), mounted
  via flash card reader today, but *potentially* over the network some
  day.

The first 3 should all use DCIM/ directories, while the last one just
drops video files into the root of the card's filesystem, possibly
plus a `proxy` subdir.

# Project structure

This should be written in Go, ideally without CGO, and live at
github.com/scottlaird/mediamanager.  The bulk of the code should be a
library, with tool(s) that use the library for manual operations.

Phase 1 of the project should just be the library plus a minimal
import tool that will use library code to implement the rules above.
See phase 2 before designing phase 1, however.

Phase 2 will move to using Temporal to handle the state around copying
files and maintaining the link tree.  When starting the import of
(potentially) new media content, a set of Temporal workflows should be
kicked off to manage the actual copy process.  Temporal handles
crashing, restarting, etc, and should make it easier to deal with some
of the invariants that need to be maintained.  Eventually, we'll also
want to add proxy-generation to Temporal.  We will need to be able to
run everything locally on a Mac, but the code needs to support either
MacOS, Linux, or presumably any other supported Unix-like OS.  I don't
care about Windows particularly and have no desire to test against it,
but let's avoid being gratiously incompatible.

We want to limit ourselves to a small number of concurrent copy
processes.  We'd like to be able to import multiple cameras at once,
but we don't want to try to copy 50 video files to the NAS
concurrently.  Limiting copies to 1 per source device would be
reasonable, or perhaps 2-5 concurrent copies system-wide would be
fine.  Being able to adjust these would be nice-ish, but not critical.

This is (partly) an explicit attempt to use Temporal for a personal
project, so arguments like "it doesn't really add a whole lot to the
design of the system" aren't persuasive.  On the other hand "this
won't work like this," or "there are serious problems with using
Temporal this way" may be able to steer the design.
