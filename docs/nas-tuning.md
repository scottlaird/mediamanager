# fs2 SMB Tuning Notes

What had to change to get usable write and read throughput between the Mac and the ZFS-backed Samba share, what each change was measured against, and which signal to watch when revisiting a value.

*2026-09-06 · fs2: Ubuntu 24.04, 7 × raidz2 × 8 HDD, 256 GB RAM, ConnectX-5 Ex · Mac Studio: macOS 26, ConnectX-5 Ex on TB5 (PCIe 4.0 ×4), SMB 3.1.1*

## Outcome

| Measurement | Before | After | How it was measured |
|---|---|---|---|
| Write, 2 concurrent copies, aggregate | ~0.8 GB/s, sawtooth to 0 | 2.0–2.3 GB/s, steady | `netstat -I en14 -w 1` on the Mac; `zpool iostat 1` on fs2 |
| Cold read, one stream | 210–275 MB/s | 440 MB/s | `dd` from an untouched region of a large file |
| Warm read (in ARC), one stream | 1.4 GB/s | 1.4 GB/s | same, on a region fs2 had just read locally |
| Server READ turnaround, median / p90 | 4.7 / 12.9 ms | 2.7 / 6.8 ms | `tcpdump` on fs2, request/response joined by message ID |
| Resolve, 8K clip from the NAS | ~15 fps, constant stalls | real-time, occasional hitch | playback |

## Write path

### What was wrong

Writes arrived at ~1.5 GB/s and stalled to zero every 3–4 seconds. Both `zfs_dirty_data_max` and its ceiling `zfs_dirty_data_max_max` sat at the 4 GiB default cap. ZFS throttles writers hard once dirty data passes 60% of the max, and at that rate 2.4 GiB is under two seconds of buffering, so any transaction-group commit that ran long put the brake on the client. Per-connection, macOS also caps SMB socket buffers at 2 MiB.

### What changed

```
# /etc/modprobe.d/zfs.conf (fs2) — needs reboot: _max_max is read-only at runtime
options zfs zfs_dirty_data_max_max=34359738368 zfs_dirty_data_max=17179869184

# macOS, per new session (persist in /etc/sysctl.conf)
sudo sysctl -w kern.ipc.maxsockbuf=16777216 net.smb.fs.tcpsndbuf=8388608 net.smb.fs.tcprcvbuf=8388608
```

Multichannel added ~10%: four TCP connections to one address once Samba advertised the connected interface as RSS-capable. Samba's option syntax bit here: the first `;` separates interface from options and options are comma-separated, so `"lo;speed=40000000000,capability=RSS"`, not `;capability=RSS`. The address the Mac connects to lives on `lo`, which is why the physical NIC's auto-detected RSS flag didn't apply.

### What to watch

- **zfs_dirty_data_max** — **signal** `zpool iostat 1` bursting to full pool speed while the client's per-second rate drops to ~0 in step. Raise until the client trace is flat; 16 GiB was enough at 2 GB/s. It is bounded by `zfs_dirty_data_max_max`, which needs a reboot to change.

- **net.smb.fs.tcpsndbuf / tcprcvbuf** — **signal** per-connection throughput plateauing with idle CPU on both ends. Takes effect on the next session (remount). Made no visible difference by itself here; kept because it costs nothing.

- **concurrency.nas (mm) / smbd thread** — **signal** `top -H` on the smbd serving the session. One thread handles all channels of one session; at ~80% of a core, more parallel files stop helping and the next multiplier is a second session (second smbd). `aio write size` keeps disk writes off that thread.

## Read path

### What was wrong

Cold reads ran at a quarter of what the pool delivers a local reader (761 MB/s–1.2 GB/s), while warm reads over the same session ran at 1.4 GB/s. Two facts explained it:

- The macOS SMB client keeps only ~4 READ requests of 256 KiB–1 MiB in flight per file (~1.2–1.6 MiB), fixed, and they arrive at the server out of order. Per-stream throughput is that window divided by the server's turnaround, so turnaround is everything: ~1 ms warm, 10–20 ms when the disks are involved.
- OpenZFS 2.2.2's prefetcher keys a stream on the next expected block and could not follow the reordered requests; `arcstat` showed prefetch supplying ~5% of reads. Every block was a demand read at disk latency.

