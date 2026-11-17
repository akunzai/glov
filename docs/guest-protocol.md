# Guest Service protocol

The contract between `glovd` (host) and `glov-guest` (guest). Both sides implement it; change it here first.

## Transport

- Windows: a virtio-serial port named `org.glov.guest.0`. QEMU exposes it on the host as a unix socket
  `<vm dir>/guest.sock`:

  ```text
  -device virtio-serial-pci,id=vser0
  -chardev socket,id=guest0,path=<vm dir>/guest.sock,server=on,wait=off
  -device virtserialport,bus=vser0.0,chardev=guest0,name=org.glov.guest.0
  ```

  The Guest Service opens `\\.\Global\org.glov.guest.0` (vioserial driver from virtio-win).
- Linux: the Guest Service listens on AF_VSOCK, any CID, port 4022; the host
  (`VZVirtioSocketDevice.connect(toPort:)`) connects to the guest. Same framing and messages. There is one host
  connection at a time: when it ends the Guest Service accepts the next, and a new connection replaces the current one (the
  old one is closed), because after a snapshot restore the guest never sees the old connection end. Each new
  connection starts with `guest.ready`. Any guest process can connect to the listener (for example over the loopback
  CID), so the Guest Service accepts only peers with CID 2 (`VMADDR_CID_HOST`) and closes the rest, logging each rejection. If vsock is not available yet at boot, the Guest Service retries every 2 seconds.
- Linux guest on a Windows host (Hyper-V, `windows/`): the same Guest Service and port. The host dials `hv_sock` with
  the VM's HCS `RuntimeId` as the VM ID and service ID `00000fb6-facb-11e6-bd58-64006a7986d3` (port 4022 in the
  `XXXXXXXX-FACB-11E6-BD58-64006A7986D3` vsock template); the guest sees the host as CID 2, so the check above holds.
  glovd uses the VM's own ID as its `RuntimeId`. A connect sent before the guest listens hangs until Windows' 30 s
  hv_sock connect timeout, so the host dials with a 1 s timeout and retries; the guest listens about 2 s after boot.
- The stream survives neither side's restart cleanly: virtio-serial gives the guest no EOF when the host side closes.
  So the Guest Service never treats a read error or silence as fatal; it keeps the port open (reopening on error) and serves
  whatever arrives. The host treats a reconnect as a new session and calls `guest.info` first, and notices silence with
  a heartbeat (see Heartbeat).
- After the guest is restored from a live snapshot (qemu is restarted into it), the host reconnects, marks every running job
  lost and calls `time.sync` once the Guest Service answers, because the restored guest clock is from the past.

## Framing

Each message is a 4-byte big-endian unsigned length followed by that many bytes of UTF-8 JSON (a JSON-RPC 2.0 request,
response or notification). A frame larger than 1 MiB is a protocol error: the receiver drops the connection
(host) or discards bytes up to the next readable frame boundary it can resynchronise on (agent: it reopens the port).

The Guest Service logs a reply it fails to send (`reply not sent`, with method, frame size and error, and whether the
frame was cut short, which leaves the host out of step). The host logs a frame that is not JSON and an
answer to a request that no longer waits for it (timed out or cancelled; heartbeats excepted), along with each session
change.

## Roles

- The host sends requests; the Guest Service answers each with a response carrying the same `id`.
- The Guest Service sends notifications (no `id`): `guest.ready`, `exec.output`, `exec.exit`.
- Binary data is base64 in a `data` field. Times are RFC 3339 UTC strings.

## Methods

