import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct ShareCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "share", abstract: "Share host folders with a guest: synced into Windows, mounted live in Linux.",
        discussion: """
            A share is a host folder, a guest folder, ro or rw, and exclude patterns, stored in the VM's vm.json; a VM has \
            none unless added. `share sync` mirrors the host folder into the guest folder: new and changed files are \
            written (a file moves only when its SHA-256 differs), files missing on the host are deleted in the guest, and \
            paths the exclude patterns match are left alone on both sides. Syncing happens only when asked. A share also \
            joins the VM's copy allowlist: cp from the host may read every share, cp to the host may write rw shares only.

            A Linux (VZ) VM sees the host folder live instead: each share is a virtiofs device, mounted at its guest path \
            (default /mnt/<name>) by the daemon once the Guest Service is ready, so there is nothing to sync and no exclude. \
            Shares are attached when the VM starts: changes made while it runs apply at its next start.
            """,
        subcommands: [Add.self, Remove.self, List.self, Sync.self, Status.self])

    struct Add: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Add a share to a VM.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Share name: letters, digits, '.', '_' or '-'.") var name: String
        @Argument(help: "Host folder.") var hostPath: String
        @Argument(help: "Guest folder: C:\\work\\proj for Windows (a sync deletes what the host folder lacks there), required; /mnt/<name> by default for Linux.") var guestPath: String?
        @Flag(help: "Read-only: copies to the host never write into this folder.") var ro = false
        @Option(help: "Windows only: gitignore pattern left alone on both sides; a leading / anchors it to the share root. Repeatable.") var exclude: [String] = []

        func run() async throws {
            try await report {
                let share = VMConfig.Share(
                    name: name, hostPath: VMConfig.Share.absolute(hostPath, directory: FileManager.default.currentDirectoryPath), guestPath: guestPath ?? "",
                    mode: ro ? .ro : .rw, exclude: exclude)
                let config = try GlovHome.resolve().addShare(vm: vm, share)
                print("added share " + describe(try config.share(name)))
                await printNextStartNote(vm)
            }
        }
    }

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Remove a share. Files already synced stay in the guest.", aliases: ["rm"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Share name.") var name: String
        func run() async throws {
            try await report {
                try GlovHome.resolve().removeShare(vm: vm, name)
                print("removed share \(name)")
                await printNextStartNote(vm)
            }
        }
    }

    struct List: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "List a VM's shares.", aliases: ["ls"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report {
                let home = GlovHome.resolve()
                let shares = try home.loadConfig(try home.resolve(vm)).shares
                if shares.isEmpty { print("no shares") }
                for s in shares { print(describe(s)) }
                if !shares.isEmpty { await printNextStartNote(vm) }
            }
        }
    }

    struct Sync: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Mirror a share's host folder (or one subdirectory of it) into a Windows guest.",
            discussion: "A Linux (VZ) share is a live mount and cannot be synced. The VM must be running with its Guest Service ready. Files changed in the guest since the last sync are overwritten and listed; files in use in the guest fail the sync, which names them.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "<name> or <name>/<subdir>.") var share: String
        @Flag(help: "Rehash every file on both sides instead of trusting unchanged stat keys.") var verify = false

        func run() async throws {
            try await report {
                let r = try await daemonClient().call(Method.shareSync, Params.ShareSync(vm: vm, share: share, verify: verify ? true : nil), as: Results.ShareSync.self)
                print(describeSync(r))
            }
        }
    }
}

extension ShareCommand {
    struct Status: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Show the share syncs running on a VM, and the file the guest is on.",
            discussion: "Run it beside a share sync that seems stuck: a guest file whose bytes stop growing is one another program may hold. With --watch it repeats every 2 s until no sync runs.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Only this share.") var share: String?
        @Flag(name: .shortAndLong, help: "Repeat every 2 s until no sync runs.") var watch = false

        func run() async throws {
            try await report {
                while true {
                    let r = try await daemonClient().call(Method.shareStatus, Params.ShareStatus(vm: vm, share: share), as: Results.ShareStatus.self)
                    if r.syncs.isEmpty {
                        print("no share sync running")
                        return
                    }
                    for p in r.syncs { print(describeProgress(p)) }
                    guard watch else { return }
                    try await Task.sleep(for: .seconds(2))
                }
            }
        }
    }
}

/// One line per running sync: the host's stage, then what the guest is on.
func describeProgress(_ p: Results.SyncProgress) -> String {
    let mib = { (n: Int) in String(format: "%.1f MiB", Double(n) / 1_048_576) }
    var line = "\(p.share): \(p.stage) for \(Int(p.seconds)) s"
    if p.stage == "applying" { line += ", sent \(mib(p.sentBytes)) of \(mib(p.totalBytes))" }
    if let g = p.guest {
        line += "; guest \(g.phase == "manifest" ? "hashing" : "writing") \(g.path.isEmpty ? "(between files)" : g.path)"
            + " (\(g.files) files, \(mib(g.bytes)) in \(Int(g.seconds)) s)"
    } else if p.stage == "guest listing" {
        line += "; guest progress unavailable (between calls, or a Guest Service without sync.status: glov vm guest update)"
    }
    return line
}

/// For a running VZ VM: its shares are attached at start, so a change applies at the next one. Best effort: a daemon that
/// does not answer means the VM is not running.
private func printNextStartNote(_ vm: String) async {
    let home = GlovHome.resolve()
    guard let name = try? home.resolve(vm), (try? home.loadConfig(name))?.engine == .vz,
        let info = try? await daemonClient().call(Method.vmStatus, Params.VM(vm: name), as: Results.VMInfo.self), info.state == "running"
    else { return }
    print("note: \(name) is running; its shares are attached when it starts, so changes apply the next time it starts (vm stop, vm start)")
}

private func describe(_ s: VMConfig.Share) -> String {
    "\(s.name) (\(s.mode.rawValue)): \(s.hostPath) -> \(s.guestPath)" + (s.exclude.isEmpty ? "" : ", exclude \(s.exclude.joined(separator: " "))")
}

/// A sync's summary, then the paths written (+), deleted (-), guest changes overwritten (!) and host paths skipped (~).
func describeSync(_ r: Results.ShareSync) -> String {
    var lines = [
        "\(r.share) -> \(r.guestPath): \(r.written) written, \(r.deleted) deleted, \(r.unchanged) unchanged, \(r.bytes) bytes in "
            + String(format: "%.1f", r.seconds) + " s (hashed: \(r.hostHashed) on the host, \(r.guestHashed) in the guest)"
    ]
    func more(_ shown: Int, _ total: Int) { if total > shown { lines.append("  ... and \(total - shown) more") } }
    lines += r.writtenPaths.map { "+ " + $0 }
    more(r.writtenPaths.count, r.written)
    lines += r.deletedPaths.map { "- " + $0 }
    more(r.deletedPaths.count, r.deleted)
    if !r.overwritten.isEmpty { lines.append("changes made in the guest, overwritten:") }
    lines += r.overwritten.map { "! " + $0 }
    if !r.skipped.isEmpty { lines.append("not synced (links, special files or names Windows cannot hold):") }
    lines += r.skipped.map { "~ " + $0 }
    return lines.joined(separator: "\n")
}
