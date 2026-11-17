import Foundation

/// Guest binaries shipped beside glov/glovd by tools/build.sh and host install.
public enum GuestServiceDistribution {
    public struct Target: Sendable {
        public var path: String
        public var version: String?
    }

    private struct Manifest: Decodable {
        var version: String
        var sha256: String
    }

    public static var directory: String {
        URL(fileURLWithPath: Bundle.main.executablePath ?? CommandLine.arguments[0])
            .resolvingSymlinksInPath().deletingLastPathComponent().appendingPathComponent("guest-services").path
    }

    /// Install beside the host executables, replacing obsolete generated bundles only after validation.
    public static func install(from sourceBin: String, to destinationBin: String) throws {
        let source = sourceBin + "/guest-services"
        let destination = destinationBin + "/guest-services"
        let targets = try ["windows", "linux"].map { try target(os: $0, directory: source) }
        let fm = FileManager.default
        let staged = destination + ".new-" + UUID().uuidString
        let backup = destination + ".old-" + UUID().uuidString
        defer { try? fm.removeItem(atPath: staged) }
        for target in targets {
            let dir = (target.path as NSString).deletingLastPathComponent
            let output = staged + "/" + (dir as NSString).lastPathComponent
            try fm.createDirectory(atPath: output, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try fm.copyItem(atPath: target.path, toPath: output + "/" + (target.path as NSString).lastPathComponent)
            try fm.copyItem(atPath: dir + "/manifest.json", toPath: output + "/manifest.json")
        }
        // Validate the copies as well: the source may have been rebuilt while it was copied.
        for os in ["windows", "linux"] { _ = try target(os: os, directory: staged) }
        let existed = fm.fileExists(atPath: destination)
        if existed { try fm.moveItem(atPath: destination, toPath: backup) }
        do { try fm.moveItem(atPath: staged, toPath: destination) } catch {
            if existed { try fm.moveItem(atPath: backup, toPath: destination) }
            throw error
        }
        if existed { try? fm.removeItem(atPath: backup) }
        let obsolete = destinationBin + "/agents"
        if fm.fileExists(atPath: obsolete) { try fm.removeItem(atPath: obsolete) }
    }

    public static func target(os: String, binary: String? = nil, directory: String = directory) throws -> Target {
        guard os == "windows" || os == "linux" else { throw GuestServiceUpdateError("unsupported guest OS: \(os)") }
        let dir = directory + "/\(os)-arm64"
        let path = binary ?? dir + (os == "windows" ? "/glov-guest.exe" : "/glov-guest")
        guard FileManager.default.fileExists(atPath: path) else {
            throw GuestServiceUpdateError("Guest Service not found at \(path); run tools/build.sh and glov host install, or pass --guest-service <path>")
        }
        let kind = try GuestServiceBinary.inspect(path: path)
        guard kind.os == os, kind.arch == "arm64" else { throw GuestServiceUpdateError("\(path) is \(kind); expected \(os) arm64") }
        // A custom build need not carry a manifest. Never infer a version from its filename.
        if binary != nil { return Target(path: path, version: nil) }
        do {
            let manifest = try JSONDecoder().decode(Manifest.self, from: Data(contentsOf: URL(fileURLWithPath: dir + "/manifest.json")))
            guard !manifest.version.isEmpty, manifest.sha256 == (try GuestServiceBinary.sha256(path: path)) else {
                throw GuestServiceUpdateError("bundled Guest Service does not match its manifest; rebuild with tools/build.sh")
            }
            return Target(path: path, version: manifest.version)
        } catch {
            throw GuestServiceUpdateError("cannot validate bundled \(os) Guest Service: \(error); run tools/build.sh and glov host install, or pass --guest-service <path>")
        }
    }
}