### What changed

1. Moved to the HWE kernel's in-tree OpenZFS 2.4.1, whose prefetcher tolerates reordering (`zfetch_max_reorder`, 16 MiB default). The blocker was `zfs-dkms` 2.2.2 shadowing the kernel's own module and failing to build against 7.0; purging it and regenerating both initramfs images (root and /boot are on ZFS) fixed the boot chain.
2. Started the prefetch stream far ahead and let it queue deeply:

```
# /etc/modprobe.d/zfs.conf (fs2), same line as above
zfetch_min_distance=268435456 zfetch_max_distance=1073741824 zfs_vdev_async_read_min_active=8 zfs_vdev_async_read_max_active=32
```

Each step was measured by server-side READ turnaround, which is the number the client's window multiplies: 4.7 ms median before the distance change, 2.7 ms after, with throughput following (339 → 444 MB/s). The remaining gap to the ~1 ms warm floor is prefetch that is issued but not yet complete when the request lands.

### What to watch

- **arcstat -f time,read,dread,pread,ph%,pm%,dm% 1** — **signal** `pread` relative to `dread` during a cold read, and `dm%`. Prefetch is working when `pread` is in the thousands per second and `dm%` is ~1%; it was 40/s and 15–40% on 2.2.2. This is the first thing to look at if cold reads regress.

- **zfetch_min_distance / zfetch_max_distance** — **signal** server READ turnaround median (capture below). Raise the minimum until the median stops falling; 64 → 256 MiB took it from 4.7 to 2.7 ms. The ceiling is the pool's own single-stream rate (local `dd` on fs2, ~1.1–1.2 GB/s here: 56 drives at ~20 MB/s each, IOPS-shaped because each 1 MiB record fans out to six disks).

- **zfs_vdev_async_read_max_active** — **signal** `pread` flat despite a larger distance. Prefetch runs through the async read queue, 3 per vdev by default (21 pool-wide); it must be deep enough to carry the distance. No effect on its own, necessary once the distance grew.

- **Server READ turnaround** — **how** on fs2, during a read: `timeout 30 tcpdump -i any -nn -s 128 -w /tmp/smb.pcap 'port 445'`, then join requests to responses by message ID (`tshark -Y 'smb2.cmd == 8' -T fields -e smb2.flags.response -e smb2.msg_id -e frame.time_epoch` piped through awk). With ~4 requests in flight, each millisecond of median is worth ~150 MB/s; the p90/max tail is what drops frames.

- **The 15-second bursts** — **open** `arcstat` shows 300–450K reads/s every ~15 s from something on fs2, and READ max latency of ~380 ms lines up with them. Nine frames at 24p. `iotop -o` during a burst names the process; reducing it is the likely fix for the remaining playback hitches.

## What did not help

- `aio read size = 0`: forces in-order reads from smbd's main thread. On 2.2.2 the prefetcher liked the order but the serialised disk waits made it slower (100–180 MB/s). Reverted to 1.
- `vfs_readahead`: never issued a hint on this build (no `readahead`/`fadvise64` calls in `strace`). Removed.
- Jumbo frames: the Mac's ConnectX-5 driver reports `max mtu: 2034`. Not available.
- Bigger distance or queues alone on 2.2.2: no effect until the reorder problem was fixed by the ZFS upgrade.

> **Rule of thumb from all of it:** when neither end is saturated (disks, CPU, wire) and throughput is still flat, measure turnaround per request and multiply by what the client keeps in flight. Every result today fell out of that one equation.

## Where the tools stand

- Local reader on fs2: `dd if=<file> of=/dev/null bs=4M skip=<fresh region> count=1024`: the pool's own number, and the ceiling for anything over SMB.
- Mac cold read: same `dd` against `/Volumes/photography/…`; second run of the same region measures the Mac's page cache, not the NAS.
- `smbutil multichannel -a` and `smbutil statshares -a`: channels, RSS flags, signing/encryption state; session properties are fixed at mount time, so remount after server-side changes.
- For editing rather than browsing, the spool tier remains the answer: `mm spool <day> --pin` reads at 3 GB/s from m2.
