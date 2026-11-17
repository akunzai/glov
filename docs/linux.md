# Linux VMs

## Create a Linux VM

```sh
glov vm create ubuntu --engine vz
glov vm create debian --engine vz --distro debian
```

Each `vm create` starts the new VM and returns once its Guest Service answers (`--no-start` only creates it).

`vm create --engine vz` defaults to `--distro ubuntu` (the newest Ubuntu LTS from https://changelogs.ubuntu.com/meta-release),
or downloads Debian 13 (`--distro debian`). To install from another arm64 cloud image
(qcow2 or raw), pass `--image <path>`.

To adjust CPU count, memory or disk size (grow-only) on any stopped Windows or Linux VM:

```sh
glov vm configure ubuntu --cpus 4 --memory 8G --disk-size 64G
```

cloud-init installs the Guest Service on first boot. The VM's SSH key is in `<vm>/secrets/`; `glov ssh ubuntu` opens a shell over vsock, so it needs no guest address and works for an isolated VM too.
To run a coding agent in a Linux VM, see [Run a coding agent in an isolated VM](isolated-harness-vms.md).

## Share host folders with a Linux VM

A Linux (VZ) guest sees a host folder live, through virtiofs, with the same `share` commands. The guest path is optional
and defaults to `/mnt/<name>`:

```sh
glov share add ubuntu proj ~/src/proj              # mounted at /mnt/proj, read-write
glov share add ubuntu ref ~/src/ref /srv/ref --ro   # guest writes fail
glov vm create ubuntu --engine vz --share proj,~/src/proj --share ref,~/src/ref,/srv/ref,ro
```

- Each share is a virtiofs device attached when the VM starts (its tag is the share name). Once the Guest Service is
  ready, the daemon creates the directory and runs `mount -t virtiofs` in the guest (`-o ro` for `--ro`), unless the path
  already is a mount point. Nothing is written to cloud-init or `/etc/fstab`, so a share added after `vm create` works from the next
  start, and a reboot of the guest is mounted again. `vm status` lists each share as `mounting`, `mounted` or `failed`
  with the reason; a failed mount does not fail the VM.
- Changes are live in both directions (`ro`: host to guest only). There is nothing to sync: `share sync` and `--exclude`
  are refused. Shares added or removed while the VM runs apply at its next start, and `share list` says so.
- The guest path must be absolute, without `..`, and a folder below `/mnt`, `/media`, `/srv` or `/opt` (not one of
  them itself). The daemon creates and mounts it as root, so before it does, every directory from `/` down must be a real
  directory (no symbolic link) owned by root and not writable by group or others; missing ones are created with mode 0755. A
  path that fails the check is not mounted and `vm status` says why. Two shares may not overlap, on the host or in the guest.
  As for Windows, a share joins the VM's copy allowlist.
