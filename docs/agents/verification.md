# Verification

How an agent exercises a change in this repo before it reaches review.

## Starting the environment

Run `tools/verify.sh` for the host gate (it runs `tools/check-issue-references.py` first, as the CI `references` job does; a few minutes at most; each test run stops at `VERIFY_TEST_TIMEOUT`, 900 s by default, and then names the unfinished tests and samples the test processes into `.build/verify/`), plus the Go checks under Checks when `guest/` changes.
The script builds the host, runs the tests, then repeats them with `LIBDISPATCH_COOPERATIVE_POOL_STRICT=1`.
It selects the compatibility backend and Testing search paths for the Command Line Tools layout that otherwise fails
to load Swift Testing or emits unsigned executables; other developer installations use SwiftPM's default backend.
Backend selection happens before the build, so a failing build or test stops verification without retrying another backend.
When changing this entrypoint, also run `python3 -B -m unittest discover -s tools/tests -p 'test_*.py' -v`.
The strict run limits Swift's cooperative pool to one thread, so
blocking work on it (a socket write, a process wait) hangs here as it would on a small CI runner; such work goes through `Blocking.run`. The tests include
integration tests that boot EDK2 to the UEFI shell under real QEMU, two directly (one covers live and stopped snapshots, which also needs `qemu-img`) and one through
`glovd` and `glov mcp`; they run only when `qemu-system-aarch64` and `swtpm` are on PATH
and are reported as skipped otherwise. `UbuntuSignatureTests` builds `glov-image` with Go and checks Ubuntu's published signature with it, and is
skipped without `go` on PATH; `mise run diskimage:test` covers the helper's own packages. Tests use a short temp `GLOV_HOME` (`/tmp/gp.XXXXXX`, because unix
socket paths must stay under 104 bytes), never the default root. A change outside the Swift package is verified by the end-to-end check
under Checks, or reported under Not verified.

<!-- drift:forge github -->
<!-- drift:entrypoint-cmd tools/verify.sh -->

It never prompts. A step needing a human aborts non-zero naming the
prerequisite — see Human prerequisites below.

**Proof it ran**: the host gate and end-to-end check each print `verify: ok` as their last line; each test run ends
with `Test run with N tests in M suites passed`, and the integration tests are listed
as passed rather than skipped (`bootsToUEFIShellTakesScreenshotTypesAndStops`, `liveAndStoppedSnapshotsCreateRevertListAndDelete`, `liveRevertRestartsQemuIntoTheSnapshotAndReconnectsTheGuestChannel`, `driveAVMThroughTheDaemonAcrossClientAndDaemonRestarts`).

CI (`.github/workflows/ci.yml`) automatically runs the `guest` job for the Guest Service's Go checks on Linux for pull requests and pushes
to `main` that change `guest/` or the CI workflow, and the `windows` job for the Windows edition (`windows/`) on a
Windows runner, plus `windows-portable` for its portable packages on Linux, when `windows/`, `guest/` (which it imports), `skills/` (which `glov guide` embeds) or the CI workflow changes. Other changes skip these jobs.
Unreadable or incomplete change lists run both instead of skipping them.
The `references` job runs on every pull request and push, whatever it changes: `tools/check-issue-references.py` fails
when a tracked file cites an issue or pull request by number (`mise run repo:check-references` runs it locally, on any host).

The macOS `host` job runs the entrypoint tests and `tools/verify.sh` on `macos-26` for every pull request and push to
`main`. A manual run (**CI → Run workflow**, or `gh workflow run ci.yml --ref <branch>`) runs only `references` and
`host`. `main` requires `references`, `host`, `guest`, `windows-portable` and `windows` to pass; a job skipped for an
unrelated change counts as passed.

The macOS runner has no QEMU, so the four QEMU integration tests skip there and remain a local check. Manual triggering
still consumes runner usage; it controls when the macOS cost is incurred.

**VZ (Linux) VMs need a signed daemon.** Virtualization.framework refuses a process without the
`com.apple.security.virtualization` entitlement, and `swift build` relinks `glovd` without it: run
`tools/linux-vm/sign-daemon.sh` after every build and before starting the daemon (an unsigned daemon says so at
startup and on `vm start`). The test runner cannot carry the entitlement, so `swift test` covers the VZ configuration,
seed, routing and snapshot files without booting; booting a Linux guest is the manual check below.

**A Windows release is verified with `glov claude`, not only `vm create`.** Before publishing the draft a `v<VERSION>`
tag opens, install its Windows archive (`gh release download v<VERSION> -p '*windows*'` reads a draft too) and run the
Windows glov claude row below. 0.2.0 and 0.2.1 shipped with every new project VM refused, because their checks created
only plain VMs.

## Checks

