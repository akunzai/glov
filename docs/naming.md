# Naming

Accepted naming contract. Canonical terms are defined in the [glossary](../GLOSSARY.md).

- The project, product and brand name is `glov`, written in lowercase everywhere, including at the start of a sentence.
- `glov` is the only CLI executable. The host daemon is `glovd` (`glovd.exe` on Windows); on Windows the
  `glov-broker` service makes the HCS calls `glovd` cannot.
- Data roots: `~/Library/Application Support/glov` and `~/Library/Caches/glov` on a Mac, `%LOCALAPPDATA%\glov`
  (binaries in its `bin`) and, for glov-broker, `%ProgramFiles%\glov\broker` and `%ProgramData%\glov-broker` on a
  Windows host. Environment variables use the `GLOV_` prefix (`GLOV_HOME`, `GLOV_CACHE`).
- The MCP server and the agent skill are named `glov`.
- Use Guest Service for the resident guest component and reserve agent for AI agents. The guest executable is
  `glov-guest` (`glov-guest.exe` on Windows), the Linux service `glov-guest.service`, and the Windows service
  `GlovGuestService`. The guest user is `glov`.
- Public status JSON uses `guestService`; the readiness condition is `guest-ready`; host management RPC uses
  `guest.check` and `guest.update`.
- The guest wire uses the same terms: `guest.info`, `guest.ready` and `guest.stats`, a `version` field, the Windows
  virtio-serial port `org.glov.guest.0` and the host socket `guest.sock`. In `vm.json`, `guestService` attaches the
  Guest Service; `agentControl` is about AI agents and keeps its name.
- Renames do not keep aliases, migrations or legacy detection. VMs created under an earlier name are unsupported and
  must be recreated; nothing deletes them automatically.
