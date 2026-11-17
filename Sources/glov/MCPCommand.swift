import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI
import MCP

struct MCPCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "mcp", abstract: "Serve the VM tools to an MCP client over stdio (talks to glovd).")

    func run() async throws {
        let server = await MCPTools.makeServer(client: daemonClient())
        try await server.start(transport: BlockingStdioTransport())
        await server.waitUntilCompleted()
    }
}

enum MCPTools {
    private static let frameHint =
        "Take a screenshot first: x and y are pixels of the most recent screenshot of this VM (origin top-left, full frame)."

    private static func prop(_ type: String, _ description: String) -> Value {
        .object(["type": .string(type), "description": .string(description)])
    }

    private static let vm = prop("string", "VM name or unique name prefix (see vm_list).")

    private static func tool(_ name: String, _ description: String, _ properties: [String: Value] = [:], required: [String] = ["vm"]) -> Tool {
        var all = properties
        if required.contains("vm") { all["vm"] = vm }
        return Tool(
            name: name, description: description,
            inputSchema: .object(["type": "object", "properties": .object(all), "required": .array(required.map { .string($0) })]),
            annotations: nil)
    }

    private static let button = prop("string", "left (default), right or middle.")
    private static let x = prop("integer", "Screenshot pixel, from the left.")
    private static let y = prop("integer", "Screenshot pixel, from the top.")

    /// MCP tool name to daemon method.
    static let methods: [String: String] = [
        "vm_list": Method.vmList, "vm_status": Method.vmStatus, "vm_configure": Method.vmConfigure,
        "vm_start": Method.vmStart, "vm_stop": Method.vmStop, "vm_reboot": Method.vmReboot,
        "vm_wait": Method.vmWait, "screenshot": Method.screenshot, "click": Method.click, "move": Method.move,
        "drag": Method.drag, "type": Method.type, "key": Method.key, "scroll": Method.scroll,
        "exec": Method.execRun, "exec_start": Method.execStart, "exec_read": Method.execRead, "exec_kill": Method.execKill,
        "file_read": Method.fileRead, "file_write": Method.fileWrite, "copy_to_vm": Method.copyToVM, "copy_from_vm": Method.copyFromVM,
        "snapshot_list": Method.snapshotList, "snapshot_create": Method.snapshotCreate,
        "snapshot_revert": Method.snapshotRevert, "snapshot_delete": Method.snapshotDelete, "share_sync": Method.shareSync,
        "egress_list": Method.egressList, "egress_allow": Method.egressAllow, "egress_remove": Method.egressRemove, "egress_log": Method.egressLog,
    ]

    private static let shellHint =
        "Runs argv directly with no shell: on Windows pass a shell explicitly, e.g. [\"powershell.exe\", \"-NoProfile\", \"-Command\", \"...\"] or [\"cmd.exe\", \"/c\", \"...\"]; on Linux [\"/bin/sh\", \"-c\", \"...\"]."
    private static let allowlistHint =
        "The host path must be inside the VM's allowlist (by default its shared folder and the scratch directory, both under the glov home); anything else is refused and the error lists the allowed directories."
    private static let jobId = prop("string", "Job id returned by exec_start or exec.")
    private static let execProperties: [String: Value] = [
        "argv": .object(["type": "array", "items": .object(["type": "string"]), "description": "Program and arguments, e.g. [\"msbuild.exe\", \"app.sln\"]."]),
        "cwd": prop("string", "Working directory in the guest."),
        "env": .object(["type": "object", "additionalProperties": .object(["type": "string"]), "description": "Extra environment variables."]),
        "as": prop("string", "\"system\" (default: the Guest Service's account) or \"user\" (the signed-in console user's desktop session, for GUI programs)."),
        "stdin": prop("string", "Text to write to the process's stdin; stdin is then closed."),
    ]

