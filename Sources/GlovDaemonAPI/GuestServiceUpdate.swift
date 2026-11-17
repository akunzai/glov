import CryptoKit
import Foundation
import GlovCore

/// The operating system and CPU a Guest Service binary was built for, read from its executable header.
public struct GuestServiceBinary: Equatable, Sendable, CustomStringConvertible {
    public var os: String  // "windows" or "linux"
    public var arch: String  // "arm64", "amd64", ...
    public var description: String { "\(os) \(arch)" }

    public struct Unreadable: Error, CustomStringConvertible {
        public var description: String
    }

    /// PE (Windows) or ELF (Linux); anything else, a Mach-O included, is refused.
    public static func inspect(path: String) throws -> GuestServiceBinary {
        guard let data = FileManager.default.contents(atPath: path) else { throw Unreadable(description: "cannot read \(path)") }
        return try inspect(data, path: path)
    }

    public static func inspect(_ data: Data, path: String = "the file") throws -> GuestServiceBinary {
        let b = [UInt8](data.prefix(1 << 16))
        func u16(_ at: Int) -> Int? { at + 2 <= b.count ? Int(b[at]) | Int(b[at + 1]) << 8 : nil }
        func u32(_ at: Int) -> Int? { at + 4 <= b.count ? Int(b[at]) | Int(b[at + 1]) << 8 | Int(b[at + 2]) << 16 | Int(b[at + 3]) << 24 : nil }
        if b.starts(with: [0x4d, 0x5a]), let pe = u32(0x3c), pe + 6 <= b.count, Array(b[pe..<pe + 4]) == [0x50, 0x45, 0, 0], let machine = u16(pe + 4) {
            let arch = [0xAA64: "arm64", 0x8664: "amd64", 0x14c: "386"][machine] ?? String(format: "machine 0x%x", machine)
            return GuestServiceBinary(os: "windows", arch: arch)
        }
        if b.starts(with: [0x7f, 0x45, 0x4c, 0x46]), b.count > 19, b[5] == 1, let machine = u16(18) {
            let arch = [183: "arm64", 62: "amd64", 3: "386", 40: "arm"][machine] ?? "machine \(machine)"
            return GuestServiceBinary(os: "linux", arch: arch)
        }
        throw Unreadable(description: "\(path) is not a Windows (PE) or Linux (ELF, little-endian) executable")
    }

    public static func sha256(path: String) throws -> String {
        let h = try FileHandle(forReadingFrom: URL(fileURLWithPath: path))
        defer { try? h.close() }
        var hasher = SHA256()
        while let chunk = try h.read(upToCount: 1 << 20), !chunk.isEmpty { hasher.update(data: chunk) }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }
}

public struct GuestServiceUpdateError: Error, CustomStringConvertible {
    public var description: String
    public init(_ description: String) { self.description = description }
}

/// Puts a Guest Service binary on a running VM and waits until the new Guest Service answers; see `glov vm guest update`.
///
/// Uses the daemon's status, exec and copy operations, directly inside VMGuestService or over DaemonClient. The host-side
/// allowlist applies: the binary is copied into `<home>/scratch` first. Installing stops the Guest Service, which ends the exec
/// that started it, so `install` runs detached: a one-shot SYSTEM scheduled task on Windows, a transient `systemd-run`
/// unit on Linux.
public struct GuestServiceUpdate: Sendable {
    public enum Outcome: Codable, Equatable, Sendable {
        case upToDate(version: String)
        case updated(from: String, to: String)
    }

    public var vm: String
    public var binary: String
    public var home: GlovHome
    private var readStatus: @Sendable () async throws -> Results.VMInfo
    private var execute: @Sendable ([String], Double) async throws -> Results.ExecRun
    private var copy: @Sendable (String, String) async throws -> Void
    public var targetVersion: String?
    /// Seconds to wait for the new Guest Service after `install` was started.
    public var timeout: Double = 120
    public var pollInterval: Duration = .milliseconds(250)
    public var log: @Sendable (String) -> Void = { _ in }
    /// Tests run the daemon without a VM process, so its status never says "running".
    var adjustStatus: @Sendable (Results.VMInfo) -> Results.VMInfo = { $0 }

