import Foundation

/// Owns the qemu and swtpm child processes of one VM and its QMP connection.
public actor VirtualMachine {
    public let config: VMConfig
    private let qemuPath: String
    private let swtpmPath: String?
    private var qemu: Process?
    private var swtpm: Process?
    private var qmp: QMPClient?
    /// A qemu this actor did not spawn (found through `qemu.pid` after a daemon restart).
    private var attachedPid: Int32?

    /// Resolves `qemu-system-aarch64` (glov-qemu first, see `findQEMU`) and `swtpm` when the config has a TPM on PATH,
    /// /opt/homebrew/bin or /usr/local/bin, because a LaunchAgent starts with a minimal PATH.
    public init(config: VMConfig) throws {
        self.config = config
        qemuPath = try Self.findQEMU()
        swtpmPath = config.tpm ? try Self.find("swtpm") : nil
    }

    /// Where the `akunzai/tap/glov-qemu` formula installs: keg-only, so it is never on PATH.
    public static let glovQEMUPrefixes = ["/opt/homebrew/opt/glov-qemu", "/usr/local/opt/glov-qemu"]

    /// `qemu-system-aarch64` from glov-qemu when it is installed (its virtio-balloon frees memory on macOS),
    /// else the first one `find` sees.
    public static func findQEMU(prefixes: [String] = glovQEMUPrefixes) throws -> String {
        try findQEMUTool("qemu-system-aarch64", prefixes: prefixes)
    }

    /// `qemu-img` from glov-qemu when it is installed, else the first one `find` sees.
    public static func findQEMUImg(prefixes: [String] = glovQEMUPrefixes) throws -> String {
        try findQEMUTool("qemu-img", prefixes: prefixes)
    }

    private static func findQEMUTool(_ name: String, prefixes: [String]) throws -> String {
        for prefix in prefixes {
            let candidate = prefix + "/bin/" + name
            if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
        }
        return try find(name)
    }

    /// True for a `qemu-system-aarch64 --version` of glov-qemu, which marks itself with this package version.
    public static func isBalloonBuild(version: String) -> Bool { version.contains("(glov-balloon)") }

    private static let versions = QEMUVersionCache()

    /// Whether the QEMU at `path` is glov-qemu, asked once per binary and remembered.
    public static func supportsBalloon(qemu path: String) -> Bool { isBalloonBuild(version: qemuVersion(of: path) ?? "") }

    /// The first line of `qemu-system-aarch64 --version` at `path` ("QEMU emulator version 11.1.3 (glov-balloon)"),
    /// asked once per binary and remembered; nil when it cannot be run.
    public static func qemuVersion(of path: String) -> String? {
        versions.value(for: path) { path in
            versionOutput(of: path)?.split(whereSeparator: \.isNewline).first.map { $0.trimmingCharacters(in: .whitespaces) }
        }
    }

    private static func versionOutput(of qemu: String) -> String? {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: qemu)
        p.arguments = ["--version"]
        p.standardInput = FileHandle.nullDevice
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        do { try p.run() } catch { return nil }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return String(decoding: data, as: UTF8.self)
    }

    /// The QEMU binary of the running VM: the one this actor spawned, or the attached process's.
    public func runningQEMUPath() -> String? {
        guard isRunning else { return nil }
        // The binary the process runs, not `qemuPath`: that is a Homebrew symlink an upgrade repoints under it.
        if let qemu { return Self.executablePath(of: qemu.processIdentifier) ?? qemuPath }
        return attachedPid.flatMap { Self.executablePath(of: $0) }
    }

    public static func find(_ name: String) throws -> String {
        let path = (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":").map(String.init)
        for dir in path + ["/opt/homebrew/bin", "/usr/local/bin"] {
            let candidate = dir + "/" + name
            if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
        }
        throw GlovError.executableNotFound(name)
    }

    public var isRunning: Bool {
        if let qemu { return qemu.isRunning }
        if let attachedPid { return kill(attachedPid, 0) == 0 }
        return false
    }
    public var processID: Int32? { isRunning ? (qemu?.processIdentifier ?? attachedPid) : nil }

    /// The pid in `qemu.pid` if it is alive and really is `qemu-system-aarch64` (a recycled pid is not).
    func liveQEMUPid() -> Int32? { Self.liveQEMUPid(pidFile: config.pidFilePath) }

    /// The pid in `pidFile` if it is alive and really is `qemu-system-aarch64`: a QEMU VM running with or without a daemon.
    public static func liveQEMUPid(pidFile: String) -> Int32? {
        guard let text = try? String(contentsOfFile: pidFile, encoding: .utf8),
            let pid = Int32(text.trimmingCharacters(in: .whitespacesAndNewlines)), kill(pid, 0) == 0
        else { return nil }
        return executablePath(of: pid)?.hasSuffix("/qemu-system-aarch64") == true ? pid : nil
    }

    /// The executable `pid` runs. Once that file is deleted (a Homebrew upgrade removes the old keg while the VM runs)
    /// `proc_pidpath` fails, so this falls back to the path the process was started from, as the kernel recorded it.
    static func executablePath(of pid: Int32) -> String? {
        var buf = [UInt8](repeating: 0, count: 4 * Int(MAXPATHLEN))
        let n = proc_pidpath(pid, &buf, UInt32(buf.count))
        if n > 0 { return String(decoding: buf[..<Int(n)], as: UTF8.self) }
        // KERN_PROCARGS2: argc (Int32), then the NUL-terminated exec path.
        var mib: [Int32] = [CTL_KERN, KERN_PROCARGS2, pid]
        var size = 0
        guard sysctl(&mib, 3, nil, &size, nil, 0) == 0, size > MemoryLayout<Int32>.size else { return nil }
        var args = [UInt8](repeating: 0, count: size)
        guard sysctl(&mib, 3, &args, &size, nil, 0) == 0, size > MemoryLayout<Int32>.size else { return nil }
        let path = args[MemoryLayout<Int32>.size..<size].prefix { $0 != 0 }
        return path.isEmpty ? nil : String(decoding: path, as: UTF8.self)
    }

    /// Reconnects to a qemu left running by a previous daemon. False if none is running.
    /// Throws if it runs but QMP cannot be reached (for example another client holds the socket).
    public func attach() async throws -> Bool {
        if isRunning { return qmp != nil }
        guard let pid = liveQEMUPid() else { return false }
        do { qmp = try await QMPClient.connect(socketPath: config.qmpSocketPath) } catch {
            throw GlovError.startFailed("qemu (pid \(pid)) is running but QMP is unreachable: \(error)")
        }
        attachedPid = pid
        return true
    }

    /// A console bound to the live QMP connection.
    public func console(maxWidth: Int = 1280) throws -> Console {
        guard isRunning, let qmp else { throw GlovError.notRunning }
        return Console(qmp: qmp, scratchPath: URL(fileURLWithPath: config.directory).appendingPathComponent("screen.ppm").path, maxWidth: maxWidth)
    }

    /// The QMP connection, for commands and events (for example SHUTDOWN).
    public func qmpClient() throws -> QMPClient {
        guard isRunning, let qmp else { throw GlovError.notRunning }
        return qmp
    }

    /// Unplugs `extraDrives[i]` for each index from the running guest and waits (up to `timeout`) until QEMU has dropped
    /// its block backend, which happens once the guest lets the device go. Throws if one is still there.
    public func detachDrives(_ indices: [Int], timeout: Duration = .seconds(10)) async throws {
        let qmp = try qmpClient()
        for i in indices { _ = try await qmp.execute("device_del", ["id": .string(VMConfig.driveDeviceID(i))]) }
        let ids = Set(indices.map { "extra\($0)" })
        let deadline = ContinuousClock.now + timeout
        while true {
            guard case .array(let block) = try await qmp.execute("query-block") else { return }
            let left = block.compactMap { b -> String? in if case .string(let d)? = b["device"], ids.contains(d) { d } else { nil } }
            if left.isEmpty { return }
            if ContinuousClock.now > deadline { throw GlovError.unsupported("drives \(left.sorted().joined(separator: ", ")) still attached after \(timeout)") }
            try await Task.sleep(for: .milliseconds(200))
        }
    }

    /// Starts swtpm (if configured) and qemu, and returns once QMP is connected.
    /// With `loadSnapshot`, qemu starts paused, loads that live snapshot and then resumes, so the guest continues from it.
    public func start(timeout: Duration = .seconds(30), loadSnapshot: String? = nil) async throws {
        if let pid = processID ?? liveQEMUPid() { throw GlovError.alreadyRunning(pid: pid) }
        let qemuPath = qemuPath
        let balloon = await Blocking.run { Self.supportsBalloon(qemu: qemuPath) }
        let args = try config.qemuArguments(paused: loadSnapshot != nil, balloonSupported: balloon)
        let fm = FileManager.default
        try fm.createDirectory(atPath: config.directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try? fm.removeItem(atPath: config.qmpSocketPath)
        try? fm.removeItem(atPath: config.guestSocketPath)

        if let swtpmPath {
            try fm.createDirectory(atPath: config.tpmDirectory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try? fm.removeItem(atPath: config.tpmSocketPath)
            // Supervised child, not --daemon: we hold its Process to kill it, and --terminate ends it
            // when QEMU disconnects, so it cannot outlive the VM.
            swtpm = try spawn(swtpmPath, [
                "socket", "--tpm2", "--terminate", "--tpmstate", "dir=\(config.tpmDirectory)",
                "--ctrl", "type=unixio,path=\(config.tpmSocketPath)",
                "--log", "file=\(config.tpmDirectory)/swtpm.log",
            ], log: nil)
            try await waitFor(timeout: .seconds(5), "swtpm socket") {
                fm.fileExists(atPath: self.config.tpmSocketPath) || !(self.swtpm?.isRunning ?? false)
            }
            guard swtpm?.isRunning ?? false else { throw GlovError.startFailed("swtpm exited, see \(config.tpmDirectory)/swtpm.log") }
        }

        let q = try spawn(qemuPath, args, log: config.logPath)
        qemu = q
        do {
            let deadline = ContinuousClock.now + timeout
            while true {
                if !q.isRunning { throw GlovError.startFailed("qemu exited, see \(config.logPath)") }
                if let c = try? await QMPClient.connect(socketPath: config.qmpSocketPath) {
                    qmp = c
                    if let loadSnapshot {
                        try await runSnapshotJob("snapshot-load", name: loadSnapshot)
                        _ = try await c.execute("cont")
                    }
                    return
                }
                if ContinuousClock.now > deadline { throw GlovError.startFailed("QMP socket not ready after \(timeout)") }
                try await Task.sleep(for: .milliseconds(100))
            }
        } catch {
            await stop(timeout: .zero)
            throw error
        }
    }

    /// system_powerdown, then after `timeout` quit, then SIGKILL. Returns when qemu has exited.
    public func stop(timeout: Duration = .seconds(60)) async {
        if isRunning, let pid = processID {
            if let qmp {
                _ = try? await qmp.execute("system_powerdown")
                if timeout > .zero { _ = await waitUntilExit(timeout: timeout) }
                if isRunning {
                    _ = try? await qmp.execute("quit")
                    _ = await waitUntilExit(timeout: .seconds(5))
                }
            }
            if isRunning {
                kill(pid, SIGKILL)
                _ = await waitUntilExit(timeout: .seconds(5))
            }
        }
        await qmp?.close()
        qmp = nil
        if let t = swtpm, t.isRunning {
            t.terminate()
            let deadline = ContinuousClock.now + .seconds(5)
            while t.isRunning && ContinuousClock.now < deadline { try? await Task.sleep(for: .milliseconds(50)) }
        }
        qemu = nil
        swtpm = nil
        attachedPid = nil
        // A QEMU that failed to start never wrote the file; one still named in it is another live VM process.
        if Self.liveQEMUPid(pidFile: config.pidFilePath) == nil { try? FileManager.default.removeItem(atPath: config.pidFilePath) }
    }

    /// True if qemu has exited (or was never started) within `timeout`.
    public func waitUntilExit(timeout: Duration) async -> Bool {
        let deadline = ContinuousClock.now + timeout
        while isRunning && ContinuousClock.now < deadline { try? await Task.sleep(for: .milliseconds(50)) }
        return !isRunning
    }

    private func waitFor(timeout: Duration, _ what: String, _ ready: () -> Bool) async throws {
        let deadline = ContinuousClock.now + timeout
        while !ready() {
            if ContinuousClock.now > deadline { throw GlovError.startFailed("\(what) not ready after \(timeout)") }
            try await Task.sleep(for: .milliseconds(50))
        }
    }

    private func spawn(_ exe: String, _ args: [String], log: String?) throws -> Process {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.standardInput = FileHandle.nullDevice
        if let log {
            FileManager.default.createFile(atPath: log, contents: nil, attributes: [.posixPermissions: 0o600])
            let h = try FileHandle(forWritingTo: URL(fileURLWithPath: log))
            p.standardOutput = h
            p.standardError = h
        } else {
            p.standardOutput = FileHandle.nullDevice
            p.standardError = FileHandle.nullDevice
        }
        try p.run()
        return p
    }
}

/// Remembers each QEMU binary's version line, so a VM start runs `--version` once per binary.
///
/// Keyed by `identity`, by default the real path and modification date: `findQEMU` returns a Homebrew symlink that
/// `brew upgrade` repoints, and a version kept for the old binary would be recorded with a snapshot of the new one.
final class QEMUVersionCache: @unchecked Sendable {
    private let lock = NSLock()
    private var known: [String: String?] = [:]
    private let identity: @Sendable (String) -> String

    init(identity: @escaping @Sendable (String) -> String = QEMUVersionCache.binaryIdentity) { self.identity = identity }

    static func binaryIdentity(_ path: String) -> String {
        let real = URL(fileURLWithPath: path).resolvingSymlinksInPath().path
        let modified = (try? FileManager.default.attributesOfItem(atPath: real)[.modificationDate] as? Date)?.timeIntervalSince1970
        return "\(real)@\(modified.map { "\($0)" } ?? "-")"
    }

    func value(for path: String, compute: (String) -> String?) -> String? {
        let key = identity(path)
        lock.lock()
        if let v = known[key] { lock.unlock(); return v }
        lock.unlock()
        let v = compute(path)
        lock.lock(); known[key] = v; lock.unlock()
        return v
    }
}
