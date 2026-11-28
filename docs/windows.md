# Windows VMs

## Create a Windows VM

```sh
glov vm create win11    # creates it, starts it and waits until Windows is installed (10 to 15 minutes)
```

`vm create` starts the new VM and waits for its Guest Service by default (up to 60 minutes for an install), printing the
install phase as it goes; `--no-start` only creates it, and the install then runs on the first `vm start`.

`vm create` prepares a VM that installs Windows 11 Arm64 unattended: a 200G thin qcow2 disk, UEFI variables, a TPM, 6
vCPUs and 12G of memory (`--disk-size`, `--cpus`, `--memory`), and the install media for `--language` and `--edition`
(default en-US Professional; also `Core`; other languages from the catalog, e.g. `--language zh-TW`). An en-US guest has only the US keyboard, so keys typed over QMP never go through an IME. The guest's time zone is the host's, mapped to a Windows ID with Unicode CLDR's `windowsZones.xml`; `--time-zone "Taipei Standard Time"` overrides it, and a host zone without a mapping leaves Setup's default (Pacific) with a note. The first `vm start` boots the installer by itself and Setup runs unattended:
virtio drivers, the Guest Service, an Administrators account `glov` that signs in automatically, with a random password
in `<vm>/secrets/`. `vm status` says `installing` until the Guest Service is ready and setup is done, which takes 10 to 15 minutes, and
`vm wait <vm> guest-ready` returns only then; the install media are then detached. Meanwhile both show the phase:
`booting the installer` and `Windows Setup` are told apart by the time since the first start alone, so an installer that never
booted also shows `Windows Setup`; `first sign-in and setup script` means the Guest Service answers. Past 15 minutes they
point to `glov vm screenshot <vm>`, which shows whether it is stuck. `vm wait` prints its progress on stderr: the phase when
it changes and a `still` line every minute. An install that does not finish within 60 minutes, or is interrupted, is not resumed: delete the VM
and create it again.

The guest is driven through the Guest Service. To also reach it over SSH, add `--ssh`: OpenSSH is installed for key
authentication only, with the key in `<vm>/secrets/id_ed25519`, and `--ssh-port` (or a free port) on `127.0.0.1` forwards
to it. `glov ssh win11` opens a shell (`glov ssh win11 -- hostname` runs one command): it replaces itself with
`/usr/bin/ssh` as user `glov` with that key, and keeps host keys in `<vm>/known_hosts`, not `~/.ssh/known_hosts`. If
the Mac is on a VPN that breaks QEMU's `10.0.2.3` DNS forwarder, pass `--dns 1.1.1.1,8.8.8.8` at creation or switch
any time with `glov vm configure win11 --dns 1.1.1.1,8.8.8.8` (`--dns auto` resets to DHCP).

`glov host media` builds the install ISO from Microsoft's Windows 11 24H2 Media Creation Tool catalog; Windows Update
brings the guest current. To register a disk that already has Windows on it instead, pass `--disk <qcow2>` and
`--firmware-code "$(brew --prefix qemu)/share/qemu/edk2-aarch64-code.fd"`.

`glov vm delete win11` deletes a stopped VM's directory (disk, secrets and snapshots) after asking; `--yes` skips the
question.

To build .NET Framework projects in the guest, install Visual Studio Build Tools with `--includeRecommended`, then
`glov vm reboot win11`: it restarts Windows through the Guest Service and returns once the new boot's Guest Service is
ready, so the next command never runs on the old boot.

## Give memory back to macOS

A Windows guest keeps every page it ever touched, so a VM with 12G holds close to 12G on the Mac even when Windows uses
far less. A VM made by `glov vm create` while `glov-qemu` is installed (see [install.md](install.md)) gets a
virtio-balloon device, and `glov vm balloon` asks the guest to hand memory back. Such a VM starts in auto, where glovd
picks the target from what the guest uses:

```sh
glov vm balloon win11        # mode, how much the guest has now, the target and, in auto, why
glov vm balloon win11 8G     # a fixed target: the guest keeps 8G, the rest goes back to macOS; auto is off
glov vm balloon win11 12G    # give it all back to the guest
glov vm balloon win11 auto   # let glovd choose again
```

In auto, every 5 seconds glovd reads the guest's memory stats and keeps what the guest uses plus headroom: 2G or half of
the use while the Mac's memory pressure is normal, 1G or a quarter of it at warning or critical. The target never goes
below 1G or a quarter of the VM's memory, nor above it. When the guest has less than 512M available, it gets all its
memory back at once (Windows pages before its stats show what it needs) and glovd checks again every second; the target
goes down only a minute after the last change, and a change under 256M is not made. So a 4G VM whose Windows uses 2G
keeps all 4G while the Mac has memory to spare, and gives about 1G back under pressure; a 12G VM building something with
6G keeps 9G.