    public init(vm: String, binary: String, home: GlovHome, client: DaemonClient) {
        self.init(vm: vm, binary: binary, home: home,
            status: { try await client.call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self) },
            execute: { argv, timeout in
                try await client.call(Method.execRun, Params.Exec(vm: vm, argv: argv, timeout: timeout), as: Results.ExecRun.self)
            }, copy: { source, target in
                _ = try await client.call(Method.copyToVM, Params.CopyToVM(vm: vm, hostPath: source, guestPath: target), as: Results.Copy.self)
            })
    }

    public init(vm: String, binary: String, home: GlovHome,
        status: @escaping @Sendable () async throws -> Results.VMInfo,
        execute: @escaping @Sendable ([String], Double) async throws -> Results.ExecRun,
        copy: @escaping @Sendable (String, String) async throws -> Void
    ) {
        self.vm = vm; self.binary = binary; self.home = home
        self.readStatus = status; self.execute = execute; self.copy = copy
    }

    // MARK: guest commands (internal for tests)

    static let linuxInstalled = "/usr/local/bin/glov-guest"
    // Next to the installed binary, since that filesystem allows exec; Debian mounts /run and often /tmp noexec.
    static let linuxStaged = linuxInstalled + ".next"
    // C:\ProgramData\glov is the Guest Service's own directory, created with an ACL for SYSTEM and Administrators only.
    static let windowsStageDir = #"C:\ProgramData\glov\update"#
    static let windowsTask = "GlovGuestServiceUpdate"

    static func stagedPath(os: String, file: String) -> String { os == "windows" ? windowsStageDir + "\\" + file : linuxStaged }

    /// Hashes `path` in the guest; nil means the installed Guest Service (wherever the service runs it from).
    static func hashCommand(os: String, path: String?) -> [String] {
        if os == "windows" {
            let target = path.map { "'\($0)'" } ?? "$p"
            let find = path != nil ? "" : #"$p=(Get-CimInstance Win32_Service -Filter "Name='GlovGuestService'").PathName; if ($p -match '^"([^"]+)"') { $p=$matches[1] }; "#
            return ["powershell.exe", "-NoProfile", "-Command", find + "(Get-FileHash -Algorithm SHA256 -LiteralPath \(target)).Hash"]
        }
        return ["sha256sum", path ?? linuxInstalled]
    }

    /// The commands that start `install` outside the Guest Service's own process tree.
    static func launchCommands(os: String, staged: String) -> [[String]] {
        if os == "windows" {
            return [
                ["schtasks", "/create", "/f", "/tn", windowsTask, "/sc", "once", "/st", "00:00", "/ru", "SYSTEM", "/tr", "\(staged) install"],
                ["schtasks", "/run", "/tn", windowsTask],
            ]
        }
        return [["systemd-run", "--no-block", "--collect", "--unit", "glov-guest-update", staged, "install"]]
    }

    static func cleanupCommands(os: String) -> [[String]] {
        os == "windows"
            ? [["schtasks", "/delete", "/f", "/tn", windowsTask], ["cmd.exe", "/c", "rmdir", "/s", "/q", windowsStageDir]]
            : [["rm", "-f", linuxStaged]]
    }

    /// The hex digest in a hasher's output: `sha256sum`'s first field, or PowerShell's whole line.
    static func parseHash(_ output: String) -> String? {
        for line in output.split(whereSeparator: \.isNewline).reversed() {
            if let word = line.split(whereSeparator: \.isWhitespace).first, word.count == 64, word.allSatisfy(\.isHexDigit) { return word.lowercased() }
        }
        return nil
    }

    // MARK: running

    private func exec(_ argv: [String], timeout: Double = 30) async throws -> Results.ExecRun {
        try await execute(argv, timeout)
    }

    private func execOK(_ argv: [String], _ what: String, timeout: Double = 30) async throws -> String {
        let r = try await exec(argv, timeout: timeout)
        guard r.exited, r.code == 0 else {
            throw GuestServiceUpdateError("\(what) failed in the guest (\(r.exited ? "exit \(r.code ?? -1)" : "no exit")): \(r.tail.trimmingCharacters(in: .whitespacesAndNewlines))")
        }
        return r.tail
    }

    private func hash(os: String, path: String?, what: String) async throws -> String {
        let out = try await execOK(Self.hashCommand(os: os, path: path), what)
        guard let h = Self.parseHash(out) else { throw GuestServiceUpdateError("cannot read a SHA-256 from \(what): \(out.trimmingCharacters(in: .whitespacesAndNewlines))") }
        return h
    }

    private func status() async throws -> Results.VMInfo {
        adjustStatus(try await readStatus())
    }

    /// The Guest Service the update talks to, or why there is none.
    public static func requireReadyGuestService(_ info: Results.VMInfo) throws -> Results.GuestServiceStatus {
        switch info.state {
        case "running": break
        case "installing": throw GuestServiceUpdateError("VM \(info.name) is still installing; wait for it with `glov vm wait \(info.name) guest-ready --timeout 3600`")
        case "install-failed": throw GuestServiceUpdateError("the install of VM \(info.name) failed\(info.installFailure.map { ": " + $0 } ?? ""); delete it and create it again")
        default: throw GuestServiceUpdateError("VM \(info.name) is not running (\(info.state)); start it with `glov vm start \(info.name)`")
        }
        guard let agent = info.agent, agent.state != "disabled" else {
            throw GuestServiceUpdateError("VM \(info.name) has no Guest Service to update (\"agent\": false in its vm.json)")
        }
        guard agent.state == "ready", let os = agent.os, !os.isEmpty else {
            throw GuestServiceUpdateError("the Guest Service of VM \(info.name) is not ready (\(agent.state)\(agent.problem.map { ": " + $0 } ?? "")); wait with `glov vm wait \(info.name) guest-ready`")
        }
        return agent
    }

    /// A read-only comparison: no file is copied and no service is restarted.
    public func check() async -> Results.GuestServiceCheck {
        var result = Results.GuestServiceCheck(state: "unknown", targetVersion: targetVersion)
        do {
            let info = try await status()
            result.currentVersion = info.agent?.version
            let agent = try Self.requireReadyGuestService(info)
            let os = agent.os ?? ""
            try await Blocking.run { try validateBinary(os: os) }
            let hostHash = try await Blocking.run { try GuestServiceBinary.sha256(path: binary) }
            let installed = try await hash(os: os, path: nil, what: "reading the installed Guest Service")
            result.state = installed == hostHash ? "up-to-date" : "update-available"
        } catch { result.problem = "\(error)" }
        return result
    }

    private func validateBinary(os: String) throws {
        let kind = try GuestServiceBinary.inspect(path: binary)
        guard kind.os == os else { throw GuestServiceUpdateError("\(binary) is a \(kind) binary, but VM \(vm) runs \(os); nothing was copied") }
        guard kind.arch == "arm64" else { throw GuestServiceUpdateError("\(binary) is built for \(kind.arch), but VM \(vm) is arm64; nothing was copied") }
    }

    public func run() async throws -> Outcome {
        let info = try await status()
        let agent = try Self.requireReadyGuestService(info)
        let os = agent.os ?? ""
        let oldVersion = agent.version ?? ""
        try await Blocking.run { try validateBinary(os: os) }
        let kind = GuestServiceBinary(os: os, arch: "arm64")

        let hostHash = try await Blocking.run { [binary] in try GuestServiceBinary.sha256(path: binary) }
        let installedHash = try await hash(os: os, path: nil, what: "reading the installed Guest Service")
        if installedHash == hostHash {
            log("\(info.name): the installed Guest Service (version \(oldVersion)) is already this binary")
            return .upToDate(version: oldVersion)
        }

        let file = os == "windows" ? "glov-guest.exe" : "glov-guest"
        let staged = Self.stagedPath(os: os, file: file)
        let scratch = home.path + "/scratch/agent-update-" + UUID().uuidString
        try FileManager.default.createDirectory(atPath: scratch, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: scratch) }
        try FileManager.default.copyItem(atPath: binary, toPath: scratch + "/" + file)

        log("\(info.name): copying the Guest Service (\(kind)) into the guest")
        var launched = false
        do {
            try await copy(scratch + "/" + file, staged)
            if os == "linux" { _ = try await execOK(["chmod", "755", staged], "making the Guest Service executable") }
            guard try await hash(os: os, path: staged, what: "checking the copy") == hostHash else {
                throw GuestServiceUpdateError("the copy in the guest differs from \(binary); nothing was installed")
            }
            let newVersion = try await execOK([staged, "--version"], "running the new Guest Service with --version").trimmingCharacters(in: .whitespacesAndNewlines)
            log("\(info.name): installing Guest Service version \(newVersion) over \(oldVersion)")
            launched = true
            for argv in Self.launchCommands(os: os, staged: staged) { _ = try await execOK(argv, "starting the install") }
            try await awaitUpdate(os: os, hostHash: hostHash, oldVersion: oldVersion, newVersion: newVersion, oldConnection: agent.connectionID)
            await cleanup(os: os)
            return .updated(from: oldVersion, to: newVersion)
        } catch {
            if !launched { await cleanup(os: os) }
            throw error
        }
    }

    private func cleanup(os: String) async {
        for argv in Self.cleanupCommands(os: os) { _ = try? await exec(argv) }
    }

    /// Returns once a Guest Service that restarted reports `newVersion` from a file with `hostHash`.
    private func awaitUpdate(os: String, hostHash: String, oldVersion: String, newVersion: String, oldConnection: String?) async throws {
        let deadline = ContinuousClock.now + .seconds(timeout)
        var interrupted = false
        while ContinuousClock.now < deadline {
            try await Task.sleep(for: pollInterval)
            guard let a = (try? await status())?.agent else { interrupted = true; continue }
            if a.state != "ready" { interrupted = true; continue }
            // Windows can reopen the port between polls without changing its build label or guest boot ID.
            let reconnected = oldConnection != nil && a.connectionID != nil && a.connectionID != oldConnection
            guard interrupted || reconnected || a.version != oldVersion else { continue }
            guard let installed = try? await hash(os: os, path: nil, what: "reading the installed Guest Service") else { continue }
            guard installed == hostHash else {
                throw GuestServiceUpdateError(
                    "the Guest Service restarted, but the installed file is not the new binary (the install was rolled back or failed): "
                        + (os == "windows" ? #"see C:\ProgramData\glov\guest-service.log in the guest"# : "see `journalctl -u glov-guest-update` in the guest"))
            }
            guard a.version == newVersion else {
                throw GuestServiceUpdateError("the new Guest Service is installed, but the running Guest Service still reports version \(a.version ?? "?") instead of \(newVersion)")
            }
            return
        }
        throw GuestServiceUpdateError(
            "timed out after \(Int(timeout)) s waiting for the new Guest Service" + (interrupted ? " to answer" : ": the Guest Service never restarted")
                + "; check `glov vm status \(vm)` and the install log in the guest")
    }
}
