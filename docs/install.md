# Install and run

## Requirements

- An Apple silicon Mac on macOS 26 or later
- To install a release: the [GitHub CLI](https://cli.github.com/) (`gh`), signed in to
  GitHub. No administrator rights, Xcode, Command Line Tools, Go or Homebrew.
- To build from source: Xcode or the Command Line Tools with Swift 6.2 or later, and Go 1.27 or later (for the Guest
  Service and `glov-image`, the helper that converts and checks Linux cloud images)
- `brew install qemu swtpm wimlib`, for Windows guests: QEMU and its UEFI firmware, a software TPM, and `wimlib-imagex`
  to build the install ISO
- Optional, for Windows guests: `brew install akunzai/tap/glov-qemu`, a QEMU build whose virtio-balloon gives the memory a
  guest hands over back to macOS (Homebrew's `qemu` keeps it on macOS), for `glov vm balloon`. It is keg-only, so it
  stays off `PATH` and leaves `qemu` alone; `glovd` uses it for Windows VMs whenever it is installed. Keep `qemu`
  installed as well: new VMs take their UEFI firmware from it, so they still boot if `glov-qemu` is removed.
- Nothing from Homebrew for Linux guests: `glov-image`, shipped and installed with glov, converts the cloud image and
  checks Ubuntu's signature
- Disk space: a Windows VM disk grows to tens of GiB. Install media and cloud images add several GiB more.

Windows install media and virtio-win need no manual download: `glov vm create` (or `glov host media`) fetches them from Microsoft and the
[Fedora virtio-win archive](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/),
verifies them and caches them under `~/Library/Caches/glov/windows/` (about 11 GiB). Set `GLOV_CACHE` to put
the cache on another disk.

Linux guests need no manual download either: `glov vm create --engine vz` fetches the newest build of the selected
`--distro` (`ubuntu` by default, or `debian`), checks its published checksums and signatures, and caches it under
`~/Library/Caches/glov/<distro>/` (or `$GLOV_CACHE/<distro>`).

## Install a release

Each release has a macOS arm64 archive with `glov`, `glovd`, `glov-image` and the Guest Services, and its SHA-256
file (the Windows x64 archive is in [Windows host](windows-host.md#install-a-release)). Unpack it under your home folder and install the daemon; nothing here asks for `sudo`:

```sh
cd "$(mktemp -d)"
gh release download -R akunzai/glov -p 'glov-*-macos-arm64.tar.gz*'
shasum -a 256 -c glov-*-macos-arm64.tar.gz.sha256
mkdir -p ~/.local/opt/glov ~/.local/bin
tar -xzf glov-*-macos-arm64.tar.gz -C ~/.local/opt/glov --strip-components 1
ln -sf ~/.local/opt/glov/glov ~/.local/bin/glov   # ~/.local/bin must be on PATH
glov host install
```

To upgrade, run the same steps with the newer release: unpacking replaces the files in `~/.local/opt/glov`, and
`glov host install` switches the daemon to the new `glovd` (see [Run the daemon](#run-the-daemon)). Name a tag
(`gh release download v0.2.0 -R akunzai/glov ...`) to pick a release other than the latest.

`gh release download` and `curl` leave no quarantine attribute on the archive, so Gatekeeper does not check the
binaries. An archive fetched with a browser carries one; clear it before unpacking with
`xattr -d com.apple.quarantine glov-*-macos-arm64.tar.gz`.

## Build from source

For development, build in the repository instead:

```sh
tools/build.sh
```

The build includes Windows and Linux arm64 Guest Services, with version and SHA-256 manifests, next to the host binaries. Python 3 writes the manifests. Use `tools/build.sh debug` for a debug build; plain `swift build` only builds the host, without the Guest Services or `glov-image`, which Linux guests need. `tools/package.sh` builds the release archive and its SHA-256 file into `.build/dist/`, as the release workflow does for a `v<VERSION>` tag.

Both builds produce `glov` next to `glovd`. The examples below assume `glov` is on your `PATH`.
A symbolic link keeps it pointing at the latest build (it replaces the link to a release, if any):

```sh
mkdir -p ~/.local/bin && ln -sf "$PWD/.build/release/glov" ~/.local/bin/glov   # ~/.local/bin must be on PATH
```

The `vm`, `share` and `snapshot` commands accept `ls` for `list` and `rm` for `delete`/`remove`:

```sh
glov vm ls
glov share ls win11
glov snapshot ls win11
glov vm rm win11  # asks for confirmation, just like vm delete
```

## Run the daemon

```sh
glov host install     # LaunchAgent: starts glovd now and at login
glov vm list
```

`glov host install` copies the bundled Guest Services into `bin/guest-services/` and `glovd` to `~/Library/Application Support/glov/bin/` and signs the copy ad hoc with the
`com.apple.security.virtualization` entitlement Linux guests need (`swift build` drops it; no Apple Developer account is
needed). The LaunchAgent runs that copy, so rebuilding or unpacking a new release does not touch the running daemon:
run `glov host install` again to switch to it. A `glovd` you start by hand needs `tools/linux-vm/sign-daemon.sh` after every build.

VMs live under `~/Library/Application Support/glov/<name>/`. Set `GLOV_HOME` to use another root, and keep it
short: unix socket paths are limited to 104 bytes.

Logs stay bounded:

| Log | Kept |
| --- | --- |
| `<root>/glovd.log` (the daemon) | Past 4 MiB it becomes `glovd.log.1`, replacing the previous one; checked at start and every 5 minutes |
| `<root>/<vm>/jobs/j*.log` (`exec` output) | The last 50 finished jobs per VM, counting those of earlier daemon runs |
| `<root>/<vm>/qemu.log`, `console.log` | Started afresh each time the VM starts; a VZ VM's last non-empty `console.log` becomes `console.previous.log` |
| `C:\ProgramData\glov\guest-service.log` (Windows Guest Service) | Past 1 MiB it becomes `guest-service.log.1`, replacing the previous one; the Linux Guest Service logs to journald |

## Connect an MCP client

Point the client at `glov mcp`. For Claude Code:

```sh
claude mcp add glov -- glov mcp
```

The tools are `vm_list`, `vm_status`, `vm_configure`, `vm_start`, `vm_stop`, `vm_reboot` and `vm_wait`; `screenshot`, `click`, `move`, `drag`,
`type`, `key` and `scroll`; `exec`, `exec_start`, `exec_read` and `exec_kill`; `file_read`, `file_write`, `copy_to_vm`
and `copy_from_vm`; `share_sync`; `snapshot_list`, `snapshot_create`, `snapshot_revert` and `snapshot_delete`; and, for
isolated VMs, `egress_list`, `egress_allow`, `egress_remove` and `egress_log` (changes are refused until
`glov egress mcp <vm> on`). Secrets (`glov secret`, model-API keys included) have no MCP tool.

On a Windows host, `glov mcp` (`claude mcp add glov -- glov mcp` there too) serves the tools of the features that
edition has, with the same names and input schemas: the `vm_*` tools, `exec`, `exec_start`, `exec_read`, `exec_kill`,
`file_read`, `file_write`, `copy_to_vm`, `copy_from_vm`, `share_sync`, and `egress_list`,
`egress_allow`, `egress_remove` and `egress_log` for isolated VMs (changes refused until `glov egress mcp <vm> on`). The
screenshot and input tools and snapshots are absent rather than failing. It differs in these ways:

- `exec` and `exec_start` take no `stdin`, and `as` defaults to root. `exec` keeps no log file on the host: its reply
  holds the last 50 lines and the line count, and `exec_read` from cursor 0 reads the whole output from glovd's
  last 16 MiB.
- `vm_reboot` takes no `force`.
- `copy_to_vm` and `copy_from_vm` take host paths in the VM's shares (a `rw` one when copying from the guest) or
  `%LOCALAPPDATA%\glov\scratch`, and `glov` reads and writes them as the user, as `glov cp` does.

Each call opens its own connection to glovd, which checks the caller is its owner, as it does for the CLI.

### Agent skill

[`skills/glov`](../skills/glov/SKILL.md) teaches a coding agent what `glov --help` does not: readiness checks,
`exec`/`cp`/`share` pitfalls and troubleshooting. Install it with
[Skills Manager](https://github.com/akunzai/skills-manager): `skills add akunzai/glov --skill glov`.
The same text is built into `glov`: `glov guide` prints the skill, `glov guide troubleshooting` its reference.
Product behaviour and exit codes (for example `glov exec --as user` exits 75 until someone has signed in) are in `glov <command> --help`.