The mode and a fixed target are kept in the VM's `vm.json`, so either can be set while the VM is stopped, and every
start applies them. A VM in auto starts with all its memory, since a boot needs more than the guest uses later, and
shrinks once its balloon service reports, a minute after the start at the earliest. A daemon restart (a `glov host
install`) leaves a running VM as it was: auto carries on from what the guest has and waits the usual minute before
giving more back. If a QMP call fails, auto keeps the target and tries again, waiting 5 seconds and up to 30; `glov vm
balloon` says so. The guest gives memory up gradually, and Windows stops early when it runs short, so the amount it
keeps can stay above the target; check again without a size. The target survives a guest restart: the Balloon driver
fills the balloon again after boot.

Setup installs that driver from virtio-win and its balloon service (`blnsvr.exe`, in `C:\Program Files\glov`), which
reports the guest's memory use to the device. Without it auto has nothing to go on, so it leaves the target where it is
and `glov vm balloon` says so.

A VM made before this, or made without `glov-qemu` installed, needs the device turned on while stopped: `glov vm
configure win11 --balloon on`, and the service installed once in the guest from the virtio-win ISO
(`Balloon\w11\ARM64\blnsvr.exe -i`, copied somewhere that stays), then `glov vm balloon win11 auto` if glovd should
choose the target. On Homebrew's `qemu` the setting adds no device and `glov vm balloon` says to install `glov-qemu`.

Turning `--balloon` on or off, or installing or removing `glov-qemu` for a VM with it on, changes the VM's devices,
and QEMU cannot load a live snapshot into a different machine. glov records the QEMU version, the balloon device,
CPUs and memory with each live snapshot and refuses to revert one taken on another machine, naming what changed and
how to put it back (`glov vm configure win11 --balloon on`, installing or removing `glov-qemu`, `--cpus`/`--memory`).
Disk-only snapshots of a stopped VM are not affected, and live snapshots taken before glov recorded the machine are
not checked.

## Sync host folders into a Windows VM

A Windows guest cannot mount a host folder. Instead, declare the folder once as a **share** and mirror it into the guest
with one command whenever you want the guest to see your changes, for example before each build:

```sh
glov share add win11 proj ~/src/proj 'C:\work\proj' --exclude bin/ --exclude obj/ --exclude /.git
glov share sync win11 proj          # or proj/src to sync one directory
glov share status win11 --watch    # from another terminal: what a running sync is on
glov share list win11
glov share remove win11 proj        # files already synced stay in the guest
```

- `share sync` (MCP `share_sync`) makes the guest folder a copy of the host folder: new and changed files are written,
  files deleted on the host are deleted in the guest. A file moves only when its SHA-256 differs; each side rehashes
  a file only when its size, modification time, change time or file id changed (`--verify` rehashes everything).
  A guest program can edit a file in place and then set both times back, which the guest's check misses: only
  `--verify` is sure to find every difference.
- `--exclude` takes gitignore patterns: `bin/` matches a `bin` directory at any depth, `/root-only` only at the share
  root. Excluded paths are neither copied nor deleted, so build output in the guest survives a sync.
- The host wins: a file changed in the guest is overwritten and listed. A file another guest process holds open fails
  the sync, which names it; close the program and sync again. A read-only file, or one the Guest Service may not write, fails
  as access denied; Windows reports replacing or deleting a held file the same way, so that failure says "in use or
  access denied".
- Symbolic links in the host folder are never followed: they are skipped, listed as not synced, and their guest
  paths are left alone. A `<subdir>` must be a real directory inside the share.
- Syncing happens only when asked; there is no watcher. The VM must be running with its Guest Service ready.
- One sync per share at a time. Stopping `share sync` (Ctrl-C, killing it) cancels its sync, in the guest too. A
  guest listing that makes no progress for 2 minutes is stopped with an error naming the file it was reading (another
  program holding it, for instance); the Guest Service's log (`C:\ProgramData\glov\guest-service.log`) also names files
  that took 30 s or more to hash. `glov share status <vm> [--watch]` shows each running sync's stage and the
  file the guest is hashing or writing, with its byte count, so a stuck file shows while it happens.
- The guest folder must be a folder of its own (not a drive root, `C:\Windows`, `C:\Program Files` or a user profile),
  since a sync deletes what the host folder lacks. The guest keeps its manifest in `C:\ProgramData\glov\sync\`.
  It may not be, or lie below, a symbolic link or junction: the Guest Service runs as LocalSystem, so the sync refuses
  one rather than follow it.
- A share joins the VM's copy allowlist: `cp` and `copy_to_vm` may read any share; `copy_from_vm` may write only into a
  share added without `--ro`. Two shares may not overlap, on the host or in the guest, so a `rw` share cannot open a
  `ro` one to writes. `vm create --share NAME,HOST,GUEST[,ro][,exclude=PATTERN]...` adds shares at creation.
- Linux (VZ) VMs get live mounts instead, below; `share sync` and `--exclude` are refused for them.
