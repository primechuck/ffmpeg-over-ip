# Wrapper options for transparent remote file handles (CPU on different host than IO)

Goal: Jellyfin orchestrator has files, N100 little nodes have GPU. Want ffmpeg on N100 to read files that live on orchestrator, without copying entire file first, and without patching ffmpeg source if possible. File ops should be transparent.

## Why patch existed

Original `ffmpeg-over-ip` patches `libavformat/file.c` to call `fio_open/read/write/...` instead of `open/read/write`. `fio.c` tunnels those calls over loopback TCP to Go server, which tunnels over TCP to client. Works, but needs custom `jellyfin-ffmpeg` build.

## Options without patching ffmpeg

### 1. LD_PRELOAD shim (implemented: `fio/fio_preload.c`)

Intercepts libc file ops via `dlsym(RTLD_NEXT, "open")` and routes through same `fio_*` logic.

```bash
gcc -fPIC -shared -o libfio_preload.so fio/fio.c fio/fio_preload.c -Ifio -ldl -lpthread -O2

# Passthrough when no env
LD_PRELOAD=./libfio_preload.so cat /tmp/file  # works

# Tunneled: server sets FFOIP_PORT and spawns child with LD_PRELOAD
FFOIP_PORT=5000 LD_PRELOAD=./libfio_preload.so ffmpeg -i /media/a.mkv ...
```

Pros:
- Works with **stock** `jellyfin-ffmpeg` binary, no patch, no rebuild.
- Same short-circuit logic (RO verified, RW shared) can live in `fio.c`, no duplicate.
- Transparent: any binary (not just ffmpeg) gets remote file handles if it does file I/O.

Cons:
- Still C code, needs `LD_PRELOAD` env propagation (server already sets `FFOIP_PORT`, now also `LD_PRELOAD`).
- `openat`, `stat`, `access` etc need careful handling (we keep `access`/`stat` real to avoid breaking `file_check`).
- Thread safety: `dlsym` + `fio_*` uses mutexes, okay.
- Doesn't work on static binaries or setuid.

This is the most boring wrapper that achieves CPU ≠ IO without hacking ffmpeg.

### 2. FUSE filesystem

Mount a directory like `/mnt/remote-media` as FUSE FS on N100. FUSE daemon forwards file ops over TCP to Jellyfin host (like sshfs but custom protocol). Then ffmpeg opens `/mnt/remote-media/movie.mkv` normally.

Pros: No ffmpeg patch, no LD_PRELOAD, works for any app, kernel handles caching.
Cons: Extra daemon, FUSE overhead (~10% slower), need to manage mountpoints, seek still needs round trips.

Example: `sshfs jellyfin:/media /mnt/remote-media` — simplest FUSE, but uses SFTP, not our protocol.

### 3. NFS / Ceph / SeaweedFS (fixed mounts)

Mount NAS at same path on all hosts (`/media` on Jellyfin + all N100s). Then remote exec is just `ssh n100 ffmpeg -i /media/a.mkv ...` or our Go daemon exec without any file tunneling at all. No C code.

Pros: Kernel NFS client is fast, no patch, no LD_PRELOAD, no tunnel. If you already have NAS, this is most boring.
Cons: Requires shared storage for **both** input and output (for HLS, `/cache/transcodes` must also be shared). If output is local to Jellyfin host, won't work.

This is what our short-circuit optimizes: when shared FS *is* present, skip leg.

### 4. Named pipes (FIFO) + streaming cat

Wrapper creates FIFO per input file: `mkfifo /tmp/fifo_input`, starts background `cat /real/file |` or `rclone cat` feeding FIFO, ffmpeg reads FIFO. For output, creates FIFO and streams out.

Pros: No patch, pure shell.
Cons: FIFO not seekable — ffmpeg needs seekable for mkv/mp4 index. Fails for most media. Could download entire file to temp file first (not streaming) — slow for large files.

### 5. HTTP range + custom AVIO (no patch, but wrapper script)

Jellyfin host runs tiny HTTP server serving files with Range support. Wrapper rewrites ffmpeg args: `/media/a.mkv` → `http://jellyfin:8080/media/a.mkv`. FFmpeg's `http` protocol does range requests for seeking.

Pros: No C, works with stock ffmpeg, seek via HTTP Range (extra RTT per seek but okay on 10G).
Cons: Need to translate all file paths to URLs, handle output via HTTP PUT or still need shared output dir.

### 6. eBPF / ptrace

Use eBPF to intercept `open` syscalls and redirect, or `ptrace` like `strace` to capture file ops and proxy. Very heavy, not boring.

### Recommendation for your 4× N100s

- If you have NAS at same path everywhere: **NFS + simple remote exec** (Go daemon that just `exec.Command` ffmpeg, no fio). That's boring, no patch, and with our `maxConcurrent=1` + hedged failover you get fleet LB. This is effectively `rffmpeg` but Go + WireGuard instead of Python + SSH.

- If you have mixed (some files local only to Jellyfin, like fonts, attachments): use **LD_PRELOAD shim** (`libfio_preload.so`) with stock ffmpeg. Transparent, no patch, keeps tunneling fallback. Build once, `LD_PRELOAD` in server's child env.

- If you want kernel-level and any app: **FUSE sshfs/rclone mount**.

Current `fio.c` patched approach and LD_PRELOAD approach share 95% code — `fio.c` is the core. Patch is slightly faster (no `dlsym` indirection, direct `fio_*` calls in `file.c`), LD_PRELOAD is more compatible.

Both achieve CPU on different host than IO.