| Method | Params | Result |
|---|---|---|
| `guest.info` | – | `{protocolVersion: 1, version, os: "windows"\|"linux", arch, hostname, user: <signed-in console user's bare account name, or null>, bootId}` |
| `guest.stats` | – | `{memoryTotal, memoryAvailable, diskPath, diskTotal, diskUsed, diskAvailable, load: [1, 5, 15 min] \| null, cpuBusy}` |
| `exec.start` | `{argv: [string], cwd?, env?: {K: V}, as?: "system"\|"user", stdin?: bool}` | `{pid}` |
| `exec.stdin` | `{pid, data, eof?: bool}` | `{}` |
| `exec.kill` | `{pid}` | `{}` (the `exec.exit` notification still follows) |
| `file.stat` | `{path}` | `{exists, isDir, size, mtime}`; a missing path is `{exists: false, isDir: false, size: 0, mtime: null}`, not an error |
| `file.list` | `{path}` | `{entries: [{name, isDir, size, mtime}]}` |
| `file.read` | `{path, offset, length}` (length ≤ 524288) | `{data, eof}` |
| `file.write` | `{path, offset, data, truncate?: bool, mkdirs?: bool}` | `{size}` (file size after the write) |
| `file.remove` | `{path, recursive?: bool}` | `{}` |
| `power.reboot` (Windows only) | – | `{}` (sent before the guest goes down) |
| `time.sync` | `{unixMillis}` | `{}` |
| `secrets.sync` (Linux only) | `{owner?, secrets: [{name, data}]}` | `{names: [string]}` (the files there now, sorted) |
| `sync.manifest` | `{id, root, subdir?, exclude?: [{pattern, negate?, dirOnly?}], verify?: bool, offset?, paths?: [string], owner?}` | `{entries: [{path, dir?, size, mtime?, hash, modified?, error?}], total, hashed, fresh, missing?: [string], owner?}` |
| `sync.apply` | `{id, root, ops: [op], commit?: bool, owner?}` | `{failures: [{path, message, locked, denied, conflict?}], owner?}` |
| `sync.watch` (Linux only) | `{id, root, exclude?}` | `{}` (changes after it arrive as `sync.changed`) |
| `sync.unwatch` (Linux only) | `{id}` | `{}` |
| `sync.read` (Linux only) | `{id, root, path, offset}` | `{data, size, key, eof?}` (data ≤ 524288 bytes) |

- `exec.start` runs `argv` directly, with no shell; callers pass `powershell.exe -NoProfile -Command ...` or
  `/bin/sh -c ...` themselves. `as` defaults to `system` (the Guest Service's own account). `as: "user"` runs in the signed-in
  console user's session (Windows: `WTSQueryUserToken` + `CreateProcessAsUser`, with that user's environment block;
  Linux: see below); with nobody signed in it fails with error 4. Without `stdin: true` the child's stdin is closed at start.
  Output flows only once the reply carrying the pid is written; if that write fails (the host went away), the Guest
  Service kills the process tree and reaps it, with no `exec.output` or `exec.exit` for a pid the host never saw.
- `file.write` with `truncate: true` is only valid at `offset: 0` and creates or truncates the file; `mkdirs: true`
  creates missing parent directories.