| What | Command |
| --- | --- |
| End-to-end in a Windows guest | through `glov mcp` (or `glov cp` / `glov exec`): `copy_to_vm` `samples/HelloFramework` from `<home>/scratch/`, `exec_start` `powershell.exe -NoProfile -ExecutionPolicy Bypass -File <dir>\verify.ps1`, `exec_read` until it exits; the output ends with `verify: ok` |
| Windows balloon | with `glov-qemu` installed (`brew install akunzai/tap/glov-qemu`), on your own root: `glov vm configure <vm> --balloon on` while stopped, `vm start`; `ps` shows QEMU from `/opt/homebrew/opt/glov-qemu/bin` with `virtio-balloon-pci,id=balloon0`; `glov vm balloon <vm> 2G`, and a minute later `glov vm balloon <vm>` reports about 2G and `footprint <qemu pid>` shows `phys_footprint` lower by about the difference; `glov vm balloon <vm> <full size>` gives it back; then fill and read back guest memory (2.5 GiB written per 4 KiB page, read after 60 s) with no page wrong, also after `glov vm reboot` and after a QMP `system_reset` while the balloon is inflated (the next boot comes up within a minute) |
| Windows balloon auto | on your own root and daemon, started with `GLOV_DEBUG_MAC_PRESSURE_FILE=<file>` (debug builds only: `normal`, `warning` or `critical` in the file stands in for the Mac's pressure, so none has to be made): `glov vm balloon <vm> auto`; the reason line shows the guest's use plus 2G, and the target follows it within 10 s. Write `warning`: within 10 s the target drops to use + 1G. Run a script that touches 2.5 GiB and one that writes and verifies a pattern (as in the row above): the reason says `raised to all <-m> at once` within about a second of the guest running short and the guest gets all its memory, both finish, no page wrong. `vm stop`: `glov vm balloon <vm>` shows `stopped; auto`; `vm start` boots with all of `-m`, says `no memory stats` until the balloon service reports, and shrinks a minute after the start at the earliest. Restart the daemon while the VM runs and the guest is shrunk: `glov vm balloon <vm>` keeps the amount it had (no jump to all of `-m`), the reason says `going down ... in <n>s` or follows the guest within 10 s, and the first decrease waits a minute. `glov vm balloon <vm> <size>` shows `fixed` and vm.json has `balloonAuto` false |
| Windows edition (Go) | `mise run windows:test` (`go vet` and `go test` in `windows/`). On macOS or Linux it covers the portable packages, glovd's daemon included; on a Windows host it covers all of them: the named-pipe tests (`internal/daemon/pipe_windows_test.go`) and the HCS adapters, whose HCS and data-directory tests run only in an elevated shell and skip otherwise |
| Windows service | `mise run windows:build` builds `glov.exe`, `glovd.exe`, `glov-broker.exe` and `glov-guest-linux-amd64` into `.build/windows`; `mise run windows:install-broker` from an elevated shell runs `glov.exe host install-broker` (installs `%ProgramFiles%\glov\broker`, data in `%ProgramData%\glov-broker`; an old `glovd` service is stopped and deleted, `sc.exe query glovd` then fails, and `%ProgramData%\glov` stays), and `mise run windows:install` from a non-elevated shell runs `glov.exe host install` (installs `%LOCALAPPDATA%\glov\bin`, puts it on the user `PATH`, adds the `glovd` value under `HKCU\...\CurrentVersion\Run` and starts `glovd.exe`, which opens no window; data in `%LOCALAPPDATA%\glov`); from an elevated shell it is refused. Run the `glov` commands from a non-elevated shell: `explorer.exe <script>.cmd` starts a script at Medium integrity, where `runas /trustlevel` stays High. With `glovd` ended (`glov` has no stop command: `taskkill /im glovd.exe /f`), `glov vm list` starts it again; with glov-broker stopped (`sc.exe stop glov-broker`), `glov vm start` says to run `glov host install-broker`. `glov vm create <vm> --distro debian` from that shell downloads the Debian cloud image once then boots the VM once to provision it (`provisioning: ...` lines; a failure logs the guest console to `glovd.log`), and leaves `cache\<release>\base.vhdx` and `vms\<vm>\` (`vm.json` with `"provisioned":true`, `disk.vhdx`) in the data directory; then `vm create` starts it and waits for `guest-ready`, as `vm start <vm>` and `vm wait <vm> guest-ready` do after `vm create --no-start` (a few seconds; stderr says `waiting for <vm> to be guest-ready (up to 15m)`, or `(up to 1m)` from `vm wait`, then each state change, such as `<vm>: running, Guest Service connecting (1s)`, and a `still` line every minute), `vm status`, `vm reboot <vm>`, `vm stop <vm>` and `vm stop <vm> --force`; end `glovd` (`taskkill /im glovd.exe /f`) and run `glov vm status` while a VM runs: it still shows it running with its Guest Service ready, and so it does after `sc.exe stop glov-broker` and `sc.exe start glov-broker`, and `vms\<vm>\console.log` holds the boot's serial console; `vm stop`, then `vm configure <vm> --disk-size <larger>` and `vm start`: `console.log`'s `Checking local filesystems` line shows the larger block count once the boot after that has run; `vm delete <vm>` asks (answer `n` and it exits 1), `--yes` deletes, a prefix is refused, and `icacls` on `base.vhdx` then lists no `S-1-5-83-1-` VM account; With a VM running: `glov exec <vm> -- /bin/sh -c "seq 1 300000"` prints every line, a command's exit code becomes glov's, `--as user` exits 75, and Ctrl+C leaves no process behind in the guest; `glov cp <dir> <vm>:/root/x` and back reproduce the tree, empty directories included (compare `sha256sum`); `glov ssh <vm> -- <command>` from the non-elevated shell accepts the host key on first use, runs as `glov` (`sudo -n id -un` prints `root`), passes stdin and the exit code through, and with the VM stopped says to start it, and `vm delete` removes `%LOCALAPPDATA%\glov\vms\<vm>\`; once `guest-ready` and after `glov egress allow <vm> deb.debian.org`, `curl https://deb.debian.org/debian/` in the guest succeeds after a start, a `vm reboot` and a `glovd` restart (a request loop running across the restart loses its `glov exec` connection, as the egress proxy lives in `glovd`; the next request after `vm wait <vm> guest-ready` succeeds), and `hnsdiag list endpoints` shows no `glov-` endpoint while it runs. Shares: `glov share add <vm> proj <repo of 10k+ files> --exclude node_modules/` (a drive root, the profile, `%LOCALAPPDATA%\glov`, `.ssh`, a junction and an overlapping folder are refused), `glov cp` from a folder outside every share is refused, `glov share sync <vm> proj` twice (the second writes nothing; note both times), then edit, delete and rename files on Windows, sync again and compare `sha256sum` of every file in `/mnt/<drive>/<path>` with `Get-FileHash` on the host; Ctrl+C during a sync of a large file ends it and `glov share status <vm>` then shows nothing running. From Git Bash set `MSYS_NO_PATHCONV=1`, or `/bin/sh` becomes a Windows path. `host install --cache D:\<dir>` moves the cache and an upgrade without it keeps it (the `Run` value's command line). A second Windows user, after their own `glov host install`: `glov vm list` shows none of the first user's VMs, and opening the first user's `%LOCALAPPDATA%\glov` is denied. Sign the first user out with a VM running: `glov-broker.log` has `user signed out; stopping their VMs` and `VM stopped`, and after signing in, `glov vm start` boots it with its file system clean (`journalctl -b -1` ends with `Journal stopped`). Finish with `glov host uninstall` (the `Run` value and the `PATH` entry go, `%LOCALAPPDATA%\glov\bin` goes) and, elevated, `glov host uninstall-broker`, refused while a VM runs; delete `%LOCALAPPDATA%\glov` and any `--cache` directory if they hold only test data |
| Windows host shutdown | with glov-broker and `glovd` installed, a running VM and a file written in it after `sysctl -w vm.dirty_writeback_centisecs=0 vm.dirty_expire_centisecs=360000` and `mount -o remount,commit=3600 /` (so only a clean shutdown flushes it): restart Windows. `%ProgramData%\glov-broker\glov-broker.log` has `stopping every VM for host shutdown` (or `user signed out; stopping their VMs`, when the sign-out comes first) and `VM stopped` within about 15 s, then `glov-broker stopped`, with no crash. After `glov vm start`, `journalctl -b -1` in the guest ends with `systemd-shutdown` syncing and `Journal stopped`, and the file is there. `orphan cleanup` in the next boot's kernel log is not evidence either way: Ubuntu logs it after a normal `glov vm stop` too. `Restart-Service glov-broker` alone still leaves the VM running |
| Windows Debian and Ubuntu VMs | with glov-broker and `glovd` installed, from a non-elevated shell, once per distro: `glov vm create <vm> --distro debian` (or `ubuntu`) downloads the cloud qcow2 once, checks it against `SHA512SUMS` (Debian) or the signed `SHA256SUMS` (Ubuntu) (`verifying <file>`), converts it to `cache\<release>\base.vhdx` (`converting <file> to VHDX`) and provisions it (`provisioning: booting the VM`, the guest's steps up to `turning off the network wait`, then `provisioning: waiting for the VM to power off`; a few seconds on Debian, about 40 s on Ubuntu, which waits for snapd); each step prints `still <step> (Xm YYs)` every 30 s while it runs; then it starts and waits for `guest-ready`; then the Windows service row's VM checks: `glov exec` (`dmesg` shows `Secure boot enabled`, `systemctl --failed` lists nothing, `curl` to the distro's archive succeeds once it is allowed), `vm reboot`, `vm configure --disk-size <larger>` and `df -h /` shows it after the next boot, `glov ssh <vm> -- sudo -n id -un` prints `root`, `glov vm guest update` with a differing build (it runs through `systemd-run`, and `boot_id` is unchanged), and `glov vm console <vm>` prints the boot without escape sequences while `--follow` keeps printing across a `vm reboot`. Host clock: `date -s "-60 sec"` in the guest is undone within 2 s (`journalctl -u glov-guest` shows `stepped the clock to the host's`); and after the host sleeps (Modern Standby) for 2 minutes or more, the guest's `date -u` matches the host's UTC time within a few seconds of waking, in an isolated VM and an ordinary one alike |
| Windows live shares | with glov-broker and `glovd` installed, from a non-elevated shell, on a running VM and again on an isolated one: `glov share add <vm> <name> <clone of a few thousand files> --live` prints `(live)` and `glov share status <vm>` shows `<name>: live, kept in step for N s` within seconds; the agent log `%LOCALAPPDATA%\glov\vms\<vm>\sync\<name>.live.log` has `live share started` and a full round; a host edit, a new file, a new directory and a deletion each reach the guest within a second, and the same from the guest reach the host; a guest editor-style save (write a temp file, rename it over the target, 20 times) leaves only the last version on the host and no temp file; renaming a directory on either side moves it on the other; `git status` in the guest copy answers in well under a second (with `core.autocrlf=true` the CRLF files show as modified); edit one file on both sides before a round: the newer is kept on both, `share status` lists the path and `glovd.log` has `live share conflict`; a host junction to a folder outside the share and guest symbolic links to `/etc/hostname` and to a guest directory are not copied either way, even after `glov share sync <vm> <name>` (`asked the agent of live share <name> for a full round`), and the junction's target is untouched; with `--exclude secret/`, a `secret/x` written in the guest stays out of the host's folder (the Guest Service holds it back; `go test ./internal/live -run 'Excluded|SpelledAnotherWay'` in `windows/` drives a guest that sends it anyway, in other cases and by 8.3 names, and the host refuses it); edit on the host while the VM is stopped, then `glov vm start`: a new agent copies the edit; `glov vm reboot`: the agent rewatches and keeps working; `mise run windows:install` again (glovd restarts): the agent logs `glovd went away` and `live share started` again within seconds and keeps working; `glov vm stop --force` with nothing changing: the agent exits within seconds with `VM <vm> stopped`, and `glov vm delete` then removes `%LOCALAPPDATA%\glov\vms\<vm>`; `--live --ro` is refused |
| Windows isolated VMs | with glov-broker and `glovd` installed, from a non-elevated shell: `glov vm create <vm>` (with or without `--isolated`) provisions with a `setting up the egress proxy` step; once it is `guest-ready`, `glov exec <vm> -- ip -br link` lists only `lo`, and `env` shows `HTTPS_PROXY=http://127.0.0.1:3128`; `glov egress list <vm>` says nothing is allowed and `curl https://example.com` fails with `CONNECT tunnel failed, response 403`; after `glov egress allow <vm> example.com '*.ubuntu.com'` the same `curl` gets 200 without a restart, and `apt-get update` succeeds; `curl --noproxy '*'` fails to resolve; the host's own address with a port, allowlisted, still gets 403 with `no public address` in `glov egress log <vm> --denied`; `glov egress remove <vm> example.com` makes the next `curl` 403 again; `glov ssh <vm> -- -G -A true` shows `clearallforwardings yes` and `forwardagent no`; after `glovd` restarts (`taskkill /im glovd.exe /f`, then any `glov` command), the running VM still gets out through the proxy. Secrets (non-elevated): `glov secret set anthropic --host api.anthropic.com` shows one `*` per character, then the length and ending, and stores only after `y`; `glov secret source gh --host api.github.com -- gh auth token` (from PowerShell: Git Bash rewrites arguments such as `/c`) runs `gh` as the user and `glov secret list` shows `command: C:\...\gh.exe auth token` and no value; `glov exec <vm> -- curl -s https://api.anthropic.com/v1/messages -H 'x-api-key: glov-secret:anthropic' ...` (with `api.anthropic.com` allowed) is an opaque tunnel (the provider sees the placeholder and answers `401`) until `glov secret grant <vm> anthropic`; then `glov-secret:other` gets a `403` naming it undefined, and the request gets the provider's answer (an `authentication_error` with a made-up value, a model reply with a real key and the cheapest model), and `glov egress log <vm>` shows it with `secrets=anthropic`; `curl -s http://example.com -H 'x: glov-secret:anthropic'` (allowed) is refused `403` with nothing sent; a host not bound to any granted secret is still an opaque tunnel (its own certificate); `glov secret revoke` makes the next request `403` again; after `glovd` restarts a granted request still succeeds, and signing the user out stops the VM (the Windows service row); the value appears in none of the guest's environment and files, `egress.log`, `glovd.log` or `glov` output |
| Windows egress modes and trust | with glov-broker and `glovd` installed, from a non-elevated shell, on an isolated VM: `glov egress allow <vm> <LAN or VPN host>` and `curl` from the guest is denied with `no public address` in `glov egress log <vm> --denied`; `glov egress allow <vm> <host> --private` (a `*.suffix` is refused) lists it with `(private)` and the same `curl` connects, while the host's own address stays refused; `glov egress remove` drops the mark. `glov vm configure <vm> --egress open` on the running VM prints the open line, `glov vm status` says `egress: open`, and `curl https://example.com` (not allowlisted) gets 200 with an `allow` line in the log; `--egress restricted` makes it 403 again; `--egress open --cpus 2` is refused. With a self-signed CA and a server certificate from it on a reachable host: `glov vm trust add <vm> ca.pem` on the running VM lists `<name>  CN=... until <date>  sha256 <16 hex>`, the guest has `/usr/local/share/ca-certificates/glov-trust/<name>.crt` and `curl https://<host>` verifies without `-k`; a second certificate file, a bad name and a non-PEM file are refused; for the proxy's side, serve a certificate from that CA for a DNS name resolving to a private address the host is not (such as `<a-b-c-d>.sslip.io` for another VM's address), allow it `--private` and bind a made-up secret to it: a request carrying the placeholder gets `502` with `upstream TLS: ... unknown authority` in the log until `vm trust add`, then `200` with `secrets=<name>` even with the guest's `glov-trust` directory emptied; `glov vm trust remove` takes it out of the guest at once, and a certificate added while the VM is stopped is there after `vm start`. After `glov vm harness install` froze the VM open with a trusted certificate, `--egress restricted`, `vm trust remove`, `vm stop`, `glov vm reset`, `vm start`: it is still restricted, `vm trust list` is empty and the guest has no `glov-trust` directory once `guest-ready` |
| Windows git sign-ins | with glov-broker and `glovd` installed, from a non-elevated shell, on an isolated Debian or Ubuntu VM with `github.com` and `api.github.com` allowed and `gh` installed in the guest: `glov secret source gh --forge github --host github.com --host api.github.com -- gh auth token` lists `(github sign-in)`, and `--forge gitea` is refused; after `glov secret grant <vm> gh` the guest has `/usr/local/bin/git-credential-glov`, `/etc/glov/git-sign-ins`, the `# glov git sign-in` block in `/etc/gitconfig` (lines you added there stay) and `/home/glov/.config/gh/hosts.yml` owned by `glov`, mode 0600, holding only the placeholder; `glov secret repos <vm> add github.com/<owner>/<repo>` (a private repository of yours), then as `glov`: `git clone https://github.com/<owner>/<repo>` and `git clone git@github.com:<owner>/<repo>.git` succeed, `git push` of a branch works, `gh pr list -R <owner>/<repo>` and `gh pr create` answer; another private repository of yours gets `403` naming `glov secret repos <vm> add` with a `deny` line `(repository github.com/<owner>/<other> is not allowed)` in `glov egress log <vm>`, as does `gh api graphql -f query='{ viewer { repositories(first: 1) { nodes { name } } } }'`; a public repository still clones; `glov secret revoke <vm> gh` removes the sign-ins line, the block and `hosts.yml` from the running VM at once; `glov secret repos <vm> rm` of a repository not listed is refused; the token appears in none of the guest's files, `egress.log`, `glovd.log` or `glov` output |
| Windows secret store | with glov-broker and `glovd` installed, from a non-elevated shell, a terminal driven by `hc run` (first line `Remove-Item Env:HC_TERMINAL_ID -ErrorAction SilentlyContinue`; `hc keys <t> a b c ...` types, `backspace` is a key) and a pwsh script that prints a made-up token (and exits 3 while a `fail` file next to it exists); disposable names, removed at the end. `glov secret set t-a --host example.test`: twenty letters and three Backspaces show 17 `*`, then `read 17 characters, ending …nopq. Use it? [y/N]`; `y` prints `stored secret t-a for example.test (17 characters, ending …nopq)`, `n` or Enter prints `glov: not stored`. A value with a space inside adds `; it has 1 whitespace or control character inside, which tokens never have` before the question; only spaces then Enter prints `glov: nothing was entered` and stores nothing. 70000 characters put into the console input (a `WriteConsoleInput` helper in the same pwsh; `hc keys` takes many minutes for them) end with `glov: over 64 KiB at the prompt; pipe the value on stdin instead` and store nothing, while the same piped says `glov: the value is over 64 KiB`; a short piped value is stored without a question. `glov secret list` prints `t-a`, `stored`, `example.test`, `17 characters, ending …nopq` tab-separated, and `--json` has `length` and `ending` and no value. `set` or `source` without `--host` exit 64 with the usage line, and a name with `/` is refused. `glov secret source t-s --host example.test -- pwsh -NoProfile -File <script>` prints `secret t-s comes from <absolute path of pwsh.exe> for example.test`, and `secret list` shows `command: <path> -NoProfile -File <script>` and no value. `glov secret refresh t-s` prints `refreshed secret t-s from <path>`; with the `fail` file it exits 1 with `glov: secret t-s: pwsh.exe failed (exit 3)`; `secret refresh t-a` says `secret t-a is not taken from a command: store it again with glov secret set t-a` and `secret refresh nope` says `no secret named nope`. On a running isolated VM: `glov secret grant <vm> t-s t-a` runs the script again (with the `fail` file it stops before granting anything) and prints both names, `glov secret grants <vm>` prints them again, `glov secret revoke <vm> t-a` prints `t-s` and revoking `t-a` again says `VM <vm> was not granted t-a`, `glov secret remove t-s` also drops it from `glov secret grants <vm>`, and `glov secret grant <vm> nope` says `no secret named nope: define it with glov secret set or glov secret source`. No value appears in any output, `glovd.log` or `egress.log` |
| Windows open mode | with glov-broker and `glovd` installed, from a non-elevated shell: `glov vm create <vm> --egress open` ends its provisioning output with `isolated: no network adapter; egress open: any public host through the egress proxy (vm configure --egress restricted for the allowlist)`; `glov egress list <vm>` prints `<vm>: egress open: any public host, through the egress proxy; the allowlist applies again after glov vm configure <vm> --egress restricted` (allowlist entries, if any, follow on their own lines), `glov vm status <vm>` has `egress: open (any public host, through the proxy)`, and `glov exec <vm> -- curl -s -o /dev/null -w '%{http_code}' https://example.com` prints `200` with an `allow example.com:443` line in `glov egress log <vm>`; `glov vm configure <vm> --egress restricted` brings the allowlist back (the same `curl` is `403`). In a project (a git repository without a remote is enough): `glov claude --egress open` creates the project VM open, `--egress restricted` on that VM exits 1 with `VM <vm> already exists: change its egress with glov vm configure <vm> --egress restricted`, and `--allow-plain-secrets` without `--egress open` exits 64 with `glov: --allow-plain-secrets needs --egress open`. The refusal that names `--allow-plain-secrets` (`VM <vm> would hold <names> as real values in tmpfs while open, and then any host could receive them; if that is acceptable, run glov ... --allow-plain-secrets`; with a terminal glov asks `[y/N]` instead) cannot be reached live: no shipped profile declares a `tmpfs` secret (see the Windows tmpfs secrets row) |
| Windows GitLab sign-ins | with glov-broker and `glovd` installed, from a non-elevated shell, on a project VM of `glov claude` (so it has the guest user `glov`) and a pwsh script that prints a made-up token as the source: `glov secret source gl --forge gitlab --host gitlab.example.test -- pwsh -NoProfile -File <script>` lists `gl` as `command: ... (gitlab sign-in)` for `gitlab.example.test`, and `--forge gitea` is refused with `forge gitea: use github or gitlab`; `glov secret grant <vm> gl` puts `gitlab.example.test gitlab gl` into the guest's `/etc/glov/git-sign-ins`, a `# glov git sign-in: begin` block with `[credential] helper = glov` and `[url "https://gitlab.example.test/"]` into `/etc/gitconfig`, and `/home/glov/.config/glab-cli/config.yml` owned by `glov`, mode 0600, with `host: gitlab.example.test` and a placeholder in place of the token (`grep -c` for the made-up token finds nothing); `glov secret repos <vm> add 'gitlab.example.test/grp/*'` lists `gitlab.example.test/grp/*`. Needs the owner's GitLab account (`glab auth login --hostname <host>` first, and `<host>` allowed): `glov secret source gl --forge gitlab --host <host> -- glab config get token --host <host>`, `glov secret grant <vm> gl`, `glov secret repos <vm> add <host>/<group>/<repo>`, glab into the guest (glov does not install it: `glov egress allow <vm> archive.ubuntu.com security.ubuntu.com`, `sudo apt-get install -y glab`, then `glov egress remove` both), then as `glov`: `git clone https://<host>/<group>/<repo>` and `git clone git@<host>:<group>/<repo>.git` succeed, `git push` of a branch works and `glab mr create` answers; another repository gets `403` naming `glov secret repos <vm> add` with a `deny` line in `glov egress log <vm>`; a `401` after a few hours is glab's expired OAuth token: `glab auth status` on the host, then `glov secret refresh gl`; `glov secret revoke <vm> gl` removes the sign-ins line, the block and `config.yml` at once |
| Windows harness config and records | with glov-broker and `glovd` installed, from a non-elevated shell, with a skill under `%USERPROFILE%\.claude\skills` and one under `%USERPROFILE%\.agents\skills` linked to a folder outside the profile: `glov vm create <vm> --harness claude` lists the live share `harness-sessions` at `/mnt/glov-sessions`; `glov vm harness add <vm> codex` on the running VM allows `api.openai.com`, prints `codex --version`, keeps an existing `~/.codex/config.toml` (`kept`), copies `~/.agents/skills` (the linked skill included, owned by `glov`) and links `~/.codex/sessions` into `/mnt/glov-sessions/codex/`, and a file written there in the guest reaches `%LOCALAPPDATA%\glov\sessions\<vm>\codex\sessions`; `glov vm harness install <vm> --harness claude` copies the claude config without `.credentials.json`, links `~/.claude/projects` and freezes the VM; `harness add` on it is refused as frozen; write a file in the guest's records, `vm stop`, `vm reset`, `vm start`: the file is on the host and back in the guest, and the agent log shows a full round with `toGuest=1`; again with the agent suspended (`NtSuspendProcess`) from before `vm stop` until after `vm start`: on resuming, the same agent logs `VM reset: taking the share again`, then `live share started` and a full round, and the file is on both sides; `glov vm delete` keeps `%LOCALAPPDATA%\glov\sessions\<vm>`; the records are counted: `glov vm status <vm>` has `conversation records: %LOCALAPPDATA%\glov\sessions\<vm> (0 files, 0 KiB)` and, after the guest writes a 3000-byte file under `~/.claude/projects/x/`, `(1 files, 3 KiB)`; `glov vm create <vm> --harness claude --no-sessions` makes no `harness-sessions` share (`glov share list <vm>` says `no shares`), `glov vm status` has no records line, and `glov vm delete` prints no `kept its conversation records` line; deleting a VM that has the share (`glov vm stop`, `glov vm delete <vm> --yes`) prints `deleted <vm>` and then `kept its conversation records at <path>; remove them yourself when no longer needed` |
| Windows glov claude | with glov-broker and `glovd` installed, the `anthropic` key stored, in a terminal driven by `hc run`, in a fresh git repository of `samples/HelloFramework` with an `origin` on a forge: `glov claude` creates an isolated VM `<folder>-<6 hex>` (`glov vm status` shows `project: <path> (through git)` and `conversation records: %LOCALAPPDATA%\glov\sessions\<vm>`, inside glovd's data directory, which only that share may be), grants `anthropic`, lists the remote's repository as `new:` and asks; `y` prints `may reach`, and `glov secret repos <vm>` lists it. It installs claude without freezing, asks to add `Include "~/AppData/Local/glov/ssh_config"` to `~/.ssh/config` (`n` writes `ssh_config.declined`, and the file is unchanged), adds the remote `glov-<vm>` (`tagOpt --no-tags`), prints `pushed main to the guest`, and starts claude in tmux; the guest repository at `/mnt/<drive>/<path>` is on `main` with your identity, `commit.gpgsign=false` and the same `origin`. Ctrl-b d detaches and glov exits 0; running it again attaches the same session. Without a terminal (`--version` as the argument): a guest commit comes back as `glov-<vm>/main is 1 commit ahead`, a host commit on top of it is pushed, and one on both sides gives `have diverged` and pushes nothing; `glov vm fetch <vm>` fetches while it runs and is refused for a VM that is not a project's. With claude left running in the session, a host commit and `glov claude --new -- --version` print `not pushed: claude runs in /mnt/<drive>/<path>` and the guest's HEAD stays; with only a shell idle in that folder (`glov ssh <vm>`, `cd` there) the next run pushes. `glov vm harness install <vm>` and `vm.freeze` refuse to freeze it |
| Windows glov claude --herdr | with glov-broker and `glovd` installed, hc 0.1.20 or later and herdr-app on PATH, `~/.ssh/config` without glov's `Include`, in a terminal driven by `hc run` with `HC_TERMINAL_ID` removed, in a fresh git repository of `samples/HelloFramework`: `glov claude --herdr --sync git`, answering `n` to the git `Include` question, prints the herdr warning, asks to add the machine (`y`), and `hc machine ls` shows it with `via` and target `"<glov.exe>" ssh <vm> --`; the terminal attaches to claude in `/mnt/<drive>/<path>`; ctrl+\ detaches and a guest commit comes back as `glov-<vm>/main is 1 commit ahead`; `git fetch glov-<vm>` works from Git Bash and PowerShell. After `glov vm stop` the next run says it waits for herdr-core, says `<vm>/claude was left at a shell`, and attaches to a new claude; a run without a restart attaches the same one. After `glov vm reboot`, with `top` typed into that shell, the next run says `runs top, not claude` and attaches it; after quitting top the next run starts claude again. From a herdr-app pane: the first run opens a space `<folder> (vm)` on the machine with the folder at the guest path, the next shows the running session, after `glov vm reboot` (or claude exited in the tab) the next replaces the tab with a new `claude` tab in the same space, and `--new` adds `claude-2` there. A commit made in the guest after that comes back with `glov vm stop` (`fetched <vm>'s branches`), and `glov vm delete` then lists it; `vm stop --force` does not fetch; after a guest commit (`sync` in the guest) and `vm stop --force`, `glov vm delete <vm>` in a terminal offers to start it, Enter starts, fetches (`fetched <vm>'s branches`), stops and lists that commit, and `--yes` only warns that later commits are not listed; a running project VM is refused with `glov vm stop <vm> first`. With hc older than 0.1.20 it fails with `--herdr needs hc 0.1.20 or later`. `glov vm delete` (stopped, `--yes`) prints `removed herdr-core machine <vm>`, the machine and its space are gone, and `ssh_config` has no `glov-<vm>` |
| Windows vm land | with glov-broker and `glovd` installed, on a project VM of `glov claude` and a project repository that signs (`gpg.format ssh`, `commit.gpgsign true` and a test `user.signingkey`): a guest commit, then `glov vm land <vm>` without a terminal lists `<n> commits for main` with author and a stat line and refuses without `--yes`; in a terminal driven by `hc run` it asks `make them again on main as your commits?`, and `y` prints `main is yours now`; `git log --format=%G?` shows `G` on the landed commits, the guest's `git log` has the same hashes and a clean tree, and no `refs/glov-land/` ref remains. A guest edit and a host edit of the same line stop the cherry-pick (`the cherry-pick stopped`), `vm land` meanwhile refuses with `a cherry-pick is under way`; resolve and `--continue`, commit once more in the guest, and `vm land` reports `the guest did not take it back` (`stale info`); the next `vm land --yes` lands only the new commit, and the guest ends on the local hashes. With an unlanded guest commit fetched, `glov vm delete <vm>` without a terminal lists `glov-<vm>/main: 1 commit main lacks` and does not delete; `--yes` deletes and removes the remote and the `Host glov-<vm>` block, and keeps `%LOCALAPPDATA%\glov\sessions\<vm>` |
| Windows glov claude live | with glov-broker and `glovd` installed, in a fresh git repository of `samples/HelloFramework` with `core.autocrlf=true`: `glov claude --sync live` warns, creates a VM whose `glov vm status` shows `project: <path> (shared live)`, waits for the share `project` to be in step and starts claude; the guest has `core.autocrlf=true` and a clean `git status`. A host commit shows in the guest's `git log` and a guest commit in the host's within seconds. With a `.git/<x>.lock` file held in the guest, a host commit does not reach the guest, and after a minute `glov share status` prints `waiting for git: the guest holds .git/<x>.lock`; a guest commit meanwhile, then removing the lock, gives `.git/refs/heads/main: kept the <side>'s ... branch main-glov-conflict-<other>` and that branch on both sides. In a terminal driven by `hc run`, in a `git worktree` of it, `glov claude` asks `[git/live]`, and `live` adds the shares `project` and `project-git`; the guest's `.git` names `/mnt/<drive>/<repo>/.git/worktrees/<name>`, the host's `.git` is unchanged, and a guest commit reaches `git worktree list` on Windows. In a folder outside git without a terminal, `glov claude` is refused, and `--sync git` on a live VM says a VM keeps its mode |
| Windows glov codex, agy and agent | with glov-broker and `glovd` installed, the `anthropic` and `openai` keys stored (`gemini` not), in a terminal driven by `hc run` (detach with `hc keys <t> ctrl+b` then `d`), in a fresh git repository of `samples/HelloFramework` without a remote, a pwsh script printing a made-up token as the source secret `src`, and `glov claude` answered `git` at its first question: `glov secret grant <vm> src`, then `glov codex` prints `<vm>: setting up OpenAI Codex CLI`, `added codex, allowed api.openai.com`, a `harness codex: secret openai ... is defined, not granted` line, the install (`codex-cli <version>`), `install hosts removed`, `copied ~/.agents/skills (N files, X MiB)` when the host has that folder, `granted openai to <vm>` and `starting codex (detach with Ctrl-b d)`; `glov codex` again and `glov agent codex` print `attaching codex`; `glov claude --new` prints `starting claude-2` and `glov exec <vm> --as user -- tmux -L claude list-sessions` lists `claude` and `claude-2`; `glov agy` (the same as `glov agent antigravity`) installs antigravity, copies `~/.gemini/GEMINI.md` and `~/.gemini/antigravity-cli/skills`, prints `not stored yet: gemini; glov secret set <name> --host <host>, then glov secret grant <vm> <name> (or glov secret login <vm> antigravity for a subscription)` and still starts the session `antigravity`. Every start re-runs the command of each source secret the VM holds: with the script's `fail` file in place `glov claude` prints `warning: secret src: pwsh.exe failed (exit 3); src keeps its old value` and still attaches; without it, no warning (a profile's own secret that is a failing source stops the first grant instead, `glov: secret <name>: <program> failed (exit N)`). Host config: the first `glov claude` copies `~/.claude/CLAUDE.md` and `~/.claude/skills` (`copied ~/.claude/skills (N files, X MiB)`, links followed); an edit of `~/.claude/CLAUDE.md` in the guest (`glov exec <vm> --as user -- sh -c 'echo x >> ~/.claude/CLAUDE.md'`) changes the guest file's `sha256sum` and not the host file's; `glov vm harness install <vm> --harness claude` again (the project VM stays up, it is not frozen) makes the two equal, because the copy is replaced whole; `glov vm harness add <vm> codex --no-host-config` on a VM that has claude prints `host config not copied (--no-host-config)` and leaves no `~/.agents` (`glov claude` itself has no such flag). In a second repository with `origin` set to a made-up `https://github.com/<owner>/<repo>.git`: `glov claude --allow-remotes --egress open --sync git` lists `new: github.com/<owner>/<repo>`, prints `<vm> may reach github.com/<owner>/<repo>` without asking, `glov secret repos <vm>` lists it, and `glov egress list <vm>` says `egress open` and lists `api.anthropic.com`. Delete both VMs and their `%LOCALAPPDATA%\glov\sessions\<vm>` folders |
| Windows tmpfs secrets | `cd windows && go test ./internal/daemon -run Tmpfs -count=1` passes (`TestARunningVMGetsExactlyItsGrantedTmpfsSecrets` for what the guest is sent on connect, grant and revoke, `TestOpeningAVMThatHoldsTmpfsSecretsNeedsTheOwnersSayAndWithholdsTheRest` for open mode) and `mise run guest:test` covers the Guest Service's side. Live, only with a build whose `windows/internal/harness/profiles/claude.json` temporarily declares a secret with `"delivery": "tmpfs"` (the shipped profiles are macOS's byte for byte and declare none; do not commit that) on a running isolated VM made with `--harness claude` and the secret stored with a made-up value: before `glov secret grant <vm> <name>`, `glov exec <vm> -- ls /run/glov/secrets` lists nothing; after it, `stat -c '%a %U' /run/glov/secrets/<name>` prints `400 glov` and `findmnt /run/glov/secrets` shows a `tmpfs`; after `glov vm reboot <vm>` the file is back once guest-ready; `glov secret revoke <vm> <name>` removes it at once; `glov vm configure <vm> --egress open` without a terminal is refused naming `--allow-plain-secrets`, with a terminal glov asks `[y/N]` instead (`n` keeps the allowlist); with the flag, or `y`, the file stays, while `glov secret grant <vm> <second>` for a second tmpfs secret is refused the same way naming `glov secret grant <vm> <second> --allow-plain-secrets`, and with that flag the second file appears; a recursive `grep -rl` for the value in the guest outside `/run` and `/proc` finds nothing (build the search string from two parts, `n=<first>; n=$n<rest>`: run through `sudo`, the command line itself lands in `/var/log/auth.log` and the journal) |
| Windows subscriptions | with glov-broker and `glovd` installed, from a non-elevated shell, on an isolated VM made with `--harness claude --harness codex` and guest-ready: `glov secret login <vm> claude` prints the paste hint, reads the token masked and prints `signed in to the subscription`; `glov secret list` shows `<vm>.claude.token` for `api.anthropic.com`; `/etc/glov/harness-subscriptions` holds `claude`, `/etc/environment` has `CLAUDE_CODE_OAUTH_TOKEN=glov-secret:<vm>.claude.token` and no `ANTHROPIC_API_KEY`, and `claude -p` as user `glov` answers with the subscription; `glov secret login <vm> codex` runs `codex login --device-auth` in the terminal, and after the browser sign-in `glov secret list` shows `<vm>.codex.access` and `<vm>.codex.refresh`, `~/.codex/auth.json` and `glov egress log <vm>` hold only placeholders, `~/.codex/config.toml` has `model_provider = "glov-chatgpt"` with the owner's other edits kept, and `codex exec` answers; `glov secret logout <vm> codex` runs `codex logout` in the guest, removes both secrets, the grants and the runtime hosts no other harness needs, and the guest is back on `CODEX_API_KEY`; a VM stopped at login is switched when it next starts; after `vm harness install` and a login, `glov vm reset` keeps the subscription; no token appears in the guest's files, `egress.log`, `glovd.log` or `glov` output; with a made-up token piped into `glov secret login <vm> claude` (no browser is needed for it) `/etc/glov/harness-subscriptions` holds `claude` and `/etc/environment` has `CLAUDE_CODE_OAUTH_TOKEN=glov-secret:<vm>.claude.token`, both again after `glov vm reboot <vm>`; `glov secret logout <vm> claude` prints `claude signed out of the subscription; it uses its API key again`, `glov secret list` has no `<vm>.claude.*`, `/etc/glov/harness-subscriptions` is gone and `/etc/environment` has `ANTHROPIC_API_KEY=glov-secret:anthropic` again. Needs the owner (a browser sign-in): `glov secret login <vm> antigravity` runs the sign-in in the VM in the terminal, and after it `glov secret list` shows `<vm>.antigravity.access` and `<vm>.antigravity.refresh`, `~/.gemini/antigravity-cli/antigravity-oauth-token` and `glov egress log <vm>` hold only placeholders, and `agy -p` answers; for the forced refresh set `expiry` in that file to a past time (codex: `last_refresh` in `~/.codex/auth.json` to more than 8 days ago) and run the one-word prompt again: `glov egress log <vm>` shows the token endpoint call (`oauth2.googleapis.com`; codex `auth.openai.com`) with the secrets' names only, and the file still holds placeholders; after `glov vm reboot <vm>` the prompt answers again with no new sign-in; `glov secret logout <vm> antigravity` removes the token file and both secrets, and the next prompt fails: agy is back on the `GEMINI_API_KEY` placeholder, which only a granted `gemini` secret makes work |
| Windows harness VMs | with glov-broker and `glovd` installed and the keys stored by the owner (`glov secret set anthropic --host api.anthropic.com`, `openai --host api.openai.com`, `gemini --host generativelanguage.googleapis.com`): `glov vm create <vm> --harness claude --harness codex --harness antigravity` prints `trusting the egress CA` and `setting up the harnesses` steps and a `harness <name>: secret <provider> ... is defined, not granted: glov secret grant <vm> <provider>` line each; after those grants, once it is `guest-ready`, `glov vm harness install <vm>` waits for the console sign-in, lists the install hosts it allowed, prints each `<command> --version`, and ends with only the runtime hosts allowed (`glov egress list`); Ctrl+C during it also removes them; `loginctl list-sessions` shows `glov` on `tty1` and `systemctl is-active serial-getty@ttyS0` is `active`; then per harness, as the user with the cheapest model and a one-word prompt, `claude -p --model <model> ...`, `codex exec --skip-git-repo-check -m <model> ...` (codex refuses to run outside a git repository without it) and `agy -p ...` each answer, with `secrets=<provider>` on its provider's line in `glov egress log <vm>` and the optional hosts it tries denied; `grep -r glov-secret` finds the placeholders in the guest and no value |
| Windows Guest Service updates and stats | with glov-broker and `glovd` installed and a VM running whose Guest Service differs from `.build/windows/glov-guest-linux-amd64` (build that file with another `-X main.version` and run `glov host install` again): `glov vm status <vm>` ends in `update available` and `glov vm guest check <vm>` says `update-available`; with `glov exec <vm> -- sleep 60` running, `glov vm guest update <vm>` is refused naming `1 exec job`, and `--force` updates while that `glov exec` exits 1 saying it lost track of the command; `/proc/sys/kernel/random/boot_id` is unchanged and a second update says the build is already installed. `glov vm stats` shows each running VM, `glov vm stats <vm> --json` too; with two guest CPUs busy (`timeout 10 sh -c "while :; do :; done"` twice in one `glov exec`) the VM's CPU reads about 200%. `glov guide` and `glov guide troubleshooting` print `skills/glov` |
| Windows MCP | with glov-broker and `glovd` installed, drive `glov mcp` from an MCP client over stdio (for example the Go SDK's `CommandTransport`): `tools/list` has the 20 tools of `docs/install.md`'s Windows list and none of screenshot, input or snapshot; on a VM made with `glov vm create` and a share added with `glov share add`: `vm_start`, `vm_wait guest-ready`, `exec` (exit code, last 50 lines, line count; with `timeout` 1 on a slower command it returns a job id), `exec_start`/`exec_read`/`exec_kill`, `file_write` then `file_read` (a file over 256 KiB is refused), `copy_to_vm` and `copy_from_vm` through `%LOCALAPPDATA%\glov\scratch` (a host path outside shares and scratch is refused), `share_sync`, `vm_stop` and `vm_configure`; then `glov vm delete <vm> --yes` |
| Guest Service (Go) | on Linux or macOS, `mise run guest:test` (`go vet` and `go test` in `guest/`), then `mise run guest:check-windows-arm64` |
| Guest Service on Windows | `GOOS=windows GOARCH=arm64 go test -c -o x.test.exe ./internal/<pkg>`, copy it into the VM's `C:\glov-dev\` and run it there; `glov-guest.exe --stdio` serves the protocol over SSH stdin/stdout. `as: user` needs LocalSystem: run the test binary from a one-shot `schtasks /ru SYSTEM` task, never by installing the service |
| VM control | `.build/debug/glov vm start <vm>`, `glov exec <vm> -- powershell.exe -NoProfile -Command ...`, `glov vm stop <vm>`; with `--ssh`, `glov ssh <vm> -- hostname` |
| Windows install | see "Windows guest end to end" below |
| Guest Service update | see "Updating the Guest Service on an existing VM" below |
| Daemon and CLI | `.build/debug/glovd` in one shell, then `.build/debug/glov vm list`, `vm start <vm>`, `screenshot <vm> -o out.png`, `vm stop <vm>` (VMs live under `~/Library/Application Support/glov/<name>/vm.json`, or `$GLOV_HOME`) |
| MCP | `.build/debug/glov mcp` speaks MCP over stdio; `tools/list` names the tools |
| Snapshots in a Windows guest | through MCP: `snapshot_create`, change the guest (a failing `exec`), `snapshot_revert`, `vm_wait guest-ready`; the change is gone and the guest's UTC time matches the host's. Then `vm_stop`, `vm_configure` with `balloon` flipped, `snapshot_revert`: refused with `was taken on another machine`, naming `balloon device`; flip it back and the revert works |
| Viewer | `.build/debug/glov view <vm> --log-fps` against a running daemon: a window opens and stderr prints frame rate, frame-payload throughput and frame/input RPC timing every 3 s; quitting it leaves `glov vm status <vm>` at `running`. `--read-only` forwards no input; Input menu has Ctrl+Alt+Delete. Mapping and connection-generation logic are covered by `swift test` (`GlovViewerTests`) |
| Remote client | See `docs/remote.md`: `GLOV_TEST_SSH_HOST=<same-host SSH alias> swift test --filter RemoteCLITests` uses an isolated daemon and fixture agent over real SSH. Two-Mac Windows/Linux desktop interaction and network conditions require separate deployed-environment evidence; the same-host check does not establish them. |
| QEMU port forwards | on your own root and daemon, `qemu-img create -f qcow2 <root>/t.qcow2 64M`, `vm create t --disk <root>/t.qcow2 --firmware-code <edk2 code> --no-tpm --forward 18080:80`, `vm start t`: `nc -z 127.0.0.1 18080` connects while it runs (QEMU binds forwards at start, no guest OS needed) |
| Linux (VZ) guest | see "Linux guest end to end" below |

### Windows guest end to end

Use your own root and daemon, on a disk with room (a finished guest takes about 25 GiB, more with VS Build Tools);
the root path must stay short (socket paths).

```sh
export GLOV_HOME=<short path>/h GLOV_CACHE=<cache with room>
swift build
.build/debug/glovd --home "$GLOV_HOME" &
(cd guest && GOOS=windows GOARCH=arm64 go build -o ../.build/glov-guest.exe ./cmd/glov-guest)
.build/debug/glov vm create win --no-start --guest-service .build/glov-guest.exe      # add --ssh for OpenSSH
.build/debug/glov vm start win
.build/debug/glov vm wait win guest-ready --timeout 3600
```

- `vm create` reuses the cached ISO and virtio-win (`glov host media`), else downloads and builds them first. It writes
  `disk.qcow2`, `vars.fd`, `secrets/admin-password` (0600), `secrets/unattend.cdr` and, with `--ssh`,
  `secrets/id_ed25519`; vm.json lists the ISO, virtio-win and the unattend image as `extraDrives` and under `install`.
- `vm start` boots EDK2, which offers the CD, times out and runs the UEFI shell; 30 s after the start
  (`InstallTiming.windows`) the daemon types `fs0:\efi\boot\bootaa64.efi`, Enter, and Space for "Press any key to boot
  from CD or DVD". A screenshot shows Setup within a minute; it reboots several times.
- `vm status` says `installing` with an `install:` line (`booting the installer`, then `Windows Setup`, then
  `first sign-in and setup script` once the Guest Service answers) until `setup.ps1` has written `C:\glov-setup.done`, then
  `running`; `vm wait` prints `waiting for …`, each new phase and a `still` line every minute on stderr, and the media
  build logs `still …` every 30 s during the ESD check, the wimlib export and hdiutil; `vm wait guest-ready` (and MCP `vm_wait guest-ready`) returns only then, or fails with the reason once the
  install fails. The install drives are unplugged, gone from vm.json, and `secrets/unattend.cdr` is deleted. The next start
  has no `usb-storage` in its QEMU command line. A full install takes 10 to 15 minutes; after 60 minutes, or if the VM
  stops first, it is `install-failed`, `secrets/unattend.cdr` is deleted too, and `vm start` refuses it: `glov vm delete <vm>` and create it again.
- In the guest: `C:\glov-setup.log` (setup's transcript), `glov-guest` running as a service, no
  `C:\Windows\Panther\unattend*.xml`. With `--ssh`, key auth works with `secrets/id_ed25519` and password auth is refused
  (`ssh -o PubkeyAuthentication=no -o PreferredAuthentications=password` gets `Permission denied`).
- `glov vm delete <vm>` refuses a running VM, asks before deleting (`--yes` skips it) and removes the VM directory;
  the cache stays.

### Updating the Guest Service on an existing VM

`tools/build.sh [debug|release]` builds the host plus Windows and Linux arm64 Guest Services under the host executable's
`guest-services/<os>-arm64/` directory. Each manifest records `tools/version.sh` (the VERSION file plus `+<git describe --always --dirty>`) and the binary's SHA-256.
`glov host install` validates and copies both guest builds alongside its installed daemon; repeat it after rebuilding.
Plain `swift build` remains useful for host-only iteration but does not refresh bundled Guest Services or build
`glov-image`, without which `vm create --engine vz` fails.

A VM keeps its installed Guest Service. Once per connection, the daemon checks it against its own bundled binary in the background;
CLI and MCP VM status expose `guestService.update`. A missing bundle, stopped VM or unavailable Guest Service is `unknown` with a reason.
A check does not stage or run the target binary, and there is no automatic installation or online release lookup.

```sh
tools/build.sh debug
.build/debug/glov host install
.build/debug/glov vm guest check <vm>
.build/debug/glov vm guest update <vm>
# Custom build override (check also accepts --guest-service):
.build/debug/glov vm guest update <vm> --guest-service .build/glov-guest.exe
```

The daemon refuses an update while managed jobs run, listing their IDs. `--force` permits their interruption and cancels a
share sync in progress. An exec start or share mount in progress must finish even with `--force`; new starts and syncs are
blocked until the update finishes.
The daemon serializes the update with VM start/stop and snapshots. Only glov-managed work is covered by the busy check.
For isolated verification, use a separate `GLOV_HOME` and daemon instead of installing over the normal daemon.

It reads the binary's header (PE or ELF, arm64) and refuses one for another OS or CPU before copying, stops with the reason
when the VM is stopped, still installing or has no ready Guest Service, and does nothing when the installed file already has the
binary's SHA-256. Otherwise it copies the binary through `<home>/scratch` into a root/SYSTEM-only place in the guest
(`/usr/local/bin/glov-guest.next`, where exec is allowed unlike a `noexec` `/run`; `C:\ProgramData\glov\update`), runs `<staged> --version`, then starts `install` detached
(stopping the Guest Service ends the `exec` that asked for it): a one-shot `schtasks /ru SYSTEM` task on Windows (which swaps the exe while
the service is stopped and rolls back on failure), `systemd-run --no-block` on Linux (which restarts the unit). It then waits for
a Guest Service that restarted to report the staged binary's version from the installed file with that SHA-256, and fails saying so
when the old version still answers or the file was not replaced. Give each build a distinct `-X main.version`; builds left at
`dev` are told apart by the SHA-256 and the observed restart. Check by hand: `glov exec <vm> -- /usr/local/bin/glov-guest --version`
(Linux), or the last "starting version=" line of `C:\ProgramData\glov\guest-service.log` (Windows).

### Linux guest end to end

Use your own root and daemon so a running daemon is never touched; the root path must stay short (socket paths).

```sh
export GLOV_HOME=~/Library/Caches/glov/<your-test-home>
tools/build.sh debug && tools/linux-vm/sign-daemon.sh   # also builds glov-image, which converts and verifies the image
.build/debug/glovd --home "$GLOV_HOME" &          # your daemon, on your root
(cd guest && GOOS=linux GOARCH=arm64 go build -o ../.build/glov-guest-linux ./cmd/glov-guest)
.build/debug/glov vm create ubuntu --engine vz --no-start --guest-service .build/glov-guest-linux   # or --image <arm64 cloud image>
.build/debug/glov vm start ubuntu
```

- Boot: `<vm>/console.log` ends with `glov: first boot done, addresses: <ip> ...` (cloud-init's last `runcmd`)
  and a `login:` prompt on `hvc0`. The IP is also in `/var/db/dhcpd_leases` under the VM's `macAddress`.
- SSH: `glov ssh ubuntu -- cloud-init status` says `done`.
- Guest Service: `vm_wait guest-ready` reports `os: linux`; then `exec`, `file_write`/`file_read`, `copy_to_vm`/`copy_from_vm`
  through `glov mcp`.
- Guest Service status (heartbeat): poll `vm_status`. An SSH session as `glov` shows as `user` within about 5 s, and goes
  after it ends. `exec` `systemd-run --on-active=25 systemctl kill -s CONT --kill-whom=main glov-guest`, then
  `exec_start` the same with `STOP`: `unresponsive` (with `os`, no `user` or `bootId`) within 13 s, `exec` fails at once
  with "Guest Service not responding", and after the CONT it is `ready` with the same `bootId` and an `exec_start` job
  started before the STOP keeps its output. A `systemctl reboot` shows `connecting`, then `ready` with a new `bootId`.
- Snapshots: through `glov mcp`, `snapshot_create` while it runs (live: about 1 s for an idle 4G guest, 2 s with 2 GiB
  in use), `exec` a marker file, `type` at the `login:` prompt, `snapshot_revert`, `vm_wait guest-ready`: the marker is
  gone, the guest's UTC time matches the host's, and a screenshot shows the prompt as it was at the snapshot (input works
  again). Then `vm_stop`, `snapshot_create` (disk only), boot and change the disk, `vm_stop`, `snapshot_revert`, boot: the
  change is gone; reverting it while running is refused. `snapshot_revert` of the live one on the stopped VM starts it
  into the snapshot. `snapshot_list` shows `live` and `disk only`; `snapshot_delete` removes `<vm>/snapshots/<name>/`.
- Screen and input: `screenshot <vm> -o out.png` is the 1280x800 `tty1` console with its `login:` prompt; `type` and
  `key enter` through `glov mcp` show at that prompt (`ctrl+alt+f2` switches to `tty2`). The text console ignores the
  pointer, so watch it in the guest: `exec` a reader of `/dev/input/event2` ("Apple Inc. Virtual USB Digitizer", see
  `/proc/bus/input/devices`) while calling `move`/`click`/`scroll`; `move` to (640, 400) of a 1280-wide screenshot reads
  `ABS_X 16396`, the value QEMU's tablet gets, and each `scroll` step is one `REL_WHEEL`.
- Shares: `glov share add ubuntu rwdir <host dir>` (guest path `/mnt/rwdir`) and `share add ubuntu rodir <host dir> /srv/rodir --ro`
  on the stopped VM, `vm start`, `vm wait guest-ready`; `vm status` lists both as `mounted`. A file written on either side
  shows on the other (`glov exec ubuntu -- cat /mnt/rwdir/f`), a write to `/srv/rodir` fails with a read-only error, and
  after `vm stop`, `vm start` both are mounted again. `share sync` and `share add --exclude` are refused.
- App Nap: while the VM runs, `pmset -g assertions` lists your `glovd` with "glov is running a virtual machine";
  it is gone after `vm stop`.
- Stop: `vm stop` (ACPI, then forced after 60 s) and the next boot's `journalctl -b -1` ends with `Journal stopped`.
  SIGTERM to the daemon powers its VZ guests down first (15 s), because they run inside it.

### Isolated Linux VM (`--isolated`)

On your own root and daemon, as above: `glov vm create <vm> --engine vz --isolated --guest-service .build/glov-guest-linux`,
`vm start`, `vm wait guest-ready` (about 8 s on Ubuntu 26.04; a 2-minute boot means `network-online.target` waited for
a link, see `CloudInitSeed.networkConfig`).

- `glov exec <vm> -- ip -br link` lists only `lo`; `env` shows the six proxy variables; `/etc/environment` and
  `/etc/apt/apt.conf.d/90glov-proxy` have them; `cloud-init status` is `done`, not `degraded`.
- With `egress.allow` empty, `curl https://example.com` fails with 403. Add `example.com` to vm.json without
  restarting: the next `curl https://example.com` and `curl http://example.com/` succeed. `https://example.com:8443`,
  another host, `--noproxy '*'`, the NAT gateway `192.168.64.1` (also when allowlisted as `192.168.64.1:80`: "no
  public address") and `1.1.1.1` fail. With `*.ubuntu.com` allowed, `apt-get update` works.
- A guest `AF_VSOCK` connect to CID 2 is reset on every port but 4024 (try 4022, 4023, 4025).
- SSH over vsock: `glov ssh <vm> -- uname -a` works with no network device. In `glov ssh <vm>`, a full-screen TUI
  (`htop`, `vim`, `claude`) redraws when the terminal is resized, keeps its colors, and Ctrl-C reaches the program, not
  the session. With an agent running on the Mac (`ssh-add -l` lists a key),
  `glov ssh <vm> -- -R 9000:localhost:22 -A -Y` on an isolated VM still gets no forward (`ss -ltn` in the guest shows
  nothing on 9000, `SSH_AUTH_SOCK` and `DISPLAY` are unset). A VZ VM with a NIC answers `glov ssh` the same way.
- The daemon log has one `<vm>: egress allow|deny <host>:<port> in=... out=...` line per attempt, and `glov egress log <vm>`
  the same records.
- Live policy (`glov egress`): with `glov exec <vm> -- sh -c 'while :; do curl -s -o /dev/null -w "%{http_code}\n" https://example.com; sleep 1; done'`
  running, `glov egress log <vm> --follow` shows `deny example.com:443`; `glov egress allow <vm> example.com` turns the
  loop's output to 200 and the log to `allow` without a restart, and `glov egress remove` turns both back. `glov egress list`
  on a VM that is not isolated fails saying so.
- Model APIs: `glov exec <vm> -- curl -s https://api.anthropic.com/v1/messages -H 'x-api-key: glov-secret:anthropic' ...`
  (with `api.anthropic.com` allowed) gets a `403` naming `glov secret grant` until the secret is granted, then the
  provider's answer: a model reply with a real key, an `authentication_error` from the provider with a made-up one
  (it was forwarded with the substituted value). `glov egress log <vm>` shows the request with `secrets=anthropic`.
  Afterwards, grep the VM disk image, `<vm>/` and the daemon log for the key: nothing.
- Project harness, git mode (`glov claude`): in a throwaway git repo below `/Users` (a copy of `samples/HelloFramework`
  with an `origin`, never a real project), `glov claude --sync git -- --version` in a terminal (`script -q /dev/null`)
  prints the project and VM name, creates the VM with only the `harness-sessions` share, offers the `Include`, adds the
  remote `glov-<vm>`, says `pushed main to the guest` and prints the harness version inside tmux. In the guest
  (`glov exec <vm> --as user`), the repository at the same path has the commit, `commit.gpgsign false` and owner `glov`;
  a commit there comes back with `glov vm fetch <vm>` (or the next `glov claude` once the session returns) as
  `refs/remotes/glov-<vm>/main`, unsigned (`%G?` is `N`), with the Mac's working tree unchanged. `--sync live` on that VM
  is refused (`a VM keeps its mode`); outside git, `glov claude` without `--sync live` is refused. In a herdr-app pane
  (`hc send`/`hc keys` drive it): with the harness left running in one pane, a Mac commit and `glov claude --new` in
  another print `not pushed: claude runs in <path>` and the guest's HEAD stays; once the session ends, the next run
  prints `pushed main to the guest`. `kill -TSTP` on the ssh child (what `~^Z` does) stops glov too (`suspended` in the
  shell) and `fg` resumes both. A repository with `git lfs install --local` (pre-push hook) and an LFS file pushes, the
  guest holding the pointer. After a guest commit and `systemctl poweroff` in the guest (not `vm stop --force`, which
  loses unsynced guest writes), `glov vm delete <vm>` in the pane offers `start it to fetch them first? [Y/n]`; Enter
  starts, fetches, stops and lists the commit.
- Project harness, vm land: on that git-mode VM, with the throwaway repository signing (`gpg.format ssh`,
  `commit.gpgsign true` and a test `user.signingkey`) and a Mac commit the guest lacks, a guest commit, then
  `glov vm land <vm>` without a terminal lists `<n> commit(s) for main` with author and a stat line and refuses without
  `--yes`; `--yes` prints `main is yours now`, `git log --format=%G?` shows `G` on the landed commit, the guest's
  `git log` has the same hashes and a clean tree, and no `refs/glov-land/` ref remains. The conflict, a guest that
  committed meanwhile, and merge commits are covered by `ProjectRepoTests`. Then a guest commit, `glov vm stop <vm>`
  prints `fetched <vm>'s branches`; `glov vm delete <vm>` without a terminal lists `glov-<vm>/main: 1 commit main lacks`
  and does not delete; `--yes` deletes, prints `removed the glov-<vm> remote`, and leaves no `refs/remotes/glov-<vm>/`
  or `refs/glov-land/` ref and no `Host glov-<vm>` block in `ssh_config` in the glov home.
- Project harness, live mode (`glov claude --sync live`): in a throwaway git repo below `/Users` (a copy of
  `samples/HelloFramework`, never a real project, since the guest can write it), `glov claude --sync live -- --version`
  warns, prints the project and VM name, creates the VM
  with the shares `project` (at the repo's own path) and `harness-sessions`, installs the harness and prints its version
  inside tmux. A second `glov claude` reuses the VM. A file the guest creates in the project appears on the Mac. With a
  stored key or subscription: detach with Ctrl-b d, `glov exec <vm> --as user -- tmux ls` still lists the session, the
  same command attaches it again, `--new --model <model>` adds `claude-2` running `claude --model <model>`, and
  `glov claude -c` starts `claude -c` (`ps -u glov -o args=` in the guest). Then `glov codex` and `glov agy` in the same
  project add each to that VM (`harnesses` in vm.json, its runtime hosts in `glov egress list`, `--version` after the
  install) and run it interactively once signed in, while the claude session keeps running.
- Last checked 2026-10-31 on Ubuntu 26.04.

### Reboot (`glov vm reboot`)

On your own root and daemon (see the Windows and Linux end-to-end sections), with nothing else using the VM:

- Windows: note the QEMU pid and `(Get-CimInstance Win32_OperatingSystem).LastBootUpTime`, then `vm reboot <vm>`: it
  returns with a new `boot` matching the new `LastBootUpTime`, the same QEMU pid, and System event 1074 says
  "Shutdown Type: restart" (a real restart, not a Fast Startup shutdown). With `glov exec <vm> -- powershell.exe
  -NoProfile -Command "Start-Sleep 600"` running it refuses naming the job, and `--force` reboots. While a SYSTEM task
  (`Register-ScheduledTask ... -User SYSTEM`) stops `GlovGuestService` for 60 s it refuses, naming `vm stop`/`vm start`.
- Linux: the returned `boot` matches `glov exec <vm> -- cat /proc/sys/kernel/random/boot_id`, and `journalctl -b -1`
  ends with `Journal stopped`. A job refuses it and `--force` reboots; `--timeout 1` reports the states and forces nothing.
  A guest that reboots itself (`systemctl reboot`) stays in the same VZ process with a new boot ID; it is not stopped.
- Last checked 2026-10-29 on Windows 11 (QEMU, 4G; about 47 s per reboot) and Ubuntu 26.04 (VZ, 2G; about 7 s).

### Resource use (`glov vm stats`)

`swift test` covers the arithmetic and a VM whose Mac process is the test runner itself (`StatsTests`, `MacResourcesTests`);
check the numbers against the Mac's and the guest's own tools on a running VM. Sample them close together: they move.

- Mac side: `glov vm stats --json` gives each VM's `pid` (qemu, or for VZ the
  `com.apple.Virtualization.VirtualMachine` XPC service that has the VM's disk image open: `lsof -p <pid>` lists it).
  `top -l 2 -pid <pid> -stats pid,cpu,mem,cmprs` (the second sample) shows the same CPU %, and `vmmap --summary <pid>`'s
  `shared memory` row has the guest RAM's resident and swapped sizes. `top`'s MEM and CMPRS and `footprint <pid>` count
  HVF-mapped guest memory twice, up to about twice the configured size; `memoryResident` plus `memoryCompressed` are the
  task's resident and compressed sizes scaled together down to the configured size, so they approximate `vmmap`'s guest RAM row,
  not `top`'s sum. `diskAllocated` matches `du -k <disk image>` × 1024; `diskSize` is `ls -l` of a raw image or
  `qemu-img info`'s virtual size of a qcow2.
- Mac memory: `sysctl kern.memorystatus_vm_pressure_level` (1 normal, 2 warning, 4 critical) and `sysctl vm.swapusage`.
- Guest side: Linux `glov exec <vm> -- free -b` (`total`, `available`), `df -B1 /` (`Size`, `Used`, `Avail`) and
  `cat /proc/loadavg`; Windows `glov exec <vm> -- powershell.exe -NoProfile -Command "Get-CimInstance Win32_OperatingSystem | Select TotalVisibleMemorySize,FreePhysicalMemory; Get-PSDrive C"`
  (KiB, then bytes). `cpuBusy` is a 0.5 s sample, so compare it with `top` (Linux) or Task Manager only roughly.
- Balloon: on a Windows VM with a balloon shrunk below `-m` (auto, or a fixed size), `glov vm stats <vm>`'s `BALLOON`
  matches `glov vm balloon <vm>`'s amount and mode, and `GUEST MEM USED` is about Task Manager's "In use", not that plus what
  the balloon holds (`-m` less the balloon's amount). `RESIDENT` plus `COMPRESSED` is at most `BALLOON`, near
  `footprint <qemu pid>`'s `phys_footprint` halved, not all of `-m`. Right after `glov vm balloon <vm> auto` on a shrunk guest,
  `glov vm balloon <vm>` shows the amount it has, not all of `-m`.
- Not ready: on a VZ guest, `systemctl kill -s STOP` the Guest Service as under "Linux guest end to end": within 13 s
  `glov vm stats` still shows the Mac columns and `guest: unavailable (Guest Service unresponsive: ...)`.
- Start warning: on your own root, `vm start` a VM whose memory with the running VMs' exceeds 2/3 of the Mac's
  (`sysctl hw.memsize`; an EDK2-only QEMU VM as in "QEMU port forwards" with `--memory` large is enough, its memory is
  reserved, not touched): stderr names each VM and its size, and the VM starts.

### Ubuntu cloud image

`vm create --engine vz` without `--image` reads https://changelogs.ubuntu.com/meta-release for the newest LTS (not
`meta-release-lts`, which lists a new LTS only from its first point release), then fetches `SHA256SUMS` and
`SHA256SUMS.gpg` from `https://cloud-images.ubuntu.com/releases/<codename>/release/`. `glov-image verify-ubuntu` checks
the signature against the key in `diskimage/pgpsig` and its pinned fingerprint; then `ubuntu-<version>-server-cloudimg-arm64.img` is downloaded
and refused unless its SHA256 matches. The SHA256 check and the qcow2-to-raw conversion each print their step and a
`still …` line every 30 s.

- Cache: `$GLOV_CACHE/ubuntu/<sha256>/` (default `~/Library/Caches/glov/ubuntu`). Every run fetches the
  current `SHA256SUMS`, so a new build gets a new directory and the older one is removed once the new one verifies;
  the run after prints `cached:` without downloading the image. An interrupted download resumes from its `.partial` file.
- Every run needs the network, to find the newest build.

### Windows install media

`glov host media [--language en-US] [--edition Professional]` prints the path of a bootable Windows 11 Arm64 ISO; `vm
create` uses the same cache and steps. It reads Microsoft's Media Creation Tool catalog (`https://go.microsoft.com/fwlink/?LinkId=2156292`),
downloads that language's ESD from Microsoft only, refuses it unless its SHA1 matches the catalog, and rebuilds it with
`wimlib-imagex` and `hdiutil makehybrid` (about 3 minutes and 10 GiB). Editions are the ESD's images: `Professional`
or `Core`. glov never hosts or mirrors Windows media. It also fetches virtio-win 0.1.302 from the Fedora archive:
the noarch RPM, checked against the archive's `CHECKSUM` (which lists no ISO), unpacked to
`$GLOV_CACHE/windows/virtio-win-0.1.302.iso`.

- Cache: `$GLOV_CACHE/windows/<sha1>/` holds the ESD and `<Edition>.iso` (default `~/Library/Caches/glov`);
  set `GLOV_CACHE` to another disk when the system disk is small. A second run prints the cached ISO without
  downloading or converting; an interrupted download resumes from its `.partial` file.
- Build: the catalog is 24H2 (26100), older than the ISO on microsoft.com. Microsoft has published no newer catalog:
  the 25H2 Media Creation Tool asks Windows Update instead. Windows Update brings the installed guest current.

`verify.ps1` checks exit codes, not stderr: under Windows PowerShell 5.1,
`$ErrorActionPreference = 'Stop'` turns native stderr into a terminating error.

## Human prerequisites

Run once, by a person. The start command fails until they are done.

- [ ] `brew install qemu swtpm wimlib`
- [ ] A Windows VM: `glov vm create <vm> --guest-service <glov-guest.exe>`, which starts it and waits for the install (see Windows guest end to end)
- [ ] In the guest, VS Build Tools with `--includeRecommended` (without it MSBuild cannot resolve the .NET SDK). Through
  `glov exec`: `curl.exe -sSL -o C:\vs_buildtools.exe https://aka.ms/vs/stable/vs_buildtools.exe`, then run it with
  `--quiet --wait --norestart --nocache --add Microsoft.VisualStudio.Workload.MSBuildTools --add
  Microsoft.VisualStudio.Workload.ManagedDesktopBuildTools --includeRecommended` (`Start-Process -Wait`, about 4 minutes),
  then `vm reboot`: the Guest Service's processes get `dotnet` on PATH only after a restart

<!-- If the wizard skill is available, suggest turning this list into an
interactive script that finishes by running the check above. Do not have
a Guest Service run that script. -->

## Ports

Not applicable. glov has no listening service, so several agents
can run the gate in the same clone at once. A Windows VM created with `--ssh` forwards guest SSH to its `sshPort` on
`127.0.0.1` (a free port picked by `vm create`, or `--ssh-port`); only one VM runs per VM directory.

## Capturing evidence

- Recording: `tcut` for terminal sessions — fall back to pasting command output in a code block.
- Screenshots: QMP `screendump` once M1 lands; until then macOS Screen Sharing over `-vnc`.
- UI locale: the viewer's few labels are English; guest screens show whatever
  the guest OS uses.

**This document is where the capture rules live**, and the request
document points here rather than restating them. A capture taken on the
developer's own machine carries their account's data, username, and home
paths as readily as a shared environment does. Assert on the frame, a
marker, or fixture data, and crop or mask what the tool happened to be
showing. Never capture the VM directory's `secrets/`.

## Not verified

- Agent heartbeat on a Windows guest (sign-out, reboot and hang over virtio-serial): covered by unit tests with a fake
  agent and checked live on a VZ guest only.
- VZ live snapshots across a macOS update: VZ may refuse to restore state saved by another OS version; not exercised.
- VZ screen with a desktop session (GNOME, X or Wayland): only the text console was exercised; the pointer was checked at
  the evdev level. `caps_lock`, `insert` and `menu` were not sent to a VZ guest.
- The proxy variables of `exec --as user` in an isolated VM: nobody can sign in to one yet (no SSH), so only
  `as: system` was checked live.
- Viewer window contents and live local UI event forwarding: frame delivery and direct input RPCs, plus mapping and
  connection-state unit tests, do not replace operating and capturing the native viewer. Remote two-Mac Windows/Linux
  desktop usability remains a deployed-environment check (see `docs/remote.md`).

A gap you could have closed is not a gap. Run the check whose dependency
you have already seen running, and report a check you skipped as untried,
rather than recording it here as one this repo cannot run.