    static let tools: [Tool] = [
        tool("vm_list", "List the configured VMs, whether each is running and its engine (qemu: Windows; vz: Linux).", required: []),
        tool("vm_status", "Show VM state and guestService.update (bundled-build comparison: up-to-date, update-available, unknown; no automatic install). For a Windows VM that glov vm create set up, installing (Setup runs unattended; vm_wait guest-ready, screenshot to watch) or install-failed (delete and create it again)."),
        tool("vm_configure", "Reconfigure a stopped VM's CPU count, memory, virtual disk size (disk size only grows; never shrinks) or, for a Windows (QEMU) VM, its balloon device. Stop the VM with vm_stop first.",
            [
                "cpus": prop("integer", "vCPUs (at least 1)."),
                "memory": prop("string", "Memory size, e.g. 8G."),
                "diskSize": prop("string", "Virtual disk size, e.g. 120G (grows the disk, never shrinks)."),
                "balloon": prop("boolean", "Windows (QEMU) only: give the VM a virtio-balloon device (true) or remove it (false), which lets glov vm balloon hand guest memory back to macOS on glov-qemu. snapshot_revert refuses a live snapshot taken before the change until the setting is put back."),
            ]),
        tool("vm_start", "Start a VM. It keeps running when this MCP session ends. Follow with vm_wait, then screenshot."),
        tool("vm_stop", "Shut a VM down: a graceful power-down (waits up to 60 s, then forces) or, with force, immediately.",
            ["force": prop("boolean", "Skip the graceful shutdown.")]),
        tool("vm_reboot", "Restart a VM, e.g. after installing software that needs a restart, and wait until the new boot's Guest Service is ready: the result is vm_status's with the new bootId. Windows restarts through its Guest Service and is refused when that is not ready (then vm_stop, with force if needed, and vm_start); Linux is stopped and started. Refused while exec jobs, a share sync or a share mount run, unless force. On timeout it reports the states and forces nothing: take a screenshot before vm_stop with force. MCP clients time out around 60 s and a Windows restart takes longer: note bootId from vm_status first, pass a timeout under 50, and after a timeout poll vm_status until it is ready with another bootId rather than calling vm_reboot again.",
            [
                "timeout": prop("number", "Seconds to wait for the new boot (default \(Int(Params.Reboot.defaultTimeout)))."),
                "force": prop("boolean", "Interrupt running exec jobs, share syncs and share mounts instead of refusing."),
            ]),
        tool("vm_wait", "Wait until a VM is running, or (guest-ready) until its Guest Service answers, which a Windows guest can need minutes after boot for; while a VM is installing, guest-ready also waits for the install to finish. Stay under about 50 s per call and call again: MCP clients time out around 60 s.",
            [
                "state": prop("string", "\"running\" (default) or \"guest-ready\"."),
                "timeout": prop("number", "Seconds to wait (default 60)."),
            ]),
        tool("screenshot",
            "Capture the VM screen as a PNG. Take one before any click, move, drag or scroll: those coordinates refer to the pixels of the last screenshot, so re-take it after the screen changes. region crops the result but coordinates stay relative to the full frame.",
            [
                "maxWidth": prop("integer", "Scale down to this width (default 1280)."),
                "region": .object([
                    "type": "object", "description": "Crop, in full-frame screenshot pixels.",
                    "properties": .object(["x": prop("integer", "Left"), "y": prop("integer", "Top"), "width": prop("integer", "Width"), "height": prop("integer", "Height")]),
                    "required": ["x", "y", "width", "height"],
                ]),
            ]),
        tool("click", "Click at a position. \(frameHint)", ["x": x, "y": y, "button": button, "count": prop("integer", "2 for a double click (default 1).")], required: ["vm", "x", "y"]),
        tool("move", "Move the pointer. \(frameHint)", ["x": x, "y": y], required: ["vm", "x", "y"]),
        tool("drag", "Press at one position, drag to another and release. Coordinates are pixels of the last screenshot; take one first.",
            [
                "fromX": prop("integer", "Start x (screenshot pixel)."), "fromY": prop("integer", "Start y (screenshot pixel)."),
                "toX": prop("integer", "End x (screenshot pixel)."), "toY": prop("integer", "End y (screenshot pixel)."), "button": button,
            ], required: ["vm", "fromX", "fromY", "toX", "toY"]),
        tool("type", "Type text on a US keyboard layout (printable ASCII, newline and tab). Click the target field first.",
            ["text": prop("string", "Text to type.")], required: ["vm", "text"]),
        tool("key", "Press a key or chord, e.g. enter, esc, tab, f5, ctrl+alt+delete, win+r.",
            ["key": prop("string", "Key or chord joined with +.")], required: ["vm", "key"]),
        tool("scroll", "Scroll the wheel at a position. \(frameHint)",
            ["x": x, "y": y, "dx": prop("integer", "Ticks right (negative: left)."), "dy": prop("integer", "Ticks down (negative: up).")],
            required: ["vm", "x", "y"]),
        tool("exec",
            "Run a command in the guest and wait. \(shellHint) Returns after at most `timeout` seconds (default 30, max 50) either way: with the exit code, or, when the command is still running, with a job id to follow using exec_read (use exec_start for work you know is long, such as builds). The reply holds the exit code, the last 50 lines of output (stdout and stderr interleaved), the line count and the path of the full log on the host, never the whole output.",
            execProperties.merging(["timeout": prop("number", "Seconds to wait, default 30, at most 50. The command keeps running past it.")]) { $1 },
            required: ["vm", "argv"]),
        tool("exec_start",
            "Start a command in the guest and return a job id at once, for long work such as builds or installs. Follow it with exec_read, stop it with exec_kill. \(shellHint)",
            execProperties, required: ["vm", "argv"]),
        tool("exec_read",
            "Read a job's output from a cursor (a byte offset into its total output; start at 0 and pass back the returned cursor). Returns the new text, the new cursor and whether the job exited. If the cursor fell out of the 1 MiB in-memory window the reply says so and gives the host log path holding everything.",
            ["jobId": jobId, "cursor": prop("integer", "Byte offset to continue from (default 0)."), "maxBytes": prop("integer", "At most this many bytes (default 32768, max 262144).")],
            required: ["vm", "jobId"]),
        tool("exec_kill", "Kill a running job's process.", ["jobId": jobId], required: ["vm", "jobId"]),
        tool("file_read",
            "Read a UTF-8 text file in the guest. Files over 256 KiB are refused: use copy_from_vm for those and for binary files.",
            ["path": prop("string", "Path in the guest, e.g. C:\\work\\log.txt."), "maxBytes": prop("integer", "Refuse files larger than this (at most 262144).")],
            required: ["vm", "path"]),
        tool("file_write", "Write a UTF-8 text file in the guest, replacing it and creating missing directories.",
            ["path": prop("string", "Path in the guest."), "text": prop("string", "File content.")], required: ["vm", "path", "text"]),
        tool("copy_to_vm",
            "Copy a file or a directory tree from the host into the guest. A directory's contents land at guestPath (missing directories are created; symbolic links and empty directories are skipped). \(allowlistHint)",
            ["hostPath": prop("string", "Absolute path on the host."), "guestPath": prop("string", "Destination path in the guest.")],
            required: ["vm", "hostPath", "guestPath"]),
        tool("copy_from_vm",
            "Copy a file or a directory tree from the guest to the host. A directory's contents land at hostPath. \(allowlistHint)",
            ["guestPath": prop("string", "Source path in the guest."), "hostPath": prop("string", "Absolute destination path on the host.")],
            required: ["vm", "guestPath", "hostPath"]),
        tool("snapshot_list", "List a VM's snapshots: name, creation time and whether each is live (a running machine) or disk only."),
        tool("snapshot_create",
            "Snapshot the VM's system disk before a risky step. A running VM gets a live snapshot (memory, devices and TPM included, the guest keeps running); a stopped VM a disk-only one. UEFI variables are covered on vz VMs only.",
            ["name": snapshotName], required: ["vm", "name"]),
        tool("snapshot_revert",
            "Revert the VM to a snapshot. A live snapshot resumes the guest where it was (starting a stopped VM) and the Guest Service reconnects: vm_wait guest-ready, and the guest clock is corrected; commands running in the guest are lost. A disk-only snapshot needs the VM stopped (vm_stop) and leaves it stopped. UEFI variables are reverted on vz VMs only.",
            ["name": snapshotName], required: ["vm", "name"]),
        tool("snapshot_delete", "Delete a snapshot.", ["name": snapshotName], required: ["vm", "name"]),
        tool("share_sync",
            "Mirror a share's host folder (glov share add) into its guest folder on a running Windows VM, before a build for example: new and changed files are written (only when their SHA-256 differs), files deleted on the host are deleted in the guest, excluded paths are left alone. Files changed in the guest are overwritten and listed; files in use in the guest fail the sync and are named. A first sync of a large tree can take minutes; later ones move only what changed. A Linux (VZ) share is a live mount and is refused.",
            [
                "share": prop("string", "Share name, or name/subdir to sync one directory of it."),
                "verify": prop("boolean", "Rehash every file on both sides instead of trusting unchanged stat keys."),
            ], required: ["vm", "share"]),
        tool("egress_list", "Show an isolated VM's egress allowlist: the only hosts it can reach, through the egress proxy; or, when the user opened it (glov vm configure --egress open), that it may reach any public host."),
        tool("egress_allow",
            "Add hosts to an isolated VM's egress allowlist; applies to its next connection, no restart. An entry is a hostname, *.suffix (names below suffix) or an IP address, for ports 443 and 80, or host:port for that port only. Private and link-local addresses and the Mac's own stay refused; only the user can allow a private-network host (glov egress allow --private). Keep the list minimal: every allowed host is a channel out. Refused unless the user opted the VM in (glov egress mcp <vm> on): ask them first.",
            ["hosts": egressHosts], required: ["vm", "hosts"]),
        tool("egress_remove", "Remove hosts from an isolated VM's egress allowlist; applies to its next connection. Refused unless the user opted the VM in (glov egress mcp <vm> on).", ["hosts": egressHosts], required: ["vm", "hosts"]),
        tool("egress_log",
            "An isolated VM's egress connections, newest last: UTC time, allow/deny/fail, host:port, bytes in and out, reason. Pass the returned cursor back to get only newer ones.",
            [
                "denied": prop("boolean", "Only connections that did not go through (deny and fail)."),
                "cursor": prop("string", "From a previous egress_log answer: return only what came after it."),
                "limit": prop("integer", "Without a cursor: how many of the latest connections (default \(Params.EgressLogRead.defaultLimit))."),
            ]),
    ]
    private static let egressHosts: Value = .object(["type": "array", "items": .object(["type": "string"]), "description": "Entries, e.g. [\"api.example.com\", \"*.ubuntu.com\", \"registry.example.org:8443\"]."])
    private static let snapshotName = prop("string", "Snapshot name: A-Z a-z 0-9 . _ - (1 to 64 characters).")

