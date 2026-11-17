# Updates and resource use

## Update the Guest Service

A VM keeps its installed Guest Service until you explicitly update it. After each Guest Service connection, the daemon compares the installed
file's SHA-256 with its bundled build in the background. `glov vm status <vm>` and MCP `vm_status` show `guestService.update`:
`up-to-date`, `update-available`, or `unknown` with a reason. A heartbeat does not repeat the check. Nothing is downloaded or installed automatically.

```sh
glov vm guest check win11          # fresh, read-only comparison
glov vm guest update win11         # use the running daemon's bundled Windows arm64 build
glov vm guest update win11 --force # allow interruption of running managed jobs
```

The target is the build shipped with the running daemon, not the newest online release. After rebuilding, run
`glov host install` again to refresh that distribution. Missing bundles, stopped VMs and unavailable Guest Services produce an
`unknown` result, never `up-to-date`. Checks do not copy or run the target binary in the guest.

Updates require a running VM and a ready Guest Service. Running managed jobs block an update and their IDs are listed; wait for them,
terminate them, or explicitly use `--force`, which also cancels a share sync in progress. An exec start or share mount already
in progress must finish first, even with `--force`. During the update, new exec starts and share syncs are refused. This protection covers glov-managed work,
not every process in the guest. The daemon serializes updates with VM start/stop and snapshot operations.

Both commands accept `--guest-service <path>` for a custom Windows/Linux arm64 binary. A custom binary's target version is unknown
until installation, but its contents are still compared by SHA-256. Matching files are left alone; after an update the
command verifies the restarted Guest Service's version and the installed file's SHA-256. Build custom binaries with
`-ldflags "-X main.version=..."` for readable version labels.

## Watch VM resource use

```sh
glov vm stats            # every running VM, and the Mac's memory pressure and swap
glov vm stats win11 --watch
glov vm stats --json
```

Each row shows the VM's CPU use on the Mac (100% is one core), its configured memory, what a Windows guest with a balloon has
now and its `glov vm balloon` mode (`4.7G auto`; `-` without one), how much of it is in Mac RAM and how much compressed or
swapped out, and its disk image's allocated and maximum size; then, from its Guest Service, guest memory used (without what the
balloon holds, which the guest itself counts as used) and available, the system volume's used and total size, and the load. A VM whose Guest Service is not ready still shows the Mac's
numbers, with the reason the guest's are missing. `glov vm start` warns when the VMs would reserve more
than 2/3 of the Mac's memory or the Mac is already under memory pressure; a guest that stops answering under memory pressure
says so in `vm status`. A VM's Guest Service needs updating (`glov vm guest update`) before it reports the guest's numbers.
