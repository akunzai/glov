import ArgumentParser
import Foundation
import GlovDaemonAPI

/// The build stamp tools/build.sh puts in the bundled Guest Service manifests (`tools/version.sh`: the VERSION file plus the commit),
/// which sit beside glov and glovd and so identify this build. A binary outside such a layout is a dev build.
enum GlovVersion {
    static let string: String = {
        struct Manifest: Decodable { var version: String }
        let path = GuestServiceDistribution.directory + "/linux-arm64/manifest.json"
        guard let data = FileManager.default.contents(atPath: path),
              let manifest = try? JSONDecoder().decode(Manifest.self, from: data), !manifest.version.isEmpty else { return "dev" }
        return manifest.version
    }()
}

struct VersionCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "version",
        abstract: "Show the glov build and whether the daemon it talks to speaks its protocol.",
        discussion: "glov --version prints only the build. With --remote, the daemon is the remote Mac's.")

    func run() async throws {
        print("glov \(GlovVersion.string) (protocol \(DaemonProtocol.version))")
        do {
            let info = try await daemonClient().call(Method.daemonInfo).decode(Results.DaemonInfo.self)
            let verdict = info.protocolVersion == DaemonProtocol.version ? "compatible" : "INCOMPATIBLE: upgrade glov or run host install"
            print("glovd protocol \(info.protocolVersion) (\(verdict))")
        } catch {
            print("glovd: not reachable (\(error))")
        }
    }
}