- `guest.stats` measures the guest as it sees itself (`glov vm stats`); sizes are bytes. Memory: Linux `MemTotal` and
  `MemAvailable` of `/proc/meminfo` (what `free` shows), Windows `GlobalMemoryStatusEx`. Disk: the system volume
  (`/`, Windows `%SystemDrive%\`), as `df` counts it: `diskUsed` is total minus free blocks, `diskAvailable` what an
  unprivileged process may still write (Windows: `GetDiskFreeSpaceEx`). `load` is `/proc/loadavg` on Linux and null on
  Windows, which has none. `cpuBusy` is the percentage of all guest CPUs busy over a 0.5 s sample (`/proc/stat`,
  `GetSystemTimes`), so the call takes that long; it is served beside the request loop. An older Guest Service answers
  -32601: the host shows the guest's numbers as unavailable and asks for a Guest Service update.
- `bootId` is an opaque string that changes on every guest boot (Windows: the System process's creation time, which
  `time.sync` does not shift; Linux: `/proc/sys/kernel/random/boot_id`), so the host can tell a reboot from a reconnect.
- Linux `as: "user"`: the signed-in user is read from systemd-logind's session records (`/run/systemd/sessions`).
  Only sessions of class `user`, uid >= 1000 and not closing count. Preference: a graphical session (x11, wayland, mir),
  then a local tty session, then a remote one (ssh); an `active` session beats an `online` one; remaining ties go to
  the oldest session. `guest.info.user` uses the same choice. The child runs with that uid, gid and supplementary
  groups and a fresh environment (not the Guest Service's): `HOME`, `USER`, `LOGNAME`, `SHELL`, `PATH`, `LANG` (the Guest Service's, if
  set), `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` when `/run/user/<uid>` and its `bus` exist, and for a graphical
  session `DISPLAY` (from logind) and `WAYLAND_DISPLAY` (the compositor socket in the runtime directory), then the
  `KEY=VALUE` lines of `/etc/environment` (quotes around a value dropped) for names not set already. `env` from the
  request overlays it; `cwd` defaults to the user's home directory. `as: "system"` runs as root with the Guest Service's own
  environment.
- A process ended by `exec.kill` exits with code 1 on Windows. On Linux each child leads its own process group and
  `exec.kill` sends SIGKILL to the group; a process ended by a signal reports `128 + signal` (137 for `exec.kill`). `exec.exit` waits until both output streams reach EOF,
  so a grandchild that keeps stdout open delays it until the grandchild exits or `exec.kill` ends the tree.
- `power.reboot` restarts Windows (`ExitWindowsEx` with `EWX_REBOOT | EWX_FORCEIFHUNG`) after the reply: a restart, so
  never a Fast Startup shutdown, and the QEMU process stays the same. The Linux Guest Service answers -32601: the host
  reboots a Linux guest by stopping it with ACPI and starting it again (`glov vm reboot`), which works whether or not its
  Guest Service answers. (A Linux guest that reboots itself restarts inside the same VZ process.) There is no `power.shutdown`: the host stops guests with ACPI.
- `time.sync` sets the system clock to `unixMillis` (Windows: `SetSystemTime`; Linux: `clock_settime(CLOCK_REALTIME)`).
  It does not change the time zone or the RTC. A time-sync daemon may step the clock again afterwards.
- Under Hyper-V (a Windows host) the Linux Guest Service also keeps the clock on the host's by itself: every 2 s it
  reads the `hyperv` PTP clock (`/dev/ptp_hyperv`, hv_utils' TimeSync, which follows the host's samples) and steps the
  system clock when the two differ by more than 1 s, logging `stepped the clock to the host's`. A guest's clock stands
  still while the host is in Modern Standby, and Hyper-V sends a sync sample only when the host's own time changes, so
  without this a VM with no NTP source (an isolated one) stays behind after the host wakes. Without the device (QEMU,
  Virtualization.framework) it does nothing.
- A response that would exceed 1 MiB (for example a huge `file.list`) is returned as error 1.

### Secrets in tmpfs

`secrets.sync` delivers the secrets a harness must hold in plain text (a harness profile's `tmpfs` secrets,
[networking](networking.md#harness-profiles)). The host sends the whole list each time; the Guest Service makes
`/run/glov/secrets` hold exactly that list:

- The directory is a tmpfs of its own, mounted on first use `nosuid,nodev,noexec`, `mode=0711`, `size=4m` and
  `noswap` (kernels before 6.4 lack `noswap` and mount without it). The call fails rather than write to a directory
  that is not a tmpfs. Mode 0711 lets the owner open its files by name; nobody but root lists them.
- Each secret is the file `<name>` holding `data` (base64) and nothing else, mode 0400, owned by `owner` (uid and
  primary gid; required when `secrets` is not empty). It is written to `<name>~new` and renamed over the old one, so a
  reader sees the old value or the new one. Every other file is removed, so an empty list removes them all.
- `name` is 1–64 of `A-Za-z0-9._-`, not `.` or `..`, as for the host's secret names; a bad name, a duplicate or an
  unknown owner is error -32602 and changes nothing.
- The host calls it when a session opens (tmpfs does not outlive a boot, and a reverted guest gets what is granted
  now), and after `glov secret grant`, `revoke`, `set`, `source`, `refresh` (Windows) or `remove` touches a running VM's secrets. It logs
  the names, never `data`; the Guest Service logs only the method. Windows guests and older Guest Services answer
  -32601.
- The host refuses a live snapshot of a VM given a tmpfs secret since it booted, because the snapshot saves guest
  memory: the file, or pages a removed one left behind.

### Share sync

`sync.manifest` and `sync.apply` mirror a host folder into the guest directory `root` (`glov share sync`). The host
decides everything; the Guest Service lists, hashes and applies. `id` names the share (`A-Za-z0-9._-`, not starting with `.`).
Both methods are served beside the request loop, so heartbeats are answered while they run; the host sends one at a
time per share.
On a Windows host (`windows/`), `glov` scans and reads the host folder as the user and `glovd` passes the calls on,
setting `id` to the share name and `root` to its guest path, `/mnt/<drive>/<path>`; `glovd` never reads the folder.

- **Root**: `root` may not be, or lie below, a symbolic link or junction. Both methods open it one component at a time
  from the volume root, each relative to its parent's handle, refuse a link, and check that the handle opened is the
  directory inspected (Windows: its final path; Unix: same device and inode); `sync.apply` creates missing components
  this way. They do so on every call, so a root replaced by a junction between calls is caught, and the call fails
  with "... is or lies below a symbolic link or junction; refusing to sync through it". The Guest Service runs as LocalSystem:
  a guest process allowed to change a parent folder could otherwise point the sync at any folder.
- **Paths** in params and results are relative to `root`, separated by `/`, without `.` or `..` segments, `:` or `\`.
  The empty path is `root` itself.
- **Guest manifest**: for each file the Guest Service records its stat key and SHA-256 in
  `C:\ProgramData\glov\sync\<id>.json` (Linux: `/var/lib/glov/sync/<id>.json`), never inside `root`. The stat
  key is size, last write time, change time and file id (NTFS `FileId`; Linux: size, mtime, ctime and inode in
  nanoseconds). A manifest recorded for another `root` is discarded.
  The stat key is a shortcut, not a guarantee: a guest process may edit a file in place and then restore its
  LastWriteTime and ChangeTime (`SetFileInformationByHandle` with `FileBasicInfo`), and the size and file id stay the
  same, so the recorded hash is reused and the edit goes unseen. `verify: true` hashes every file and is the guarantee.
- `sync.manifest` walks `root/subdir` (default: all of `root`) and returns its files and directories, with paths
  relative to `root`. A missing `root` or `subdir` is an empty listing. `exclude` rules are RE2 regular expressions
  matched against the whole relative path; for each entry the last matching rule wins (`negate` re-includes, `dirOnly`
  rules match directories only). An excluded entry is neither returned nor descended into, and nor are symbolic links,
  junctions and other reparse points: the Guest Service never touches them. The host compiles the share's gitignore patterns
  into these rules.
  - `hash` is hex SHA-256: the recorded one when the file's stat key is unchanged, else (and for every file with
    `verify: true`) the Guest Service hashes the file. `hashed` counts the files it hashed. A directory has `dir: true` and no
    hash; a file it could not read has an empty `hash` and `error`. A file's `mtime` is its last write time in Unix nanoseconds.
  - `modified: true` marks a file whose content differs from what the manifest recorded, or that has no record although
    the manifest exists: a change made in the guest since the last sync. `fresh: true` means there was no manifest.
  - Entries come in pages of about 128 KiB: offset 0 walks (and hashes); a call with `offset` = the entries received
    so far returns the next page of that walk, until `total` entries have been received.
  - `paths` (at most 512) lists just those entries in place of a walk, none descended into, and updates their records:
    what a live share's host asks after `sync.changed`. `missing` names the ones a walk would not reach: absent, a
    link, or below a missing or linked directory. A path in neither `entries` nor `missing` is excluded, by a rule on
    it or on a directory above it. The answer is a single page.
- `sync.apply` applies `ops` in order and reports the ones that failed, without stopping at the first:
  - `{op: "delete", path}` removes a file (a missing file is no failure).
  - `{op: "rmdir", path}` removes a directory if it is empty; a non-empty one (for example holding excluded files) is
    kept, which is no failure.
  - `{op: "mkdir", path}` creates a directory and its parents.
  - `{op: "write", path, offset, data, end?: bool, hash?}` streams a file: `offset: 0` starts a temporary file next to
    the target (creating parent directories), later ops append at their `offset`, and the op with `end: true` carries
    the host's `hash`. The Guest Service compares it with the SHA-256 of the bytes it received (a mismatch fails: the host file
    changed while it was sent), then renames the temporary file over the target and records the file's new stat key
    and hash. On Linux the temporary file takes the permission bits of the file it replaces (NTFS has none to send),
    so a script stays executable; a new file is `0644`. A file's ops may span several `sync.apply` calls; one call carries at most 1 MiB, so about 512 KiB of data.
  - `expect` on a `delete`, or on the `write` op with `end: true`, is what the operation replaces: the SHA-256 the
    guest file must have, or `""` for no file. Anything else (another hash, a directory, a link) fails with
    `conflict: true` and changes nothing, so a live share never overwrites a guest change its host has not seen. The
    check and the change are not atomic: a guest edit in between is still replaced.
  - A failure whose OS error says the file is in use (Windows: sharing violation, lock violation) has `locked: true`;
    one whose OS error is access denied has `denied: true`. Windows also reports replacing or deleting a file that
    another process holds without `FILE_SHARE_DELETE` as access denied, as it does a read-only file, so such a
    failure of a delete or of the rename over the target has both. Nothing checks which processes run in the guest.
  - Every change, and every read while hashing, goes through a handle on `root` (Go's `os.Root`), which refuses at
    each operation any path that leaves `root` through a symbolic link or junction: a guest process that plants a link,
    or swaps a directory for one after the listing, cannot redirect the Guest Service's writes, renames or deletes. The
    operation fails instead.
  - `owner` (Linux guests only) names a guest user: the Guest Service resolves it to its uid and primary gid, and
    makes it the owner of `root` and of every directory it creates (`mkdir`, a write's parents) and every file it
    writes (the temporary file, before the rename and before the stat key is recorded), with `lchown` and `fchown`
    through the `os.Root`, so a planted link is never followed. Files and directories it does not create or write
    keep their owner. An unknown user fails the call with -32602, as does any `owner` on a Windows guest. The answer
    echoes `owner` once applied. `sync.manifest` takes the same `owner`, checks it the same way and echoes it without
    changing anything, so the host learns before the first write whether the Guest Service honours it. `glovd`
    sets `owner` to `glov` on both methods for VMs created with `--harness`, and fails the sync when an older
    Guest Service answers without it.
  - `commit: true` (after its ops) saves the guest manifest and removes temporary files of writes that never ended.
    Without a commit the manifest on disk stays as it was, which only costs rehashing on the next sync.
- `sync.status {id}` reports the share's running call: `{busy, phase: "manifest"|"apply", path, files, bytes, seconds}`
  (`busy: false` and zeros when idle). `path` is the file being hashed or written, `files` and `bytes` count what this
  call hashed or wrote so far. While the first `sync.manifest` page is pending the host polls it every 10 s and, when
  `phase`, `path`, `files` and `bytes` have not changed for 2 minutes, stops the sync with an error naming `path`.
  Three idle answers in a row while the page is still pending mean the request or its answer was lost on the way:
  the host stops the sync saying so at once.
  `glov share status` (`share.status`) shows it too.
- `sync.cancel {id}` asks the share's running call to stop before its next file (a listing) or operation (a batch);
  that call then fails. The host sends it when it stops a stalled listing, when its client disconnects
  (`glov share sync` killed) and on `glov vm guest update --force`, and waits for the answer before the share can be
  synced again. The next `sync.manifest` with offset 0 clears it. A read blocked in the OS is not interrupted.
- Both are served inline, so a cancel reaches the share before any later call. A call that finds the share's previous
  call still running after 10 s (one the host gave up on, stuck in the guest) fails saying so and naming its file,
  instead of waiting.
- The Guest Service logs each listing (entries, hashed, bytes, seconds), each commit, each cancel, and a file whose hashing
  took 30 s or more.
- An older Guest Service answers `sync.manifest` and `sync.apply` with -32601; the host then asks for a Guest Service update. One that
  answers `sync.status` and `sync.cancel` with -32601 gets neither stall detection nor guest-side cancellation: the
  host still frees the share when it stops waiting.

### Live shares

A live share (Windows host, Linux guests only; others answer -32601) keeps a host folder and its guest directory in
step both ways. The host does the deciding, as for `sync.manifest` and `sync.apply`; the Guest Service watches the
guest side and reads files from it. `id`, `root` and `exclude` mean what they do there.

- `sync.watch {id, root, exclude}` creates `root` if missing (refusing one that is or lies below a link, as
  `sync.apply` does), puts an inotify watch on it and on each directory below it that is not excluded and not
  reached through a link, and answers once they are in place: every guest change after the answer is reported. A
  second `sync.watch` of the same `id` replaces the first. Watches end with the Guest Service: after `guest.ready`
  the host watches again and lists the whole root.
- `sync.changed {id, paths?, overflow?, error?}` (a notification) reports paths below `root` that changed, sorted,
  about 150 ms after the last change and at least every second while changes keep coming. Directories are always
  reported (a moved or deleted directory takes its files along unreported, so the host lists below it); a file is
  left out when its stat key and existence match the share's records, which is what the Guest Service's own
  `sync.apply` leaves behind. `sync.apply`'s temporary files, excluded paths, names the host cannot hold (`:` or `\`)
  and anything below a link are never reported. A directory made or moved in is watched and its contents reported.
  `overflow: true` means changes were not all seen (more than 1,000 at once, the kernel's queue overflowed, or the
  watch limit `fs.inotify.max_user_watches` was reached): the host lists the whole root. `error` says why the watch
  is incomplete or ended; when `root` itself is deleted or moved, the watch ends with an overflow and an error.
- `sync.unwatch {id}` stops it; a share not watched is no failure.
- `sync.read {id, root, path, offset}` returns `{data, size, key, eof?}`: at most 512 KiB of a regular file below
  `root`, reached as a walk reaches it (never through a link; a FIFO or device is refused without blocking). `key` is
  the file's stat key when the chunk was read; the host reads the file again from offset 0 when it changes between
  chunks.

## Notifications

| Notification | Params |
|---|---|
| `guest.ready` | same object as the `guest.info` result; sent when the Guest Service starts and each time it reopens the port |
| `exec.output` | `{pid, stream: "stdout"\|"stderr", data}`; each chunk ≤ 65536 raw bytes, in order per stream |
| `exec.exit` | `{pid, code}`; always the last notification for that `pid`, after all of its output |
| `sync.changed` | `{id, paths?, overflow?, error?}`; a live share's guest changes, see "Live shares" |

## Heartbeat

virtio-serial gives the host no EOF when the guest reboots, hangs or loses its Guest Service, and a vsock connection stays up
while the Guest Service hangs or the user changes. So once a session is established the host sends `guest.info` every 5 s, on a
fixed cadence, and waits 3 s for each answer. Any frame received counts as an answer (a busy Guest Service's `exec.output`
does too); two heartbeats in a row with no frame at all make the session `unresponsive`. The wire protocol is unchanged:
the Guest Service only has to answer `guest.info` while other requests run, which it does since it serves requests concurrently.

- Bounds: a hang, reboot or uninstall shows as `unresponsive` within 13 s (2 × 5 s + 3 s) of the last frame; a VZ
  reboot shows sooner, as `connecting`, because the vsock connection ends. A change of `user` shows within about 5 s.
- While `unresponsive`, `vm_status` reports `state: "unresponsive"` with the last `os` and `version`, `user` and
  `bootId` null (they may no longer be true), and `problem` saying how long the Guest Service has been silent. New requests fail
  at once with "Guest Service not responding"; requests already sent keep waiting for their answer or their own timeout,
  and jobs are not marked lost.
- Recovery: the next `guest.info` answer with the same `bootId` continues the session (jobs keep reporting).
  A different `bootId`, from a heartbeat or after `guest.ready`, starts a new session: running jobs are marked lost.

## Errors

JSON-RPC error objects with these codes:

| Code | Meaning |
|---|---|
| -32700, -32600, -32601, -32602 | parse error, invalid request, method not found, invalid params (JSON-RPC 2.0) |
| 1 | operating-system error; `message` carries the OS text |
| 2 | path or process not found |
| 3 | permission denied |
| 4 | no signed-in user session (`as: "user"`) |

## Service

Windows: the `GlovGuestService` service, installed by `glov-guest install`. Linux: `glov-guest install` (as root)
copies the binary to `/usr/local/bin/glov-guest` (the directory and file must be root-owned and not writable by
others, since the unit runs it as root), writes `/etc/systemd/system/glov-guest.service`
(`Restart=always`, `RestartSec=2`, no start limit, `WantedBy=multi-user.target`, and `TimeoutStopFailureMode=abort`
after 10 s, so a hung Guest Service leaves its goroutine dump in the journal), then runs `daemon-reload` and
`enable --now`; on a running service it also restarts it. `uninstall` disables and stops the unit and removes the unit
and the binary. The Linux Guest Service logs to stderr, which journald collects.

`glov vm guest check` compares the installed executable with the host distribution by SHA-256 without copying or installing.
The host-only `vm_status.guestService.connectionID` changes each time a Guest Service session is established, so an update can confirm
a same-version restart even if the ready notification and reconnect happen between status polls. It is distinct from `bootId`.
The host also checks once per Guest Service connection and exposes the result in `vm_status.guestService.update`; it never installs automatically.

`glov vm guest update` (docs/agents/verification.md) replaces the Guest Service on a running VM. It confirms the update by `guest.info`'s
`version` after the restart (the `-X main.version` of the build, `dev` by default) and by the SHA-256 of the installed file.

On a Windows host (`windows/`), `guest.check`, `guest.update` (`{vm, force, timeout}`) and `vm.status`'s `guestService.update`
and `connectionID` work the same way against the `glov-guest-linux-amd64` next to `glovd.exe`: glovd hashes
`/usr/local/bin/glov-guest` with `sha256sum` through `exec.start`, stages the binary with `file.write` in
`/run/glov-guest-update`, checks it and starts its `install` detached: on Debian and Ubuntu a `systemd-run` unit copies
it to `/usr/local/bin/glov-guest.next` first (Debian mounts `/run` noexec), runs that copy's `install` and removes it. It waits for a new connection, checks the installed
file again and removes the staging directory. An update is refused while exec jobs or a share sync run; `--force` goes ahead, and the jobs it cut off read as lost. `vm.stats` measures
the host with HCS's processor runtime, the `vmmem` process of a VM whose memory is taken on demand (matched by the VM's
account SID) and the VHDX file, and calls `guest.stats` for the guest's side.

## Versioning

`protocolVersion` is an integer, currently 1. A change that an older peer would misread bumps it. The host refuses
Guest Services with a higher version than it knows and asks for an update when the Guest Service is older.

## SSH over vsock

Every Linux Guest Service listens on AF_VSOCK port 4025 (host CID only, retried until vsock is up) and pipes each
connection, byte for byte, to TCP `127.0.0.1:22`, at most 64 at once. It parses nothing and dials nothing else; a
connection finds no sshd closed at once. The host connects in; nothing is registered for the guest to connect out.

On the host, `glov ssh-relay <vm>` (the `ProxyCommand` of `glov ssh`) opens the daemon socket and, instead of a
JSON-RPC request, sends `glov-ssh <vm>` and a newline (at most 512 bytes; the daemon tells the two apart by
that prefix). The daemon answers `ok` and a newline and pipes the connection to port 4025 of that
running VZ VM, or answers `error: <reason>` and a newline and closes: not a VZ VM, not running, or nothing listening
(a Guest Service from before the relay). After `ok`, the bytes are ssh's own.

On a Windows host the same header goes over glovd's control pipe, whose JSON-RPC frames start with a zero byte, and
glovd dials hv_sock service `VsockServiceID(4025)` of the caller's running VM. ssh's key and `known_hosts` live in
the user's `%LOCALAPPDATA%\glov\vms\<vm>\`, since glovd's data directory belongs to SYSTEM; `glov ssh` adds
the `glov` user and the key in the guest through `exec` each time it runs.

## Isolated VM egress proxy

An isolated VM (`isolated: true` in vm.json; `glov vm create --engine vz --isolated` on a Mac, `glov vm create
--isolated` on Windows) has no network device. Its only way out is an HTTP proxy, and the boundary is on the host: the
guest, root included, is assumed hostile.

- **Direction**: the guest initiates. `glovd` registers a `VZVirtioSocketDevice` listener for vsock port 4024
  only, and only on isolated VMs; VZ refuses guest connections to every other host port. Ports 4022 and 4025 stay
  host-initiated, as above. Port 4024 speaks nothing but HTTP proxy requests: no JSON-RPC, no `glovd` control or
  MCP method. On Windows, `glovd` instead binds `winio.ListenHvsock` on the VM's ID and `VsockServiceID(4024)`
  after creating the compute system and before starting it; the guest's `AF_VSOCK` connect to CID 2 reaches it with
  no `GuestCommunicationServices` registration. The Windows proxy (`windows/internal/egress`) mirrors the rules below.
- **Guest side**: first boot (cloud-init) writes the root-owned marker `/etc/glov/egress-proxy` containing
  `glov-egress-v1` and a trailing newline. With the marker (it and its ancestors root-owned, not symlinks and not
  writable by group or others), the
  Guest Service listens on TCP `127.0.0.1:3128` and pipes each accepted connection, byte for byte, to a new vsock
  connection to CID 2 port 4024. It parses nothing, so it decides nothing; any guest process may also connect to port
  4024 directly and meets the same host policy. It also sets `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy`,
  `https_proxy` (`http://127.0.0.1:3128`) and `NO_PROXY`, `no_proxy` (`localhost,127.0.0.1,::1`) in its own
  environment before serving port 4022, so `exec` children inherit them (`as: "user"` copies them, like `LANG`).
  First boot also writes them to `/etc/environment` (PAM logins) and an apt `Acquire::http(s)::Proxy` file. systemd
  services read neither, so they get no proxy unless their unit sets one.
- **Host side**: per connection, the host reads one request head (at most 16 KiB, within 10 s):
  - `CONNECT host:port HTTP/1.x`: if allowed, the host connects and answers `HTTP/1.1 200 Connection Established`,
    then pipes opaque bytes. The exception is a host a secret granted to the VM is bound to: the host then terminates
    TLS with the egress CA and substitutes placeholders (docs/networking.md "Secrets").
  - An absolute-form request (`GET http://host[:port]/path HTTP/1.x`): if allowed, the host sends it upstream in
    origin form, without `Proxy-*` headers and with `Connection: close`, then pipes the rest of the connection.
  - Anything else is answered `400`; a denied target `403`; an allowed target that cannot be reached `502`. The
    connection then closes.
- **Policy**: `egress.allow` in vm.json, re-read for every connection, so a change applies to new connections without
  a restart. An entry is an exact hostname (case-insensitive, trailing dot ignored) or `*.suffix`, which matches any
  name below `suffix` but not `suffix` itself, optionally followed by `:port`. An entry without a port allows ports
  443 and 80 only; `host:port` allows that one port (a bracketed `[v6]:port` for IPv6). An IP literal target is
  allowed only by an identical entry. The host
  checks the name before resolving it (a denied name causes no DNS query), resolves allowed names on the Mac, and
  connects only to a resolved address that is public: loopback, private (RFC 1918, `fc00::/7`), CGNAT
  (`100.64.0.0/10`), link-local, multicast, unspecified and reserved ranges, and every address of the Mac's own
  interfaces, are refused even when allowlisted. It connects to the address it checked, never re-resolving.
- **Logs**: one JSON line per connection in `<vm>/egress.log` (renamed to `egress.log.1` past 1 MiB; `glov egress log`)
  and one daemon-log line, `<vm>: egress <allow|deny|fail> <host>:<port> in=<bytes> out=<bytes>`
  (plus a reason for `deny`, refused by policy, and `fail`, allowed but unreachable), written when the connection
  ends. Never bodies or header values.
- **Limits**: a plain-HTTP request's `Host` header, if any, must name the target (else `400`), so an allowed address
  cannot front for another site; inside a CONNECT tunnel the host sees no names, so TLS SNI and the inner `Host` can
  name any site the allowed server answers for. At most 256 connections per VM are served at once.

## Naming compatibility

The wire has no aliases for earlier names: VMs created under an earlier name must be recreated (see [Naming](naming.md)).