    static func makeServer(client: DaemonClient) async -> Server {
        let server = Server(name: "glov", version: GlovVersion.string, capabilities: .init(tools: .init(listChanged: false)))
        await server.withMethodHandler(ListTools.self) { _ in .init(tools: tools) }
        await server.withMethodHandler(CallTool.self) { params in await call(client, params.name, params.arguments) }
        return server
    }

    private static func text(_ s: String) -> Tool.Content { .text(text: s, annotations: nil, _meta: nil) }

    static func call(_ client: DaemonClient, _ name: String, _ arguments: [String: Value]?) async -> CallTool.Result {
        guard let method = methods[name] else { return .init(content: [text("unknown tool \(name)")], isError: true) }
        do {
            var args = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(arguments ?? [:]))
            // The daemon refuses an MCP client's policy change unless the VM opted in; the client cannot unset this.
            if name == "egress_allow" || name == "egress_remove", case .object(var fields) = args {
                fields["mcp"] = .bool(true)
                args = .object(fields)
            }
            let result = try await client.call(method, args)
            if name == "screenshot" {
                let shot = try result.decode(Results.Screenshot.self)
                return .init(content: [
                    .image(data: shot.png, mimeType: "image/png", annotations: nil, _meta: nil),
                    text("\(shot.width)x\(shot.height) px; full frame \(shot.frameWidth)x\(shot.frameHeight). Coordinates use full-frame pixels."),
                ])
            }
            var named = ""
            if case .string(let n)? = args["name"] { named = n }
            switch name {
            case "exec": return .init(content: [text(describe(try result.decode(Results.ExecRun.self)))])
            case "exec_start":
                let id = try result.decode(Results.JobStarted.self).jobId
                return .init(content: [text("job \(id) started; read it with exec_read (jobId \"\(id)\", cursor 0)")])
            case "exec_read": return .init(content: [text(describe(try result.decode(Results.ExecRead.self)))])
            case "snapshot_list": return .init(content: [text(describeSnapshots(try result.decode(Results.SnapshotList.self)))])
            case "snapshot_create": return .init(content: [text("snapshot \(named) created")])
            case "snapshot_delete": return .init(content: [text("snapshot \(named) deleted")])
            case "snapshot_revert":
                let vm = try result.decode(Results.VMInfo.self)
                let live = vm.state == "running"
                return .init(content: [text("reverted; VM \(vm.name) is \(vm.state)" + (live ? "; vm_wait guest-ready before using exec or file tools" : ""))])
            case "file_read": return .init(content: [text(try result.decode(Results.FileRead.self).text)])
            case "share_sync": return .init(content: [text(describeSync(try result.decode(Results.ShareSync.self)))])
            case "egress_list", "egress_allow", "egress_remove": return .init(content: [text(describeAllowlist(try result.decode(Results.Egress.self)))])
            case "egress_log":
                let page = try result.decode(Results.EgressLogPage.self)
                let lines = page.records.isEmpty ? "no connections" : page.records.map(describeEgress).joined(separator: "\n")
                return .init(content: [text(lines + "\ncursor: \(page.cursor)")])
            default: return .init(content: [text(String(decoding: try JSONEncoder().encode(result), as: UTF8.self))])
            }
        } catch {
            return .init(content: [text("\(error is CocoaError ? error.localizedDescription : "\(error)")")], isError: true)
        }
    }

    static func describe(_ r: Results.ExecRun) -> String {
        var head = "job \(r.jobId): "
        if r.lost { head += "lost (the Guest Service reconnected and never reported its exit; it may still run in the guest)" }
        else if let code = r.exited ? r.code : nil { head += "exit code \(code)" }
        else { head += "still running (exec returned after its timeout; follow it with exec_read, stop it with exec_kill)" }
        let shown = r.tail.split(separator: "\n", omittingEmptySubsequences: false).count - (r.tail.hasSuffix("\n") ? 1 : 0)
        let lines = r.lineCount == 0 ? "no output" : r.lineCount > shown ? "\(r.lineCount) lines of output, last \(shown) below" : "\(r.lineCount) lines of output"
        return "\(head)\n\(lines); full log: \(r.logPath)" + (r.tail.isEmpty ? "" : "\n---\n" + r.tail)
    }

    static func describe(_ r: Results.ExecRead) -> String {
        var head = r.lost ? "lost (the Guest Service reconnected and never reported its exit)" : r.exited ? "exit code \(r.code ?? 0)" : "running"
        head += "; cursor \(r.cursor); full log: \(r.logPath)"
        if r.dropped { head += "\nthe output before this point fell out of the 1 MiB window; read the log file for it" }
        return r.data.isEmpty ? head : head + "\n---\n" + r.data
    }
}
