# Guest networking

Phase 1 uses each engine's built-in NAT and needs no root, entitlement or helper daemon. vmnet (QEMU
`vmnet-shared`/`vmnet-host`, the `socket_vmnet` helper, VZ bridged) is deferred: it would add reachability between
QEMU guests, at the cost of root, a restricted Apple entitlement, or a `sudo` install.

| | QEMU (Windows) | VZ (Linux) | VZ isolated (Linux) |
| --- | --- | --- | --- |
| NIC | user-mode (slirp), attached when `network` is true (a VM `vm create` installs Windows on) or `sshPort` or `portForwards` is set | `VZNATNetworkDeviceAttachment` | none (`ip link` shows only `lo`) |
| Guest → host | `10.0.2.2` reaches host services, including ones bound only to `127.0.0.1` | the NAT gateway (`192.168.64.1` by default) reaches host services bound to it or `0.0.0.0`, not ones bound only to `127.0.0.1` | nothing: the egress proxy refuses the Mac, the LAN and private ranges even when allowlisted |
| Host → guest | only forwarded ports: `sshPort` (guest 22) and `portForwards`, on the host's `127.0.0.1` | the guest's DHCP address on the NAT subnet, any port; `glov ssh` over vsock | `glov ssh` over vsock, with every forward cleared |
| Guest → internet | yes | yes | only allowlisted hosts, over HTTP(S) through the egress proxy; secrets substituted for the hosts they are bound to |

## Linux VMs on a Windows host

