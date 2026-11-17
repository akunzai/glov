import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct TrustCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "trust", abstract: "Give an isolated VM extra CA certificates to trust, such as a private network's internal CA.",
        discussion: """
            A certificate goes into the guest's system trust store (/usr/local/share/ca-certificates/glov-trust/, then \
            update-ca-certificates), which curl, git, Go, Python and Node (NODE_EXTRA_CA_CERTS) read. The egress proxy also \
            trusts it for this VM's intercepted connections (hosts a granted secret is bound to), so the Mac does not need it. \
            A running VM is updated at once; a stopped one when it next starts. Pair it with glov egress allow <vm> <host> --private.
            """,
        subcommands: [List.self, Add.self, Remove.self])

    struct List: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Print the VM's trusted certificates.", aliases: ["ls"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report { printTrust(try await daemonClient().call(Method.trustList, Params.Trust(vm: vm), as: Results.Trust.self)) }
        }
    }

    struct Add: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Add a PEM certificate (one per file), or replace the one with that name.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "PEM file holding one certificate.", completion: .file()) var file: String
        @Option(help: "Name to list and remove it by (A-Z a-z 0-9 . _ -); default: the file name without its extension.") var name: String?
        func run() async throws {
            try await report {
                let url = URL(fileURLWithPath: file)
                let pem = try String(contentsOf: url, encoding: .utf8)
                let label = name ?? url.deletingPathExtension().lastPathComponent
                printTrust(try await daemonClient().call(Method.trustAdd, Params.Trust(vm: vm, name: label, certificate: pem), as: Results.Trust.self))
            }
        }
    }

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Remove a certificate by name.", aliases: ["rm"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Name as listed.") var name: String
        func run() async throws {
            try await report { printTrust(try await daemonClient().call(Method.trustRemove, Params.Trust(vm: vm, name: name), as: Results.Trust.self)) }
        }
    }
}

/// `<name>  <subject>  until <date>  sha256 <first 16 hex>[  (not a CA)]`, one per line.
private func printTrust(_ r: Results.Trust) {
    guard !r.certificates.isEmpty else { return print("\(r.vm): no trusted certificates") }
    for c in r.certificates {
        let until = c.notAfter.map { " until " + $0.formatted(.iso8601.year().month().day()) } ?? ""
        print("\(c.name)  \(c.subject)\(until)  sha256 \(c.sha256.prefix(16))\(c.authority ? "" : "  (not a CA)")")
    }
}
