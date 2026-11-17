import ArgumentParser
import Foundation
import GlovDaemonAPI

struct GuestCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "guest", abstract: "Manage the Guest Service inside a VM.", subcommands: [Check.self, Update.self])

    struct Check: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Compare the Guest Service with the daemon's bundled build without changing the VM.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Option(name: .customLong("guest-service"), help: "Compare against a custom Guest Service binary instead.") var agent: String?

        func run() async throws {
            try await report {
                let result = try await daemonClient().call(Method.guestServiceCheck, Params.GuestService(vm: vm, binary: agent.map(guestServicePath)), as: Results.GuestServiceCheck.self)
                print("\(vm): \(result.description)")
            }
        }
    }

    struct Update: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Install the daemon's bundled Guest Service and wait until it answers.",
            discussion: "Nothing is changed when the SHA-256 matches. Running managed jobs block an update unless --force is given. No online version lookup is performed.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Option(name: .customLong("guest-service"), help: "Install a custom Windows/Linux arm64 Guest Service binary instead.") var agent: String?
        @Flag(help: "Allow interruption of running managed jobs.") var force = false
        @Option(help: "Seconds to wait for the new Guest Service after starting the install (0 < timeout <= 3600).") var timeout: Double = 120

        func validate() throws {
            guard timeout.isFinite, timeout > 0, timeout <= 3600 else { throw ValidationError("timeout must be between 0 and 3600 seconds") }
        }

        func run() async throws {
            try await report {
                let outcome = try await daemonClient().call(
                    Method.guestServiceUpdate, Params.GuestService(vm: vm, binary: agent.map(guestServicePath), force: force, timeout: timeout), as: GuestServiceUpdate.Outcome.self)
                switch outcome {
                case .upToDate(let version): print("\(vm): Guest Service \(version) is already this binary")
                case .updated(let from, let to): print("\(vm): Guest Service updated from \(from) to \(to) and answering")
                }
            }
        }
    }
}

private func guestServicePath(_ path: String) throws -> String {
    if remoteDaemon != nil {
        guard path.hasPrefix("/") else { throw ValidationError("Guest Service binary must be an absolute path on the remote Mac") }
        return path
    }
    return URL(fileURLWithPath: (path as NSString).expandingTildeInPath).standardizedFileURL.path
}
