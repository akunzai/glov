import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct EgressCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "egress", abstract: "Show and edit an isolated VM's egress allowlist, and what it tried to reach.",
        discussion: """
            An isolated VM (vm create --engine vz --isolated) reaches only the hosts in its allowlist (egress.allow in vm.json), \
            through the egress proxy; everything else is denied. An entry is a hostname, *.suffix (any name below suffix) or an IP \
            address, for ports 443 and 80, or with :port for that port only. Changes apply to the next connection, without a restart.
            A host must resolve to a public address, unless it was allowed with --private (listed with "(private)"): then RFC 1918, \
            CGNAT and unique local addresses count too. Loopback, link-local and the Mac's own addresses are always refused. \
            glov vm configure <vm> --egress open lets the VM reach any public host through the same proxy, keeping the allowlist \
            for a switch back (--egress restricted).
            """,
        subcommands: [List.self, Allow.self, Remove.self, Log.self, MCP.self])

    struct List: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Print the allowlist, one entry per line.", aliases: ["ls"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report { printAllowlist(try await daemonClient().call(Method.egressList, Params.Egress(vm: vm), as: Results.Egress.self)) }
        }
    }

    struct Allow: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Add hosts to the allowlist and print it.", aliases: ["add"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Hosts: name, *.suffix or IP address, each optionally with :port (default 443 and 80).") var hosts: [String]
        @Flag(
            name: .customLong("private"),
            help: "Let these hosts resolve to private-network addresses too (RFC 1918, CGNAT, unique local), for a VPN or self-hosted service. Exact names or addresses only, no *.suffix.")
        var privateNetworks = false
        func run() async throws {
            try await report {
                let p = Params.Egress(vm: vm, hosts: hosts, private: privateNetworks ? true : nil)
                printAllowlist(try await daemonClient().call(Method.egressAllow, p, as: Results.Egress.self))
            }
        }
    }

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Remove hosts from the allowlist and print it.", aliases: ["rm"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Entries as listed (spelling and case do not matter).") var hosts: [String]
        func run() async throws {
            try await report {
                printAllowlist(try await daemonClient().call(Method.egressRemove, Params.Egress(vm: vm, hosts: hosts), as: Results.Egress.self))
            }
        }
    }

    struct MCP: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Let MCP clients change the VM's allowlist (on), or not (off, the default).",
            discussion: "Off, egress_allow and egress_remove are refused, so an agent cannot widen the policy through glov mcp without you. glov mcp has no tool for this switch. An agent with a shell can still run glov itself.")
        enum State: String, ExpressibleByArgument, CaseIterable { case on, off }
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "on or off.") var state: State
        func run() async throws {
            try await report {
                let r = try await daemonClient().call(Method.egressMCP, Params.EgressMCP(vm: vm, enabled: state == .on), as: Results.Egress.self)
                print(r.mcpEdits ? "\(r.vm): MCP clients may change the egress allowlist" : "\(r.vm): only glov may change the egress allowlist")
            }
        }
    }

    struct Log: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Print the VM's egress connections: time (UTC), decision, target, bytes in and out, reason.",
            discussion: "deny: refused by the allowlist or because the host resolves to no public address; fail: allowed but unreachable. Metadata only, never bodies or headers. Kept in <vm>/egress.log and egress.log.1 (1 MiB each).")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Flag(help: "Only connections that did not go through (deny and fail).") var denied = false
        @Flag(name: .shortAndLong, help: "Keep printing new connections until interrupted.") var follow = false
        @Option(name: [.customShort("n"), .long], help: "How many of the latest connections to print first.") var lines = Params.EgressLogRead.defaultLimit
        func run() async throws {
            try await report {
                let client = daemonClient()
                var page = try await client.call(Method.egressLog, Params.EgressLogRead(vm: vm, denied: denied, limit: lines), as: Results.EgressLogPage.self)
                while true {
                    for record in page.records { print(describeEgress(record)) }
                    guard follow else { return }
                    fflush(stdout)
                    try await Task.sleep(for: .seconds(1))
                    page = try await client.call(Method.egressLog, Params.EgressLogRead(vm: vm, denied: denied, cursor: page.cursor), as: Results.EgressLogPage.self)
                }
            }
        }
    }
}

private func printAllowlist(_ r: Results.Egress) {
    if r.open == true { print(describeEgressMode(r)) }
    if r.open == true && r.allow.isEmpty { return }
    print(r.allow.isEmpty ? describeAllowlist(r) : markedEntries(r).joined(separator: "\n"))
}

/// `vm configure --egress`: open or restricted.
enum EgressModeOption: String, ExpressibleByArgument, CaseIterable { case open, restricted }

/// The VM's egress mode in one line.
func describeEgressMode(_ r: Results.Egress) -> String {
    r.open == true
        ? "\(r.vm): egress open: any public host, through the egress proxy; the allowlist applies again after glov vm configure \(r.vm) --egress restricted"
        : "\(r.vm): egress restricted: only the allowlist"
}

/// The entries, each allowed with `--private` followed by ` (private)`.
private func markedEntries(_ r: Results.Egress) -> [String] {
    let marked = Set(r.private ?? [])
    return r.allow.map { marked.contains($0) ? "\($0) (private)" : $0 }
}

/// `<UTC time> <decision> <target> in=<bytes> out=<bytes>[ (<reason>)]`.
func describeEgress(_ r: EgressRecord) -> String {
    "\(r.time.formatted(.iso8601)) \(r.summary)"
}

func describeAllowlist(_ r: Results.Egress) -> String {
    if r.open == true { return describeEgressMode(r) + (r.allow.isEmpty ? "" : "\nallowlist: " + markedEntries(r).joined(separator: ", ")) }
    return r.allow.isEmpty ? "\(r.vm): nothing allowed (every connection is denied)" : "\(r.vm) allows: " + markedEntries(r).joined(separator: ", ")
}