Every VM the Windows edition (`windows/`) creates is isolated ([below](#isolated-vms-on-a-windows-host)): its HCS
document has no network adapter, so `glovd` creates no HCN network or endpoint and never changes host networking.
`exec`, `cp` and `ssh` reach the guest over `hv_sock`; nothing reaches it by address.

Versions before this put normal VMs on an HCN network of type `ICS` named `glov`. Such a VM is not migrated:
`glov vm start` and `vm reboot` refuse it, so copy out what it holds, then delete it and create it again. The `glov`
ICS network such a version made is non-persistent and goes at the next Windows restart.

### Isolated VMs on a Windows host

`glov vm create <vm>` (Ubuntu, the default, or `--distro debian`; `--isolated` is accepted and changes nothing)
creates a VM whose HCS document has no network adapter. Its only way out is the same egress proxy as an [isolated VZ VM](#isolated-vz-vms), with the same allowlist rules, `glov egress` commands,
MCP tools, log and `glov ssh` restrictions; `--egress open` lets it reach any public host, still through the proxy.
The provisioning boot has no NIC either: a NoCloud `network-config` with a dummy device keeps cloud-init from waiting
2 minutes for the network, and provisioning then removes that configuration, masks `systemd-networkd-wait-online` and
turns cloud-init off, so later boots do not wait for a network. What differs:

- The Guest Service's proxy reaches `glovd` over `hv_sock`: `glovd` binds the VM's `VsockServiceID(4024)`
  between glov-broker creating the compute system and starting it (and again when it finds the VM running after a
  restart), for that VM's ID only. No other host `hv_sock` service listens for Linux guests. The VM's `hv_sock`
  security descriptor names only SYSTEM and the user whose `glovd` runs it, so another user, administrators
  included, can neither bind nor reach the VM's ports.
- The proxy runs in `glovd`, which runs as the user, so a VM has no way out while `glovd` is stopped: restarting or
  upgrading it (`glov host install`) cuts every connection in flight through the proxy (a download, a `git push`, a
  harness's model call). VMs keep running, and new connections work again once `glovd` has found them. Let in-VM
  work finish before upgrading. Signing out of Windows ends `glovd`, and glov-broker shuts the user's VMs down
  unless the user is still signed in to another session.
- Names resolve on the Windows host, and the refused addresses are the host's own interfaces instead of the Mac's.
- vm.json lies in the user's glov home, `%LOCALAPPDATA%\glov\vms\<vm>\`, with `egress.log` next to it; `glovd`
  changes it, through `glov egress`.
- [Secrets](#secrets) and their [substitution](#secrets) work as on the Mac, model-API keys
  ([Model API keys](#model-api-keys)) included, with `glov secret set|source|refresh|list|remove|grant|revoke|grants`
  from a non-elevated shell, and so do git secrets (`--forge`), their [repositories](#secrets) (`glov secret repos`),
  the guest's git sign-in and [subscriptions](#subscriptions) (`glov secret login|logout`). What differs:
  - `glovd` keeps each value in `%LOCALAPPDATA%\glov\secrets\<name>.value`, encrypted with DPAPI in user scope, so
    only the user decrypts it, as the login Keychain holds it on a Mac; a copied file does not decrypt for another
    account or on another machine. `secrets.json` beside it holds names, sources and hosts. Two names that differ
    only in case are refused, as NTFS would store them in one file. Secrets a build that ran `glovd` as SYSTEM
    stored are not carried over: store them again.
  - `glovd` runs no command. `glov secret source` runs its command in `glov`, as the user (stdin closed, stderr
    discarded, at most 10 s), and stores what it printed; `glov secret grant` runs it again for each source secret it
    grants (and for every source secret the VM holds at each `glov claude`, `codex` or `agy` start, where a failing command only warns), and `glov secret refresh <name>` on demand. A bare command name is resolved on the user's PATH and stored as
    an absolute path. A value a command printed stays until one of those runs again, so a token that expires needs a
    refresh.
  - `secrets.json` (in `%LOCALAPPDATA%\glov\secrets\`, next to the values) is a JSON array with one object per
    secret, sorted by name: `name`, `source`, `hosts` (canonical allowlist entries), `forge` when it is a git sign-in,
    plus the fields of its source. `"source": "stored"` (`glov secret set`) adds `length` and `ending` (the last 4
    characters, only for a value of 16 or more), which `glov secret list` prints in place of reading the value.
    `"source": "command"` (`glov secret source`) adds `command`, the argv with its program as an absolute path
    (`["C:\\Program Files\\PowerShell\\7\\pwsh.exe", "-NoProfile", "-File", "x.ps1"]`), and no `length` or `ending`;
    `secret list` prints it as `command: <argv joined by spaces>`. No value is in the file.
  - A source command that fails is reported as `secret <name>: <program> failed (exit N)`, with the program's file
    name and never its output; the other failures read `did not finish within 10s`, `printed nothing`, `printed
    over 64 KiB`, `printed text that is not UTF-8` and `cannot run <path>: ...`. `glov secret source`, `refresh` and
    `grant` stop with it (`grant` before granting anything), and so does a `glov claude`, `codex` or `agy` that grants
    a harness's secrets for the first time. At every later start those three only warn, `warning: secret <name>:
    <program> failed (exit N); <name> keeps its old value`, and the harness starts with the value from the last
    successful run. `secret refresh` of a stored secret says `secret <name> is not taken from a command: store it
    again with glov secret set <name>`.
  - The egress CA lies in `%LOCALAPPDATA%\glov\egress-ca\`, made on the first isolated VM, and the provisioning script
    makes the guest trust it (`/usr/local/share/ca-certificates/glov-egress-ca.crt`, with `NODE_EXTRA_CA_CERTS`,
    `REQUESTS_CA_BUNDLE` and `SSL_CERT_FILE` set to the system bundle in `/etc/environment`). A VM created before the
    CA does not trust it: create it again. Upstream TLS is verified against the Windows certificate store.
  - Builds before secrets had a model-API reverse proxy (`glov egress api`, `egress key`, `egress limits`): its
    vm.json `apiKeys` and `limits` are ignored, and the profiles of an isolated VM made then point at a proxy that is
    gone, so recreate those VMs and store the keys again with `glov secret set`. Delete
    `%ProgramData%\glov\api-keys\` from an elevated shell, if a build that ran `glovd` as SYSTEM left it; nothing
    reads it.
  - `tmpfs` secrets reach `/run/glov/secrets` over `secrets.sync` when the Guest Service connects and at each grant,
    revoke, set or remove ([protocol](guest-protocol.md#secrets-in-tmpfs)).
  - `glovd` writes the git sign-in files with `exec.start` (root, and `runuser -u glov` for the files in the guest
    user's home), at the same moments; a guest without `base64` or `cmp` cannot be set up, and an empty file is not
    created. Node IDs are looked up from the Windows host, verified against its certificate store.
  - Only a VM made with `--harness` has the guest user `glov`, so only such a VM gets gh's and glab's files; in
    another, git still signs in for every account, and gh or glab take the placeholder from `GH_TOKEN` or
    `GITLAB_TOKEN` (`glov-secret:<name>`).
  - A GraphQL request to a git secret's host needs one `Content-Length` of digits only (else `403` with `a GraphQL
    request without one Content-Length`), and its body must arrive within 10 s (else `408`).
  - The subscription tokens stay on the Windows host, in the same store. `glovd` switches a running guest through
    `exec.start` (root for `/etc/environment` and the marker, `runuser -u glov` for the harness's files), at login,
    logout and each Guest Service session. A sign-in in the guest runs over `glov ssh <vm> -- -t`. A token request
    needs one `Content-Length` of digits only (else `411`), its body must arrive within 10 s (else `408`), and the
    answer within 60 s. `glov vm reset` keeps vm.json's subscriptions as they are now, with their tokens' grants and
    runtime hosts, as a snapshot revert does on the Mac.
- `glov egress allow <vm> <host> --private`, `glov vm configure <vm> --egress open|restricted
  [--allow-plain-secrets]` (also `vm create --egress open`) and `glov vm trust list|add|remove` work as
  [on the Mac](#isolated-vz-vms), with the same vm.json keys (`privateAllow`, `open`, `plainSecrets`, `trust`) and
  refusals. `glov claude|codex|agy` is not there yet. What differs:
  - The refused own addresses are the Windows host's, and names resolve with its resolver, so a VPN the host is on
    reaches a `--private` entry.
  - `glovd` updates the guest's trust store itself, running the same script as root through the Guest Service when
    it connects and at once on a running VM after `vm trust add` or `remove`. A VM's `/etc/environment` already
    names the system bundle, so there is nothing to repair there.
  - The proxy verifies an intercepted upstream against the Windows certificate store plus the VM's `trust`
    certificates.
- [Harness profiles](#harness-profiles) are the Mac's, byte for byte (`windows/internal/harness/profiles/`):
  `glov vm create <vm> --harness claude` (repeatable; also `codex`, `antigravity`) writes the
  placeholders and prints the secrets each harness needs, then `glov vm harness install <vm>`. The provisioning
  script creates the user `glov` (no password, sudo without one), writes the profiles' environment and files, and
  signs that user in on `tty1`; the serial console `ttyS0` stays a plain login prompt. There are no snapshots: `glov
  vm harness install` freezes the VM as a template instead ([below](#harness-template-and-tasks-on-a-windows-host)).

### Harness template and tasks on a Windows host

```sh
# Template, once
glov vm create agent --harness claude --harness codex
glov vm start agent
glov exec agent --as user -- git config --global user.name "glov agent"   # retry while it exits 75
glov exec agent --as user -- git config --global user.email agent@agent.invalid
glov vm harness install agent       # installs, stops the VM and freezes it with the allowlist of that moment
git clone --no-local C:\src\proj C:\tasks\t1
glov share add agent t1 C:\tasks\t1 --ro --exclude node_modules/

# Each task
glov vm start agent
glov vm wait agent guest-ready
glov share sync agent t1          # into /mnt/c/tasks/t1, owned by glov
glov exec agent --as user --cwd /mnt/c/tasks/t1 -- claude -p "..."   # retry while it exits 75
glov exec agent --as user --cwd /mnt/c/tasks/t1 -- sh -c 'git add -A && git commit -m t1 && git bundle create /tmp/t1.bundle origin/HEAD..HEAD'
glov cp agent:/tmp/t1.bundle "$env:LOCALAPPDATA\glov\scratch\t1.bundle"
glov vm stop agent
glov vm reset agent
git -C C:\src\proj fetch "$env:LOCALAPPDATA\glov\scratch\t1.bundle" HEAD:refs/heads/t1
```

- `glov vm harness install` starts a stopped VM, updates its Guest Service to `glovd`'s build, installs, then stops
  it and makes its disk the template: `template.vhdx` in the VM's directory, under a new differencing disk
  `disk.vhdx`. vm.json records the egress settings of that moment (allowlist and its `--private` marks, granted
  secrets, `mcpEdits`, the egress mode, trusted certificates).
  `glov vm status` says `frozen`.
- Like on the Mac, the install also copies your harness instructions, skills and agents from `%USERPROFILE%` (links
  followed; credentials, broken links and link cycles left out and listed); `--no-host-config` skips that.
  `--harness <name>` (repeatable) installs only those harnesses.
- A VM made with `--harness` gets the live share `harness-sessions` (not with `--no-sessions`): `%LOCALAPPDATA%\glov\sessions\<vm>` on the host
  at `/mnt/glov-sessions` in the guest, where the install links each harness's conversation records (such as
  `~/.claude/projects`). The records survive `vm reset` and `vm delete`; read them as untrusted data. `glov vm status` shows the folder with its file count and size
  (links are not followed), and `glov vm delete` says it kept the folder. After a
  `vm reset` the share's agent takes the share again and copies what either side holds, so a reset deletes nothing
  on the host.
- `glov vm harness add <vm> <harness>` adds a harness to a running isolated VM made with `--harness` that is not frozen: it writes
  the profile's environment and files (keeping the files the user already has), allows its runtime hosts, installs
  it, copies the host config (`--no-host-config` skips that) and links its records. A VM made without `--harness` has
  no guest user for one and is refused before anything changes; create a harness VM instead.
- `glov vm reset <vm>` (stopped only) replaces `disk.vhdx` with an empty one on the template and restores the
  recorded egress settings, so what a task wrote in the guest and allowed in `glov egress` is gone. Like a revoked
  grant, a VM restricted since the freeze stays restricted, a `tmpfs` secret no longer allowed while open stays
  withheld, a certificate removed or replaced with `glov vm trust` stays out (the template disk still holds it,
  and the trust sync at the next Guest Service session takes it out of the guest), and so does a repository removed
  with `glov secret repos`; to change the template's mode for good, set it with the VM stopped and run `glov vm harness install`
  again. Shares are host settings and stay; what `share sync` copied in is gone with the disk, so each task syncs
  again.
- On a frozen VM, `glov vm harness install` resets it first, installs, and merges the disk into the template, so
  setting the git identity or anything else for every task means doing it before the first install. Run it with the
  VM stopped.
- `glov vm configure --disk-size` grows the differencing disk, and later resets keep the size; `/` grows at the
  next boot. Let that boot run a few seconds before stopping the VM. `glov vm delete` removes both disks.
- In a harness VM `share sync` makes `glov` the owner of the share folder and of every folder and file it
  creates or writes, so the harness can change them. A Guest Service too old for that fails the sync before writing
  anything and asks for `glov vm guest update`. On a frozen VM an update, like any change, is gone after `vm reset`:
  stop the VM and run `glov vm harness install` again, which updates the Guest Service before it freezes the template. The share is `--ro` because
  nothing comes back through it: the bundle leaves through `glov cp` into the scratch folder, and the clone on the
  host is never written by the guest.
- `glov exec --as user` exits 75 until the console sign-in, a few seconds after `guest-ready`.

## Isolated VZ VMs

`glov vm create <vm> --engine vz --isolated` creates a Linux VM with no network device (`ip link` shows only `lo`).
It reaches the internet only through an HTTP proxy at `127.0.0.1:3128` inside the guest, which the Guest Service
tunnels over vsock to `glovd` (see [the protocol](guest-protocol.md#isolated-vm-egress-proxy)). The proxy is
default deny: it allows a CONNECT, or a plain-HTTP request, only to a host in the VM's allowlist:

```json
"isolated": true,
"egress": { "allow": ["archive.ubuntu.com", "*.ports.ubuntu.com", "api.anthropic.com"] }
```

- Entries are exact hostnames or `*.suffix` wildcards, for ports 443 and 80; `host:port` allows another port
  instead (for example `registry.example.com:8443`). Edit them with `glov egress allow|remove|list <vm>` (MCP
  `egress_allow`, `egress_remove`, `egress_list`); new connections follow, no restart needed. A connection already
  open when its host is removed stays open until it ends. An entry that is no valid hostname, `*.suffix` or address
  allows nothing; `glov egress allow` refuses one.
- MCP clients may change the allowlist only after `glov egress mcp <vm> on` (vm.json `"mcpEdits": true`); `off`, the
  default, refuses `egress_allow` and `egress_remove` so an MCP-only agent cannot widen its own way out. No MCP tool sets
  the switch. It does not stop an agent that can run `glov` in a Mac shell.
- Names resolve on the Mac, so the guest needs no DNS. The Mac itself, the LAN, private and link-local ranges are
  refused even when allowlisted, and so is everything the host reaches without the proxy: there is no route.
- A self-hosted service on a private network or VPN (an internal GitLab, say) needs `glov egress allow <vm> <host>
  --private` (vm.json `"privateAllow"`, listed with `(private)`): that entry may also resolve to RFC 1918, CGNAT
  (`100.64.0.0/10`, which tailnets use) and unique local (`fc00::/7`) addresses. Only an exact name or address takes
  `--private`, never `*.suffix` (allowing it again without `--private` keeps the mark: remove and re-add it to drop
  it), and every other entry stays public-only (a denial still says `no public address`).
  Loopback, link-local, multicast and the Mac's own addresses stay refused. Only the CLI sets it: MCP `egress_allow`
  with `private` is refused even after `glov egress mcp <vm> on`. Names resolve with the Mac's resolver, so a VPN's
  split DNS applies. macOS Local Network privacy blocks a launchd agent such as glovd from the Mac's own Wi-Fi and
  Ethernet networks (not VPN ones) with "no route to host" until the user allows it; egress.log then says
  `unreachable; a host on the Mac's own LAN needs Local Network access`. glovd carries an Info.plist (identifier
  `org.glov.daemon`, linked into its `__TEXT,__info_plist`) with a usage description, so its first such connection
  brings up the macOS alert naming glovd; once allowed, the next connection goes through without restarting glovd, and
  the answer survives `glov host install`. The answer lives in System Settings > Privacy & Security > Local Network,
  which lists one glovd entry per install that switch together. On macOS 27.2, switching them off did not block glovd
  again, even after a restart, and removing them (`-`) does not reset the answer: the next connection adds glovd back
  as it was, without an alert.
- `glov vm configure <vm> --egress open` (vm.json `"open": true`; also `vm create --egress open` and `glov claude|codex|agy
  --egress open` for a new project VM) lets the guest reach any public host, still only through this proxy: the
  allowlist is kept but not checked until `--egress restricted`; private networks still need `--private` entries;
  substitution, repository checks and egress.log are unchanged. `vm status` and `glov egress list` show the mode. The
  RPC (`egress.mode`) has no MCP tool. Opening a VM that holds a `tmpfs` secret, or granting one to an open VM, needs
  `--allow-plain-secrets` (`RPCError` -32002 until then); the secrets so allowed are vm.json `"plainSecrets"`, and while
  open glovd withholds any other `tmpfs` secret from the guest. See [the harness guide](isolated-harness-vms.md#open-network).
- An internal CA reaches the guest as trust data: `glov vm trust add <vm> <ca.pem> [--name <n>]` (vm.json
  `"trust"`, one certificate per name; `list`, `remove`). The Guest Service writes each to
  `/usr/local/share/ca-certificates/glov-trust/<name>.crt` and runs `update-ca-certificates` when the set changed, on
  every Guest Service session and at once on a running VM. The same sync points an older VM's `NODE_EXTRA_CA_CERTS` in
  `/etc/environment` at the system bundle; shells opened before that keep the old value until reopened. The egress proxy also trusts these certificates, besides the
  Mac's roots, when it verifies an intercepted upstream (a host a granted secret is bound to), so the Mac need not
  trust the CA. No MCP tool edits trust.
- Only programs that honor `HTTPS_PROXY`/`HTTP_PROXY` get out (curl, apt, most language runtimes); `--noproxy`
  traffic, raw sockets and non-HTTP protocols fail closed.
- Every allowed and denied connection is logged as metadata (VM, host, port, decision, bytes): `glov egress log <vm>
  [--denied] [--follow]` (MCP `egress_log`) reads `<vm>/egress.log`; the daemon log has the same lines.
- The allowlist limits where data goes, not whether it leaks: an allowed host is still a channel, and so is any site
  that shares its servers (a CDN), since the proxy cannot see names inside TLS.
- The Guest Service comes from the seed volume: a first boot installs nothing from the network.

### Model API keys

A model-API key is an ordinary [secret](#secrets) named after its provider and bound to the provider's API host. The
harness profiles declare it as a `placeholder` secret, which gives the harness its placeholder as the key and leave it at the provider's real endpoint, so the
egress proxy substitutes the value on the way out like any other secret:

```sh
glov secret set anthropic --host api.anthropic.com   # reads the key from stdin, without echo; kept in the login Keychain
glov secret grant <vm> anthropic
```

| Secret | Host | Placeholder in the profile |
| --- | --- | --- |
| `anthropic` | `api.anthropic.com` | `ANTHROPIC_API_KEY=glov-secret:anthropic` (sent as `x-api-key`) |
| `openai` | `api.openai.com` | `CODEX_API_KEY=glov-secret:openai` (sent as `Authorization: Bearer`) |
| `gemini` | `generativelanguage.googleapis.com` | `GEMINI_API_KEY=glov-secret:gemini` (sent as `x-goog-api-key`) |

- The provider's host must also be in the VM's allowlist; each profile's `runtime` hosts include it.
- The key travels in a header, so a client that puts it in the URL (a Gemini `?key=` parameter) sends the placeholder
  unchanged and gets the provider's authentication error.
- glov caps neither requests nor tokens: cap spend at the provider (below). A closed guest connection ends the request,
  which is how a harness interrupts a generation.
- Each request is logged with method, path (no query), status, bytes and the secret's name: `glov egress log <vm>`.
- Hiding the key stops theft, not abuse: a hostile guest can spend what the key allows, and the request body is a
  channel out. Use a dedicated key for isolated VMs, in a workspace or project with a provider-side hard spend limit
  (checked 2026-10-31):
  - Anthropic: a workspace [spend limit](https://platform.claude.com/docs/en/manage-claude/workspaces), with the key
    scoped to that workspace (not available on the Default Workspace).
  - OpenAI: the project's [spend limit](https://developers.openai.com/api/docs/guides/spend-limits) with "Enforce a
    hard limit" on; a spend alert alone caps nothing.
  - Gemini: a project [spend cap](https://ai.google.dev/gemini-api/docs/billing) in AI Studio (experimental, enforced
    about 10 minutes late); a Cloud Billing alerts-only budget caps nothing.
- A harness can use the owner's subscription instead of a key: [Subscriptions](#subscriptions).
- `glovd` creates the Keychain items, so it reads them without asking. An ad-hoc signed daemon rebuilt since (the
  development builds) may show one Keychain prompt per item, or, with no prompt possible, fail with a `502` naming the
  secret. Choose Always Allow, or store the value again with `glov secret set`, which replaces the item so the running
  daemon owns it. Earlier builds kept keys under the Keychain service `glov API keys`, which nothing reads now: delete
  those items.

### Secrets

`glov secret` names secrets on the Mac and decides which isolated VMs may use them. A secret's value is either kept in
the login Keychain or is the output of a host command, so a token a host CLI already manages is never copied:

```sh
glov secret set npm --host registry.npmjs.org             # one * per character typed or pasted, then asks to confirm
glov secret source github --forge github --host github.com --host api.github.com --host uploads.github.com \
  -- gh auth token --hostname github.com
glov secret source gitlab --forge gitlab --host gitlab.example.com -- glab config get token --host gitlab.example.com
glov secret grant <vm> github npm                          # or: revoke; glov secret grants <vm> lists them
glov secret repos <vm> add github.com/owner/app            # the repositories a git secret may reach
glov secret list                                           # names, sources and hosts; never a value
```

- Each secret is bound to the hosts it may be sent to (allowlist entries: a name, `*.suffix` or an address, optionally
  with `:port`). A secret with no host is refused.
- `<home>/secrets.json` (mode 0600) holds names, sources and hosts; vm.json `egress.secrets` holds a VM's grants by
  name. Values are never written to either, to a log, or to an RPC or MCP result. `glov secret remove` also revokes
  the secret from every VM.
- `glovd` runs a source command with stdin closed and stderr discarded, waits at most 10 s, and keeps the value in
  memory for a minute; a failure names the secret, the program and its exit status, not its output. A bare command
  name is resolved on the caller's PATH when the secret is defined, because `glovd` runs with launchd's short PATH.
- At a terminal, `glov secret set` (and the pasted token of `glov secret login`) shows one `*` per character, takes
  Backspace and Ctrl-U, and has no line limit (up to 64 KiB; pipe anything longer on stdin). It then shows the
  length and, for a value of at least 16 characters, the last 4 (`108 characters, ending …a1b2`), points out
  whitespace or control characters inside it (how many, not where), and stores it only after a yes. Leading and
  trailing whitespace is trimmed, at the prompt and on stdin. `glov secret list` shows the same for each Keychain
  secret, from `secrets.json`, so checking a stored value reads no value; a secret stored before this shows `length
  not recorded`.
- Granting and storing secrets is for `glov` on the Mac; no MCP tool does either.

**Substitution.** A guest never holds a value. It writes the placeholder `glov-secret:<name>` where the credential goes,
for example `Authorization: Bearer glov-secret:github`, or as the user or password of Basic credentials
(`https://x-access-token:glov-secret:github@github.com/...`). The egress proxy replaces it on the way out:

- Only a `CONNECT` to a DNS name that a secret granted to the VM is bound to is intercepted. The proxy answers `200`,
  then terminates the guest's TLS with a certificate for that name signed by the egress CA, and sends the request on
  over TLS verified against the Mac's trust store. It connects upstream only once the request head and its values are
  read, so a slow value (a Keychain prompt, a command source) leaves no idle connection for the provider to close; an
  upstream out of reach is then a `502` inside the TLS. Every other allowed host, and every IP-literal target, is still
  an opaque tunnel. The egress allowlist applies first, as for any connection.
- One request per intercepted connection: the proxy reads one HTTP/1.1 request head (ALPN offers `http/1.1` only),
  drops `Proxy-*`, `Connection`, `Keep-Alive` and `Upgrade`, adds `Connection: close`, and passes the body and the
  response through as bytes. The `Host` header must name the CONNECT host, so a shared front end cannot route the
  value to another site.
- Placeholders are replaced in header values only, including inside Basic credentials, which are re-encoded. A
  placeholder in the request line or the body is passed through unchanged, except at a subscription's token endpoint
  ([Subscriptions](#subscriptions)).
- Refusals, answered as plain text before anything goes upstream: a secret that is not defined or not granted to the
  VM (`403`, naming `glov secret grant <vm> <name>`), one not bound to the host (`403`), a `Host` mismatch (`403`), a
  malformed head (`400`), and a value that cannot be read or contains CR, LF or NUL (`502`). A plain-HTTP request
  naming a placeholder anywhere is refused (`403`): a value never travels unencrypted.
- Each intercepted request is one egress.log line with its method, path, response status and `secrets` (names only).

**Repositories.** A secret made with `--forge github` or `--forge gitlab` is a git sign-in: a request to its hosts that
uses it (any of the VM's placeholders in its head) must name one of the VM's repositories, vm.json `egress.repos`, kept
with `glov secret repos <vm> add|remove|list`. An entry is `<host>/<owner>/<repo>`, or `<host>/<group>/*` for every
repository under a group, subgroups included; hosts and paths compare without case. A request outside them gets `403`
naming the repository and the command that allows it, and an egress.log `deny` line; nothing goes upstream. A request
that uses no secret is not checked: a public clone or package fetch reaches what anyone on the internet can, and a
private repository answers it `401`, after which git asks its credential helper and the signed-in retry is checked.

- git smart HTTP and web paths name `owner/repo` on GitHub; on GitLab the path up to `/-/`, `.git`, `info/refs`,
  `git-upload-pack`, `git-receive-pack` or `info/lfs`. GitHub's REST API (`api.github.com`, `uploads.github.com`,
  `/api/v3` on GitHub Enterprise) names it in `/repos/<owner>/<repo>`; GitLab's in `/api/v4/projects/<url-encoded path>`.
  A path with an empty, `.` or `..` segment, or a `/` encoded inside a segment (outside GitLab's project path), is
  refused, so the path checked is the path served.
- An API path that names no repository passes only when it reads none: GitHub `/user`, `/user/emails`, `/users/<u>`,
  `/orgs/<o>`, `/rate_limit`, `/meta`; GitLab `/user`, `/users/<u>`, `/version`, `/metadata`,
  `/personal_access_tokens/self`. The rest (`/user/repos`, `/issues`, `/notifications`, search, a repository or
  project by number, GitLab GraphQL) is refused, since it spans repositories. A web path of one segment (`/login`)
  may only read (`GET`, `HEAD`, `OPTIONS`).
- GitHub GraphQL (`/graphql`, `/api/graphql`) is read whole (at most 256 KiB, with a `Content-Length`), parsed, and
  sent on as it came only when every part passes; a query the proxy cannot parse is refused. Node IDs it passes are
  resolved through the forge with the VM's git secret (`nodes(ids:)`, kept for the daemon's life): a repository, or a
  node of one (a pull request, issue, label, ref, commit, milestone), must be allowed; a user, organization, bot or
  team is a person; anything else is refused.
  - A query's top-level fields may be `repository(owner:, name:)` of an allowed repository, `node`/`nodes` by ID,
    `viewer`, `user`, `organization`, or fields of no repository (`rateLimit`, `meta`, introspection); anything else
    (`search`, `resource`, `repositoryOwner`) is refused.
  - Inside a repository, fields that lead to another repository (`forks`, cross-referenced issues, `repository`) are
    refused, and fields that lead to a person or project (`owner`, `author`, `assignees`, `projectsV2`) are read by
    the person rules. A fork's `parent` or `source` and a pull request's `headRepository` or `baseRepository` are
    read for their identity only: fields without a selection set (not `title` or `body`), `owner`, `defaultBranchRef`.
  - Under a person or project only fields without a selection set (`login`, `title`), other people
    (`requestedReviewer`) and `nodes`, `edges`, `node`, `pageInfo` are read: its `repositories`, `issues` or `items`
    span repositories.
  - Each top-level mutation field must name an allowed repository by a node ID among its arguments (`repositoryId`,
    `pullRequestId`; `clientMutationId` is no node) or a `nameWithOwner`; its other node IDs must be allowed
    repositories or people. Unused variables are not read; one the request's JSON leaves out takes its definition's
    default, else null. Subscriptions are refused.

**Git sign-in in the guest.** For the git secrets granted to an isolated VM, glovd sets up the guest at each Guest
Service session and after each grant, revoke or secret change, writing only files that differ:

- `/usr/local/bin/git-credential-glov` and `/etc/glov/git-sign-ins` (`<host> <forge> <secret>` per git host): git's
  credential helper answers a `401` from a listed HTTPS host with `x-access-token` (GitHub) or `oauth2` (GitLab) and
  the password `glov-secret:<secret>`, which the proxy substitutes in the Basic credentials.
- `/etc/gitconfig`, between `# glov git sign-in` lines (other lines stay): `credential.helper = glov`, and
  `url.https://<host>/.insteadOf` for `git@<host>:` and `ssh://git@<host>/`, so SSH remotes go over HTTPS.
- `~/.config/gh/hosts.yml` (GitHub hosts) and `~/.config/glab-cli/config.yml` (GitLab hosts, `is_oauth2` so glab sends
  `Authorization: Bearer`, which GitLab takes for an OAuth or personal access token, and a far-future
  `oauth2_expiry_date` that glab requires; `check_update` and `telemetry`
  off), 0600, with the placeholder as the token. Replaced whole; removed once no git secret of the forge is granted.
  gh and glab are not installed by glov.
- A git secret's hosts name its git hosts: GitHub's `api.github.com` and `uploads.github.com` fold into `github.com`;
  wildcards, IP addresses and ports other than 443 name none. The VM's allowlist must also allow them.
- `glov claude` (and the other project commands) on a new VM offers the repositories of the project's git remotes
  (`.git/config`, read once; one repository per remote, never a `*` pattern) with the stored git secrets bound to
  their hosts. Applying them adds the repositories to `glov secret repos`, grants those secrets and allows their hosts.
  The project's `.git/config` is writable by every guest it was shared with, so the remotes the owner applied are
  kept on the Mac in `<home>/project-remotes.json` (project path to repositories), and any other is listed as `new:`.
  At a terminal glov asks, `[Y/n]` when none is new and `[y/N]` otherwise; without one it applies them only with
  `--allow-remotes`, warning of each new one. Declined, the VM is made without them.
- `glab config get token --host <host>` prints the token glab stored. After `glab auth login` in a browser that is an
  OAuth token, which GitLab expires within hours, and `config get` may print it expired: a VM then gets `401` until
  glab on the Mac renews it. For a VM that works longer, give glov a personal access token (`glov secret set` with
  `--forge gitlab`).

**Egress CA.** `<home>/egress-ca/` (0700) holds `ca.pem` and `ca.key` (0600). The first isolated VM created or started
makes it; the key never leaves the Mac. `glov vm create --isolated` writes `ca.pem` to
`/usr/local/share/ca-certificates/glov-egress-ca.crt` and runs `update-ca-certificates` on first boot, so a VM created
before the CA does not trust it: recreate it. Programs that skip the system store get it through `/etc/environment` and the Guest Service's environment (which
sets them only when the CA file exists, so a guest of a host with no CA gets none): `NODE_EXTRA_CA_CERTS` (Node), `SSL_CERT_FILE` and
`REQUESTS_CA_BUNDLE` (OpenSSL, Python requests) name the system bundle, which also holds `glov vm trust` certificates. A program with roots compiled in still fails its
handshake with a bound host, and egress.log shows `guest TLS: the guest does not trust the egress CA`.

**Cross-platform contract.** The Windows edition implements the same behaviour; only the value store differs (the
login Keychain, service `glov secrets`, account = secret name, on a Mac; DPAPI or Credential Manager on Windows).

| Part | Contract |
| --- | --- |
| Names | 1–64 of letters, digits, `.`, `_`, `-`; error `secret name <n>: use letters, digits, '.', '_' or '-', at most 64` |
| `secrets.json` | in the home directory, mode 0600 (owner-only ACL on Windows), a JSON array sorted by name: `{"name", "source": {"keychain": {}} or {"command": {"_0": [argv]}}, "hosts": [canonical entries], "length"?, "ending"?, "forge"?}`; `length` and `ending` (the last 4 characters, only for a value of at least 16) are set with a Keychain value |
| `secret.set` | params `{name, hosts, value, forge?}` (`github` or `gitlab`, else `forge <f>: use github or gitlab`); result `{name, source: "keychain", hosts, forge?, length, ending?}`; errors `secret <n>: bind it to at least one host with --host`, `secret <n>: the value is empty` |
| `secret.source` | params `{name, hosts, command, forge?}`; `command[0]` absolute, else `secret <n>: give the command with an absolute path`; result adds `command`; drops any stored value |
| `secret.list` | result `{secrets: [{name, source, command?, hosts, forge?}]}` |
| `secret.remove` | params `{name}`; also revokes it from every VM; error `no secret named <n>` |
| `secret.repos` | params `{vm, add?, remove?}` (`<host>/<owner>/<repo>` or `<host>/<group>/*`, normalized to lower case without scheme or `.git`); result `{vm, repos}`; isolated VMs only; errors `repository <r>: give <host>/<owner>/<repo> or <host>/<group>/*`, `VM <vm> has no repository <r>` |
| `secret.grant`, `secret.revoke`, `secret.grants` | params `{vm, names}`; result `{vm, names, missing}`, `missing` naming grants whose secret is gone; isolated VMs only; errors `no secret named <n>: define it with glov secret set or glov secret source`, `VM <vm> was not granted <n>` |
| Command source | stdin closed, stderr discarded, 10 s limit, trailing newlines trimmed, value cached 60 s; errors `secret <n>: <program> exited with status <s>`, `secret <n>: <program> did not finish within <limit>`, `... printed nothing` |
| CLI output | `stored secret <n> for <hosts> (<length> characters[, ending …<last 4>])`; `secret <n> comes from <program> for <hosts>`; list lines `<name>\t<source or command: argv>[ (<forge> sign-in)]\t<hosts>`, with `\t<length> characters[, ending …<last 4>]` for a Keychain secret; `removed secret <n>`; grants one name per line or `<vm>: no secrets granted`; repositories one per line or `<vm>: no repositories` |
| Placeholder | `glov-secret:` followed by the longest run of name characters |
| Proxy | the substitution rules and status codes above, with refusal bodies `glov egress proxy: <host>:<port>: <message>` |
| Log | the egress.log JSON line gains `"secrets": [names]`; the daemon line ends ` secrets=<a,b>` before any reason |

### Subscriptions

`glov secret login <vm> <harness>` signs a harness in to the owner's subscription (Claude Pro/Max, a ChatGPT plan, a
Google account for Antigravity) instead of its API key; `glov secret logout <vm> <harness>` goes back to the key. The
tokens stay on the Mac as secrets named `<vm>.<harness>.<name>`, granted to the VM and bound to the hosts in the
profile's `subscription.secrets`; the guest holds placeholders, as for a key.

| Harness | Sign-in | Secrets (hosts) |
| --- | --- | --- |
| `claude` | paste the token `claude setup-token` prints (run it anywhere; valid a year) | `token` as `CLAUDE_CODE_OAUTH_TOKEN` (`api.anthropic.com`) |
| `codex` | `codex login --device-auth`, run in the VM in the caller's terminal | `access` (`chatgpt.com`), `refresh` (`auth.openai.com`) in `~/.codex/auth.json` |
| `antigravity` | `agy`'s Google sign-in, run in the VM in the caller's terminal | `access` (`daily-cloudcode-pa.googleapis.com`, `www.googleapis.com`), `refresh` (`oauth2.googleapis.com`) in `~/.gemini/antigravity-cli/antigravity-oauth-token` |

- **Token endpoints.** A harness that signs in or refreshes itself does so through the proxy, which intercepts the
  profile's `subscription.tokenEndpoints` hosts once the harness is signed in (vm.json `egress.subscriptions`), even
  before any of its secrets exists. A `POST` to an endpoint's path is read whole: it must carry a `Content-Length` of
  at most 64 KiB (`411`, `413`). In a form or JSON body, each value that is exactly a placeholder gets its secret's
  value, under the same grant and host checks as a header. `Accept-Encoding` is dropped, and the answer (at most
  1 MiB, chunked or not) is read whole too. In a `2xx` JSON answer, each field the endpoint's `answer` maps is stored
  as that secret (replacing the old value, so a rotated refresh token is never lost) and replaced by its placeholder
  before the answer reaches the guest. An answer that cannot be read or stored is a `502`, and the guest gets no token;
  any other answer passes through. Any value the proxy put into the request is replaced by its placeholder in the
  answer, so an error that echoes the request gives the guest nothing. egress.log names the secrets read and stored.
- **Switching the guest.** The profile's `subscription` says what differs from API mode: the `/etc/environment`
  variables (the API key's placeholder out, `CLAUDE_CODE_OAUTH_TOKEN` in) and single settings in the harness's files
  (`model_provider` in `~/.codex/config.toml`; `modelProvider` removed from Antigravity's `settings.json`). Only those
  keys are changed, so other edits stay. A running guest is switched at login and logout; `/etc/glov/harness-subscriptions`
  lists the signed-in harnesses, and each Guest Service session brings a guest whose list differs (stopped at the
  switch, or reverted to a snapshot) up to vm.json. A start of the harness after the switch sees it.
- Login also allows the profile's `subscription.runtime` hosts; logout removes those no other harness of the VM needs,
  runs the profile's `signOut` in the guest first (so a revocation still gets the real token), then removes the
  secrets and their grants.
- A snapshot revert keeps vm.json's subscriptions as they are now, with their tokens' grants and runtime hosts, so
  signing in before or after taking the template snapshot makes no difference; the reverted guest is switched to
  match at its next Guest Service session. Other grants still follow the snapshot, keeping only those held now.
- The token can do what the subscription allows from any machine, so a hostile guest can spend its quota, and the
  request body is a channel out, as with a key. `glov secret logout`, or revoking the session at the provider, ends it.
- An answer field the endpoint does not map reaches the guest as it came: codex's `id_token` lands in
  `~/.codex/auth.json`. It names the account (email, account ID, plan) but is not what the harness sends to its API.
- On a Windows host the tokens stay on that host, and `glov vm reset` keeps the subscriptions as a revert does
  ([Isolated VMs on a Windows host](#isolated-vms-on-a-windows-host)).

### Harness profiles

`glov vm create <vm> --engine vz --isolated --harness claude` (repeatable; also `codex`, `antigravity`) sets a
coding-agent harness up for the VM, from a profile in `Sources/GlovCore/Harnesses/<name>.json`:

- The profile's `secrets` name each [secret](#secrets) the harness uses (its API key among them), the hosts it is
  sent to, and how it reaches the harness:
  - `placeholder`: the guest holds `glov-secret:<name>` in an environment variable (`env`) or at a `json` key path of
    a home file (`file`), and the egress proxy swaps in the value on the way to the secret's hosts.
  - `tmpfs`, for a harness that must hold the real value: the Guest Service writes it to `/run/glov/secrets/<name>`
    (mode 0400, the `glov` user's; [the protocol](guest-protocol.md#secrets-in-tmpfs)) while it is granted, at each
    session and after each grant, revoke or value change. `env` is then set to that path, or `file` is a symbolic link
    to it. The value never reaches the VM's disk, so a disk-only snapshot lacks it; a live snapshot is refused while
    the guest may hold one (stop the VM and snapshot it then).
- First boot appends the profile's `environment` and the secrets' variables to `/etc/environment` and writes its
  `files`, with the secrets' placeholders, into the `glov` user's home (`~/.claude/settings.json`,
  `~/.codex/config.toml`, `~/.gemini/antigravity-cli/settings.json`). Only a new VM gets them.
- First boot also signs the `glov` user in on the console (a getty autologin drop-in on tty1), so
  `glov exec <vm> --as user -- claude -p ...` has a session to run in and gets `/etc/environment`. Anyone with the VM's
  console is signed in as that user. A VM with codex also lifts Ubuntu's restriction on unprivileged user
  namespaces (`/etc/sysctl.d/60-glov-userns.conf`), which codex's sandbox needs; that restriction exists because
  user namespaces ease privilege escalation, so other harness VMs keep it.
- vm.json gets the profile's `runtime` hosts in `egress.allow`, each secret's hosts among them. `vm create` lists
  each secret, whether it is defined, and the `glov secret set` and `glov secret grant` commands still to run
  ([Model API keys](#model-api-keys)).
- `sessions` lists where the harness writes its conversation records in the guest (`~/.claude/projects`,
  `~/.codex/sessions`, `~/.gemini/antigravity-cli/conversations`). `vm create --harness` adds the read-write share
  `harness-sessions` from `<glov home>/.sessions/<vm>` (mode 0700) at `/mnt/glov-sessions`, unless `--no-sessions`
  is given, and `glov vm harness install` turns each record directory into a link to `/mnt/glov-sessions/<harness>/`,
  moving what it already held. Records then reach the Mac as they are written, and outlive a revert, a stop or a
  crash of the VM: shares are not part of a snapshot.
  - They are for review on the Mac, never for resuming there (`claude --resume` and the like): the guest is assumed
    hostile, and a record a Mac harness reads back is a prompt injection into the Mac. The folder is never under the Mac's
    own `~/.claude`, `~/.codex` or `~/.gemini`.
  - It is the one share allowed in the glov home: only under that name, and only as a folder of `.sessions` (a dot
    name is never a VM's, so `glov vm delete` leaves it). It exposes no VM directory; a link the guest makes in it
    to a VM's disk or `secrets/` resolves outside the share, which every host-side copy refuses.
  - Records hold what the task read and wrote, so they are as sensitive as the project; they hold no secret value,
    since the guest has only placeholders.
  - glov cannot cap what the guest writes there; `glov vm status <vm>` shows how many files and bytes it holds.
    `glov vm delete` keeps the folder and says where it is; remove it when no longer needed.
  - To stop keeping records on an existing VM, `glov share remove <vm> harness-sessions` while it is stopped, start
    it, and run `glov vm harness install` again, which makes the record directories plain ones in the guest.
- `install` lists the runtime hosts plus where the harness installs from, for `glov vm harness install` (below).
- `hostConfig` lists the owner's own config on the Mac that `glov vm harness install` copies into the `glov` user's
  home after the install, so a task follows the same instructions and skills as on the Mac:

  | Harness | Copied from `~` (when present) |
  | --- | --- |
  | `claude` | `.claude/CLAUDE.md`, `.claude/rules`, `.claude/skills`, `.claude/agents`, `.claude/commands`, `.claude/output-styles` |
  | `codex` | `.codex/AGENTS.override.md`, `.codex/AGENTS.md`, `.agents/skills`, `.codex/agents`, `.codex/prompts` |
  | `antigravity` | `.gemini/AGENTS.md`, `.gemini/GEMINI.md`, `.gemini/config/AGENTS.md`, `.gemini/config/GEMINI.md`, `.gemini/config/rules`, `.gemini/config/skills`, `.gemini/config/agents`, `.gemini/antigravity-cli/skills`, `.gemini/skills` |

  - Symbolic links are followed wherever they point, so a skill linked from `~/.agents/skills` arrives as a plain
    directory; a broken link or a cycle is left out and named. A file named like a credential store
    (`.credentials.json`, `auth.json`, `antigravity-oauth-token`, `oauth_creds.json`, `credentials.json`, `.env`) is
    left out wherever it is, and a profile may not name one. Settings, hooks and MCP config are not copied: glov writes
    the harness's settings itself, and the Mac's would name Mac paths. A file a copied instruction refers to
    elsewhere (such as `~/.agents/rules`) is not copied unless a profile lists it.
  - It is a copy, never a mount, so nothing the guest does reaches the Mac's config. Each install removes the earlier
    copy and copies again; the whole copy may hold at most 10,000 files and 64 MiB. `--no-host-config` copies none of
    it. The copy is made through a staging folder in `<home>/scratch` that is removed afterwards.
  - Whatever is copied, a hostile guest can read and send out through the allowlist: keep secrets out of these files.
- `notes` record what the harness still tries that is denied, and what it cannot turn off. Each profile names the
  harness version it was last verified against and the vendor docs it relies on; re-verify on a new release.

### Harness template and tasks

Besides the template below, `glov claude`, `glov codex` and `glov agy` run a harness interactively on the current project, in a VM of
the project's own that shares the working tree read-write at the same path (`/Users/...` or `/Volumes/...` in the
guest); see [isolated-harness-vms.md](isolated-harness-vms.md#on-the-current-project) for what that exposes.

Prepare a harness VM once, then run each task from the same clean state on its own clone of the project:

```sh
# Template, once
glov vm create agent --engine vz --isolated --harness claude --harness codex
glov vm start agent
glov vm harness install agent       # install allowlist only while it runs; runtime allowlist afterwards
glov exec agent --as user -- git config --global user.name "glov agent"
glov exec agent --as user -- git config --global user.email agent@agent.invalid
glov vm stop agent
glov snapshot create agent ready # disk-only, with the runtime allowlist

# Each task
glov snapshot revert agent ready # disk, and the allowlist as it was at the snapshot (grants: below)
git clone --no-local ~/src/proj ~/tasks/t1
glov share add agent task ~/tasks/t1 /mnt/task
glov vm start agent
until glov vm status agent | grep -q "share task at /mnt/task: mounted"; do sleep 2; done
until glov exec agent --as user -- true; do sleep 2; done
glov exec agent --as user --cwd /mnt/task -- claude -p "..."     # or codex exec "..."
glov exec agent --as user --cwd /mnt/task -- sh -c 'git add -A && git commit -m "t1" && git bundle create .git/result.bundle HEAD'
glov vm stop agent
glov share remove agent task
git -C ~/src/proj fetch ~/tasks/t1/.git/result.bundle HEAD:refs/heads/t1
```

- The snapshot is taken stopped, so reverting restores the disk without a running guest. VZ applies share changes at
  the next start, which is why each task adds its share while the VM is stopped and boots after.
- The daemon mounts a share once the Guest Service is ready, so wait for `mounted` in `glov vm status` before the
  task. `glov exec --as user` also needs the console sign-in, a few seconds after the Guest Service is ready; until
  then it exits 75, so wait for it to succeed first (`glov vm harness install` waits by itself).
- Nobody answers a permission prompt in a task, so the profiles hand the decision to each harness's automatic review
  where it has one: claude runs in `auto` permission mode, and codex runs commands in its own sandbox with escalations
  decided by `auto_review` (a VM with codex allows the unprivileged user namespaces that sandbox needs). agy's automatic
  mode is not available to API-key accounts, so it edits files and runs commands without asking
  (`toolPermission: always-proceed`); the isolated VM is its boundary. Change a template's settings files before
  taking its snapshot, or pass a stricter mode per run.
- `glov vm harness install` takes the install hosts away again even when interrupted (Ctrl-C, a closed terminal or
  pipe); check `glov egress list` before `snapshot create`, since the snapshot keeps the allowlist it is taken with.
- A revert restores the snapshot's allowlist, but of its secret grants only those the VM still has: a secret revoked
  since stays revoked, and one granted since is dropped. Grant it again after the revert if the task needs it.
- The clone is the guest's; the working repo on the Mac is never shared. Never run git inside the clone on the Mac:
  the guest can write its hooks and config. Fetching its bundle from the working repo reads only the bundle file.
- An MCP client has `snapshot_revert`, `vm_start`, `vm_status`, `vm_stop` and `exec` (with `"as": "user"` and `cwd`)
  for the guest steps; the Mac steps (the clone, `glov share add`/`remove`, `git fetch`) have no MCP tool and run in a
  shell on the Mac.
- A task stopped before `share remove` leaves its share on the VM, and shares are not part of a snapshot: check
  `glov share list agent` before the next task. A snapshot taken before glov saved egress with it restores no
  allowlist; take the template snapshot again.
- Delete the clone when the result is fetched. `glov vm harness install` again, then a new snapshot, updates the harnesses.

## Forwarding ports to a QEMU guest

`glov vm create <vm> --forward HOST:GUEST` (repeatable) or `portForwards` in `vm.json`:

```json
"sshPort": 2222,
"portForwards": [{ "host": 13389, "guest": 3389 }]
```

Forwards are TCP and bind `127.0.0.1` only, so nothing is exposed beyond this Mac. They take effect on the next start.

## SSH into a VZ guest

`glov ssh <vm>` reaches a VZ guest over vsock, with or without a network device: it runs `/usr/bin/ssh` with
`ProxyCommand=glov ssh-relay <vm>`, which asks the daemon for a stream to the Guest Service's vsock port 4025, which
pipes it to the guest's `127.0.0.1:22` (see [the protocol](guest-protocol.md#ssh-over-vsock)). No guest address is
looked up. A QEMU VM connects to `127.0.0.1:<sshPort>`. Host keys go to `<vm>/known_hosts` under the alias `<vm>`.

- The host connects in; the guest cannot open anything toward the Mac through this path.
- For an isolated VM, `glov ssh` also forces `ClearAllForwardings=yes`, `ForwardAgent=no`, `ForwardX11=no`, `Tunnel=no`,
  `PermitLocalCommand=no` and `EnableEscapeCommandline=no`, ahead of any option you pass or `~/.ssh/config` sets (ssh
  keeps the first value), and adds `-a -x -S none` after your options, since `-A`, `-X`, `-Y` and `-S` set theirs
  outright and the last one wins. So the session gives the guest no port, agent or X11 forward back to the Mac and
  shares no master connection. `-L`/`-R`/`-D` are cleared too. A VM with a network device keeps them.
- A Guest Service from before this relay answers `glov ssh` with an error asking for `glov vm guest update <vm>`.

To find a VZ guest's address for other tools, the Guest Service's `exec` (`ip -4 addr show scope global`) is
authoritative; `/var/db/dhcpd_leases` lists the address under the VM's `macAddress`, but an entry can outlive the lease.

