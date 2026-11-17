# Windows host

The Windows edition of glov runs Linux VMs on a Windows PC, through the Host Compute Service that WSL 2 and
Windows containers also use. It is a separate Go implementation (`windows/`) of the same `glov` commands, MCP tools and
Guest Service protocol, for the features it has:

- Linux VMs: Ubuntu 26.04 LTS (the default) or Debian 13, from the distros' x86_64 cloud images.
- `glov exec`, `glov cp`, `glov ssh`, `glov vm console`, and [shares](#shares): `glov share sync` copies a host
  folder into the guest, and a live share keeps the two in step both ways (no mount in either case).
- Every VM is isolated: no network adapter, its only way out an egress allowlist proxy in `glovd` (`--egress open`
  for any public host); for coding-agent harnesses, model-API keys kept on the host and a template that
  `glov vm reset` returns each task to.
- `glov claude`, `codex` and `agy` on a project, which reaches its VM [through git](isolated-harness-vms.md#on-a-windows-host).
- `glov mcp`, serving the matching MCP tools.

It has no Windows guests, no VM window or screenshots, and no snapshots.

## Requirements

- An x64 PC on Windows 11 with **Virtual Machine Platform** turned on (Turn Windows features on or off,
  `optionalfeatures.exe`). The Hyper-V feature itself is not needed. Verified on Windows 11 Pro x64.
- The [GitHub CLI](https://cli.github.com) (`gh`) to download a release; to build from source instead,
  [mise](https://mise.jdx.dev), which installs the Go version in `mise.toml`, and Git. PowerShell or Git Bash both
  work.
- An administrator account, once, to install glov-broker. Everything else, `glov host install` included, runs from a
  normal, non-elevated shell.
- Disk space: about 2 GiB in the cache for Ubuntu and 1 GiB for Debian, and each VM's disk
  grows to what its guest writes.

## Install a release

Each release has a Windows x64 archive with `glov.exe`, `glovd.exe`, `glov-broker.exe` and the Linux Guest Service,
and its SHA-256 file. Download and unpack it in a normal PowerShell:

```powershell
$dir = New-Item -ItemType Directory -Force "$env:TEMP\glov-release"; Set-Location $dir
Remove-Item glov-*-windows-x64* -Recurse -ErrorAction SilentlyContinue
gh release download -R akunzai/glov -p 'glov-*-windows-x64.zip*'
$zip = Get-Item glov-*-windows-x64.zip
(Get-FileHash $zip).Hash -eq (Get-Content "$zip.sha256").Split(' ')[0]   # must print True
Expand-Archive $zip .
$glov = (Resolve-Path glov-*-windows-x64\glov.exe).Path; $glov
```

The first time, install glov-broker from an elevated PowerShell (Run as administrator), with the path the last line
printed:

```powershell
& '<path printed above>' host install-broker
```

Then, back in the normal PowerShell:

```powershell
& $glov host install
```

To upgrade, run the same steps with the newer release, skipping `host install-broker` unless the release notes say
glov-broker changed. Name a tag
(`gh release download v0.2.0 -R akunzai/glov ...`) to pick a release other than the latest. The download folder
can be deleted afterwards: `glov host install` copies what it needs.

glov runs in two parts:

- **glov-broker**, a Windows service running as SYSTEM, makes the Host Compute Service calls a user cannot: it
  creates, starts and stops the VMs. It builds each VM from a narrow request (CPUs, memory, disk paths), opens those
  disks as the caller, refuses one under a link or junction or owned by someone else, and ties each VM to the user
  who made it, so no other user sees or drives it. `glov host install-broker` copies it to
  `%ProgramFiles%\glov\broker` and starts it; it also removes the `glovd` service that earlier builds installed,
  leaving that service's data in `%ProgramData%\glov`. `glov host uninstall-broker` removes it once no VM runs.
- **glovd** runs as you. `glov host install` copies `glov.exe`, `glovd.exe` and the Guest Service to
  `%LOCALAPPDATA%\glov\bin`, puts that folder on your `PATH`, has Windows start `glovd` at each sign-in and starts it
  now; `glov` also starts it when it is not running. It keeps your VMs, secrets and egress proxies in your glov home,
  `%LOCALAPPDATA%\glov` (`GLOV_HOME`, set as a user environment variable, moves it).

On an upgrade VMs keep running, but every connection a VM has open through the egress
proxy breaks while `glovd` restarts, so let downloads, pushes and harness turns in the VMs finish first. After an
upgrade, `glov vm status <vm>` ends in `update available` until
`glov vm guest update <vm>` installs the new Guest Service ([Updates](operations.md#update-the-guest-service)).
`glov host install --cache D:\glov-cache` keeps the image cache elsewhere, and `glov host uninstall` removes your
`glovd`, keeping VMs and data. `glov` names `glov host install-broker` when glov-broker is missing or too old for it.

Signing out of Windows ends `glovd`, and glov-broker shuts your VMs down unless you are still signed in to another
session (a disconnected Remote Desktop one counts), where `glov` starts `glovd` again. Windows shutting down stops
every user's VMs. Sign in and `glov vm start` them again.

Earlier builds ran `glovd` as a SYSTEM service with its data in `%ProgramData%\glov`. Nothing is migrated: copy out
what the old VMs hold, run `glov host install-broker` (which removes that service), create the VMs again and store
the secrets again. Delete `%ProgramData%\glov` from an elevated shell once you no longer need it.

## Build from source

For development, build the same files from a clone and install them the same way:

```powershell
mise trust; mise install
mise run windows:build                        # glov.exe, glovd.exe, glov-broker.exe and the Linux Guest Service in .build\windows
# once, in an elevated shell:
.build\windows\glov.exe host install-broker   # or: mise run windows:install-broker
# then, in a normal shell:
.build\windows\glov.exe host install          # or: mise run windows:install
```

`tools/package-windows.ps1` (pwsh) builds the release archive and its SHA-256 file into `.build\dist\`, as the
release workflow does for a `v<VERSION>` tag.

## First VM

```powershell
glov vm create ubuntu                         # downloads the cloud image once, provisions the VM, starts it and waits for its Guest Service
glov exec ubuntu -- uname -a
claude mcp add glov -- glov mcp           # or point any MCP client at `glov mcp`
```

`--no-start` leaves the new VM stopped. `glov help` lists the commands. `glov guide` prints the agent skill, which covers the Windows edition's pitfalls.

## Shares

The guest never mounts a host folder. `glov share add <vm> <name> <folder>` names one, which the guest sees at
`/mnt/<drive>/<path>` in its own file system, and one of two ways copies it there:

- `glov share sync <vm> <name>` mirrors the host folder into the guest once: changed files are written, and files the
  host lacks are deleted. Run it again after host edits.
- `glov share add <vm> <name> <folder> --live` keeps the folder and the guest's copy in step both ways while the VM
  runs. A background `glov share agent` started by `share add`, `vm start` and `share sync` copies each change within
  a fraction of a second, and runs a full comparison at its start and every few minutes. A file changed on both sides
  since they last matched keeps the newer one; the other is lost, and `glov share status <vm>` lists the path.
  `glov share sync` on a live share asks its agent for a full comparison at once. The agent exits, letting go of
  the folder, within seconds of the VM stopping or being deleted or of `share remove`.

A live share's guest copy is ext4, so `git status` and builds in the guest run at disk speed. Some limits:

- Symbolic links and junctions in the folder are not copied either way and are not followed; neither are the
  `--exclude` paths, on either side: the host refuses a guest change at or below one, and the agent's log lists it.
  On the host a pattern matches names in any case, as Windows finds them (`secret/` also keeps out `SECRET`).
- A guest file whose name a Windows folder cannot hold as itself is not copied to the host: a name ending in a dot or
  a space, a device name such as `NUL`, `CON` or `COM1` (with any extension), or one holding `\ : * ? " < > |` or a
  control character. The agent's log lists them.
- With `core.autocrlf=true` on Windows, the checkout has CRLF line endings and git in the guest shows those files as
  modified. Pin `eol=lf` in `.gitattributes`, or check the repository out with `core.autocrlf=false`.
- The agent runs as you: it stops when you sign out of Windows or the VM stops, and the next `glov vm start` or
  `glov share sync` starts it again, copying what changed meanwhile. It waits out a glovd restart, such as an
  upgrade. Its log is in `%LOCALAPPDATA%\glov\vms\<vm>\sync\`.
- The guest watches the copy with inotify, one watch per directory. A tree with more directories than the guest's
  `fs.inotify.max_user_watches` stops the agent with that error in its log; raise the limit with `sysctl` in the
  guest.

## Where things are

| Path | What |
| --- | --- |
| `%ProgramFiles%\glov\broker` | `glov-broker.exe` |
| `%ProgramData%\glov-broker` | glov-broker's log and which user owns each VM ID |
| `%LOCALAPPDATA%\glov\bin` | `glov.exe`, `glovd.exe` and the Guest Service it installs into guests |
| `%LOCALAPPDATA%\glov\vms\<vm>\` | `vm.json`, the VM's disks, `console.log`, `egress.log`, `secrets\` (SSH keys), and each live share's state (`sync\`); only you, SYSTEM and administrators can open it |
| `%LOCALAPPDATA%\glov\cache\` | Cloud images converted to VHDX, shared by the VMs as their base disks. After `vm create` and `vm delete`, builds no VM uses are removed, except the newest of each distro, and both commands print what they removed |
| `%LOCALAPPDATA%\glov\secrets\` | `glov secret` values, DPAPI-encrypted for you, and `secrets.json` (names, sources, hosts) |
| `%LOCALAPPDATA%\glov\egress-ca\` | The egress CA that isolated VMs trust and that signs the hosts the proxy intercepts |
| `%LOCALAPPDATA%\glov\glovd.log` | `glovd`'s log |
| `%LOCALAPPDATA%\glov\scratch` | Where `glov cp` may write besides a VM's shares |
| `%LOCALAPPDATA%\glov\sessions\<vm>\` | A harness VM's conversation records, per harness; kept after `glov vm delete` |
| `%LOCALAPPDATA%\glov\ssh_config` | The `glov-<vm>` SSH hosts git reaches project VMs through (`glov claude`); `ssh_config.declined` remembers a no to the `Include` |
| `%LOCALAPPDATA%\glov\project-remotes.json` | The repositories of project remotes you let a project's VM reach |

Read a VM's boot with `glov vm console <vm>`, without elevation.

## More

- [Networking on a Windows host](networking.md#linux-vms-on-a-windows-host), and
  [isolated VMs](networking.md#isolated-vms-on-a-windows-host) with model-API keys and harnesses.
- [MCP on a Windows host](install.md#connect-an-mcp-client): the tools that differ from macOS.
- From Git Bash, set `MSYS_NO_PATHCONV=1` for `glov exec` and `glov cp`. Otherwise guest paths such as `/bin/sh` turn
  into Windows paths.
