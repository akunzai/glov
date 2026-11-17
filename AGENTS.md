# glov Developer Guidelines

glov lets AI agents drive Windows and Linux VMs on Apple silicon Macs over MCP, and runs coding-agent harnesses in
isolated Linux VMs whose only way out is an egress proxy on the host. Windows runs on QEMU + HVF, Linux on
Virtualization.framework, with a Swift host daemon (`glovd`) and a Go Guest Service. The Windows edition
(`windows/`, Go) runs Linux VMs on a Windows host through the Host Compute Service, with the same `glov` surface for
the features it has.

## Repo-wide constraints
- Docs, code comments and commit messages are in English.
- Never name or reference the owner's work projects or internal package feeds; validate against `samples/HelloFramework`.
- Research on other VM products and benchmark data stays out of the repo.
- VM data directories hold `secrets/` (guest passwords, SSH keys); never print their contents.

## Pointers
- When changing the Guest Service or its host channel, read `docs/guest-protocol.md`
- When changing guest networking or port forwards, read `docs/networking.md`
- When filing or triaging an issue, read `docs/agents/issue-tracker.md`
- When opening a pull request, read `docs/agents/pull-request.md`
- Before running or reporting verification, read `docs/agents/verification.md`
- When changing `glov` commands, errors or recovery steps, update the user-facing agent skill in `skills/glov/`

## Prevent Recurrence
- **Candidate**: Name who hits this again, in which file, on what change. No such scenario, nothing to propose.
- **Promote**: Offer the first tier that reaches them and only that one, pending confirmation — enforce it (assert/type/test) with its size quoted, else a comment at that site, else an agent-facing doc (`docs/agents/<topic>.md`, else `docs/agents/lessons-learned.md`) with one backtick-path line under Pointers and one sentence on why the tiers above cannot hold it.
- **Prune**: When adding to a file, audit the rest of it in the same pass. Drop entries once stale (obsolete version, now enforced, duplicated, or a transcript) — not by a fixed count.
