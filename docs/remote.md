# Remote VM access

Run the CLI on your local Mac to operate an existing daemon on another Mac. The VM viewer runs locally; projects,
builds, VM disks and guest jobs remain on the remote host. A terminal client running inside an SSH session cannot
implicitly open a window on your laptop.

```sh
glov --remote mac-mini vm ls
glov --remote mac-mini view win11 --log-fps
glov --remote mac-mini view ubuntu --read-only
glov --remote mac-mini exec win11 -- powershell.exe -NoProfile -Command 'Get-Date'
glov --remote mac-mini mcp
```

`glov` accepts the same options. Connection options may precede or follow the subcommand, but must precede
`--`; everything after `--` belongs to the guest command.

## Prerequisites

- Both clients and the daemon are built from compatible glov sources. Remote access requires the daemon's
  `daemon.info` protocol handshake; an older daemon without it is rejected. Upgrade/install the remote daemon
  separately when it is safe to interrupt its VZ VMs. The remote client never installs, upgrades or restarts it.
- The remote Mac is awake, its user is logged in, and that user's daemon is running. Pre-login recovery, FileVault
  unlocking and automatic recovery after a host reboot are outside this feature.
- System SSH can reach the host. First run `ssh mac-mini true` in your own terminal to establish authentication and
  verify its host key. Host aliases, keys, SSH agents and configured jump hosts use your normal SSH configuration.
  In a noninteractive session (including MCP), authentication must work without a password prompt.
- SSH Unix socket forwarding is permitted on the remote SSH server. glov creates only a private local Unix
  socket; it does not expose a TCP management listener. The remote SSH account is trusted to control its daemon.

A private connection such as Tailscale can provide reachability for ordinary SSH. glov does not manage the
tailnet or require the separate Tailscale SSH feature.

The default remote root is the SSH user's `~/Library/Application Support/glov`, resolved on the remote Mac.
For a daemon with another root, use its absolute path:

```sh
glov --remote mac-mini --remote-home /Volumes/VMs/glov vm ls
```

The laptop's `GLOV_HOME` does not select the remote daemon. `view --home` remains a local-only option; use
`--remote-home` in remote mode. Remote roots cannot contain colons or control characters and must fit the existing
Unix socket path limit (103 UTF-8 bytes including `/glovd.sock`).

## Supported commands and paths

| Command | Remote behavior |
| --- | --- |
| `vm list/ls`, `status`, `start`, `stop`, `wait` | Operate on the selected remote daemon's VMs. |
| `view` | Local native window; its title identifies the VM and SSH host. Keyboard, pointer, scroll and read-only mode use the remote daemon. |
| `screenshot -o PATH` | Capture the remote VM and write `PATH` on the laptop. |
| `exec` | Guest paths, environment and working directory belong to the guest. Output streams locally. Job logs stay on the remote host. |
| `cp` | The host side is an **absolute path on the remote Mac**, still checked against the VM's host-path allowlist. The other side is `vm:guest-path`. |
| `mcp` | MCP stays on local stdio; all tools use the selected remote daemon. Copy-tool host paths refer to the remote Mac and must be absolute. |
| `snapshot` | List/create/revert/delete snapshots on the remote host. |
| `share sync` | Sync an existing remote-host share into its guest. It does not upload laptop files. |
| `share status` | Show the remote VM's running share syncs and the guest file each is on. |
| `guest check/update` | Use the remote daemon's bundled Guest Service. A custom `--guest-service` must be an absolute remote-host path. |

Remote mode rejects `vm create/delete/rm`, `share add/remove/rm/list/ls`, `daemon`, `media` and guest `ssh` commands
before changing local configuration or connecting. Run those commands on the remote Mac. Local CLI operation is
unchanged. No operation silently falls back to a local daemon.

For example, once a file is in an allowlisted remote scratch/share directory:

```sh
glov --remote mac-mini cp /absolute/remote/share/input.txt 'win11:C:\work\input.txt'
glov --remote mac-mini cp 'win11:C:\work\output.txt' /absolute/remote/share/output.txt
```

Use your existing SSH/SCP workflow to move laptop files to/from that remote directory. Relative paths and `~`
are rejected for remote copies rather than being expanded on the wrong Mac. There is no transparent project upload
or synchronization. The daemon's allowlist, symlink checks and read-only-share rules still apply.

## Disconnects and uncertain outcomes

Closing a viewer or MCP client does not stop remote VMs or submitted managed jobs. Remote `exec` prints the job ID
on stderr once submission succeeds; reconnect with the same host/root and use MCP `exec_read` with that ID to read
its status and output. `exec_kill` is an explicit cancellation operation.

The viewer dims the last frame and shows a connection error after detecting a failure. Subsequent frame polls can
establish a fresh SSH tunnel. Input is disabled until a frame is received; queued events and unfinished gestures
from an earlier connection are discarded. A frame also carries a daemon-instance identity, so a replacement daemon
does not inherit old-frame input. Events already accepted by a daemon may have taken effect.

Requests are not automatically replayed. If a connection fails after an operation was sent, the error says its
outcome may be unknown: inspect remote state before resubmitting. A missing daemon, SSH failure or incompatible
protocol never triggers installation, daemon restart or a change of destination.

`--log-fps` reports frame rate, base64 frame-payload MiB/s, frame RPC/decode round-trip time and successful input RPC
round-trip time every few seconds. These are application measurements: payload throughput excludes SSH overhead,
and RPC time is not a measurement of human input-to-display latency. `n/a` means no input samples in that interval.

## Verification

Follow `docs/agents/verification.md` for build, normal tests, strict cooperative-pool tests and isolated VM roots.
The remote transport tests use a boundary fixture for SSH while forwarding real Unix socket RPCs. They cover version
rejection, copies/allowlists, lost replies without replay, job continuity and stale-input rejection. Viewer state
tests cover frame readiness and invalidation across disconnects/replacement connections.

For an optional **same-host** real-SSH integration check, set `GLOV_TEST_SSH_HOST` to an SSH alias that points back
to the current Mac. The test starts its own isolated daemon and Guest Service fixture; do not point it at another Mac,
because its test root exists on the local filesystem:

```sh
GLOV_TEST_SSH_HOST=mac-mini swift test --filter RemoteCLITests
```

This check exercises SSH, CLI, MCP, guest execution and copies. It does not prove two-Mac latency or desktop usability.
Before declaring remote control fully verified, use a laptop and a remote Mac with disposable Windows and Linux guests:

1. Exercise list/status/start/stop, viewer input/read-only mode, screenshot output, copies, and MCP against the selected
   remote root. Use fixture data and `samples/HelloFramework` for the Windows development workflow.
2. Show installation dialogs, forms and development/debugging interactions; record the viewer and its timing output.
3. Interrupt the private-network/SSH connection and reconnect. Confirm a visible disconnected state, no delayed old
   input, the correct host/VM after recovery, and continued VM/job execution after disconnect and viewer close.
4. Check authentication/host-key errors, missing daemon, incompatible protocol and unsupported remote commands.

Capture only controlled guest/viewer contents; never include real credentials, personal data or VM `secrets/`.
