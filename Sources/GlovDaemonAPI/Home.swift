import Foundation
import GlovCore

public enum HomeError: Error, Equatable, CustomStringConvertible {
    case invalidName(String)
    case noSuchVM(String, known: [String])
    case ambiguous(String, matches: [String])
    case exists(String)
    case missingFirmware(String)

    public var description: String {
        switch self {
        case .invalidName(let n): "invalid VM name \(String(reflecting: n)): use letters, digits, '.', '_' or '-', not starting with '.'"
        case .noSuchVM(let n, let known):
            "no VM named \(String(reflecting: n))" + (known.isEmpty ? " (no VMs are configured)" : " (known: \(known.joined(separator: ", ")))")
        case .ambiguous(let n, let m): "VM name \(String(reflecting: n)) is ambiguous: \(m.joined(separator: ", "))"
        case .exists(let n): "VM \(String(reflecting: n)) already exists"
        case .missingFirmware(let p): "firmware code file not found: \(p)"
        }
    }
}

/// The glov root directory: one subdirectory per VM, each holding a `vm.json` (an encoded `VMConfig`).
public struct GlovHome: Sendable, Equatable {
    public let path: String

    public init(path: String) { self.path = path }

    /// `explicit`, else `$GLOV_HOME`, else `~/Library/Application Support/glov`.
    public static var defaultPath: String {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("glov").path
    }

    public static func resolve(explicit: String? = nil, environment: [String: String] = ProcessInfo.processInfo.environment) -> GlovHome {
        if let p = explicit ?? environment["GLOV_HOME"], !p.isEmpty { return GlovHome(path: (p as NSString).expandingTildeInPath) }
        return GlovHome(path: defaultPath)
    }

    public var socketPath: String { path + "/glovd.sock" }
    public var logPath: String { path + "/glovd.log" }
    public func directory(of name: String) -> String { path + "/" + name }
    func configPath(of name: String) -> String { directory(of: name) + "/vm.json" }

    public static func isValid(name: String) -> Bool {
        !name.isEmpty && !name.hasPrefix(".") && name.utf8.allSatisfy { c in
            (c >= 0x30 && c <= 0x39) || (c >= 0x41 && c <= 0x5a) || (c >= 0x61 && c <= 0x7a) || c == 0x2e || c == 0x5f || c == 0x2d
        }
    }

    /// Names of the subdirectories that hold a `vm.json`, sorted.
    public func vmNames() -> [String] {
        let entries = (try? FileManager.default.contentsOfDirectory(atPath: path)) ?? []
        return entries.filter { Self.isValid(name: $0) && FileManager.default.fileExists(atPath: configPath(of: $0)) }.sorted()
    }

    /// An exact name wins; otherwise a prefix that matches exactly one VM.
    public func resolve(_ reference: String) throws -> String {
        let names = vmNames()
        if names.contains(reference) { return reference }
        let matches = reference.isEmpty ? [] : names.filter { $0.hasPrefix(reference) }
        if matches.count == 1 { return matches[0] }
        if matches.isEmpty { throw HomeError.noSuchVM(reference, known: names) }
        throw HomeError.ambiguous(reference, matches: matches)
    }

    /// The directory name and location are authoritative, so a moved or renamed VM directory keeps working.
    public func loadConfig(_ name: String) throws -> VMConfig {
        var config = try JSONDecoder().decode(VMConfig.self, from: Data(contentsOf: URL(fileURLWithPath: configPath(of: name))))
        // An allowlist left at its default follows the directory to its new location.
        if config.hostPathAllowlist == VMConfig.defaultAllowlist(directory: config.directory) {
            config.hostPathAllowlist = VMConfig.defaultAllowlist(directory: directory(of: name))
        }
        config.name = name
        config.directory = directory(of: name)
        return config
    }

    /// Creates `<home>/<name>` (0700) for a new VM, refusing a name in use. The directory is made in one `mkdir`, so a
    /// caller that gets it back knows it created it, and may remove it on failure without deleting another VM's files.
    public func makeVMDirectory(_ name: String) throws -> String {
        guard Self.isValid(name: name) else { throw HomeError.invalidName(name) }
        try FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true)
        let dir = directory(of: name)
        guard mkdir(dir, 0o700) == 0 else {
            if errno == EEXIST { throw HomeError.exists(name) }
            throw ProvisionError("cannot create \(dir): \(String(cString: strerror(errno)))")
        }
        return dir
    }

    /// Writes `<home>/<name>/vm.json`, refusing to overwrite. For QEMU a missing UEFI variable store is created
    /// blank at the size of the firmware code, which is what pflash needs; VZ creates its own store on first start.
    public func create(_ config: VMConfig) throws {
        guard Self.isValid(name: config.name) else { throw HomeError.invalidName(config.name) }
        let fm = FileManager.default
        guard !fm.fileExists(atPath: configPath(of: config.name)) else { throw HomeError.exists(config.name) }
        let qemu = config.engine == .qemu
        guard !qemu || fm.fileExists(atPath: config.firmwareCode) else { throw HomeError.missingFirmware(config.firmwareCode) }
        var config = config
        config.directory = directory(of: config.name)  // the default vars.fd path follows `directory`, so build configs with it
        try fm.createDirectory(atPath: config.directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        if qemu, !fm.fileExists(atPath: config.firmwareVars), let size = try fm.attributesOfItem(atPath: config.firmwareCode)[.size] as? Int {
            fm.createFile(atPath: config.firmwareVars, contents: nil)
            let h = try FileHandle(forWritingTo: URL(fileURLWithPath: config.firmwareVars))
            try h.truncate(atOffset: UInt64(size))
            try h.close()
        }
        try write(config)
    }

    /// The only way to change an existing VM's `vm.json`: reads it, applies `change` and writes it back while holding the
    /// VM's config lock, so an edit from another process (`glov share add` while glovd edits the egress policy)
    /// is never lost. The lock is a `flock` on `<vm>/.vm.json.lock`, not on vm.json, which each write replaces. It
    /// blocks only while another writer holds it, for the length of one read and write; keep `change` short.
    @discardableResult
    public func update(_ name: String, _ change: (inout VMConfig) throws -> Void) throws -> VMConfig {
        guard Self.isValid(name: name), FileManager.default.fileExists(atPath: configPath(of: name)) else {
            throw HomeError.noSuchVM(name, known: vmNames())
        }
        let fd = open(directory(of: name) + "/.vm.json.lock", O_RDWR | O_CREAT | O_CLOEXEC, 0o600)
        guard fd >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        defer { close(fd) }  // closing the descriptor releases the lock
        while flock(fd, LOCK_EX) != 0 {
            guard errno == EINTR else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        }
        var config = try loadConfig(name)
        try change(&config)
        config.name = name
        try write(config)
        return config
    }

    private func write(_ config: VMConfig) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try encoder.encode(config).write(to: URL(fileURLWithPath: configPath(of: config.name)), options: .atomic)
    }
}
