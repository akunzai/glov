import Foundation

/// One snapshot of the system disk. On QEMU UEFI variables are not part of it and are never reverted; VZ snapshots include them.
public struct SnapshotInfo: Codable, Sendable, Equatable {
    public var name: String
    /// ISO-8601, UTC.
    public var created: String
    /// Bytes of saved machine state; 0 for a disk-only snapshot taken while the VM was stopped.
    public var vmStateSize: Int
    /// True when it holds the running machine (memory, devices, TPM), so reverting resumes the guest where it was.
    public var live: Bool

    public init(name: String, created: String, vmStateSize: Int, live: Bool) {
        self.name = name; self.created = created; self.vmStateSize = vmStateSize; self.live = live
    }
}

public enum SnapshotError: Error, Equatable, CustomStringConvertible {
    case invalidName(String)
    case exists(String)
    case notFound(String, known: [String])
    case noDisk
    case needsStoppedVM(String)
    /// A live snapshot whose machine differs from the one the VM would start now: QEMU could not load it.
    case machineChanged(String, vm: String, changes: [SnapshotMachine.Change])
    case failed(String)

    public var description: String {
        switch self {
        case .invalidName(let n): "invalid snapshot name \(String(reflecting: n)): use 1 to 64 characters from A-Z a-z 0-9 . _ -, not starting with ."
        case .exists(let n): "snapshot \(String(reflecting: n)) already exists; delete it first or pick another name"
        case .notFound(let n, let known):
            "no snapshot \(String(reflecting: n))" + (known.isEmpty ? " (this VM has none)" : " (known: \(known.joined(separator: ", ")))")
        case .noDisk: "this VM has no system disk, so there is nothing to snapshot"
        case .needsStoppedVM(let n): "snapshot \(String(reflecting: n)) holds only the disk, not the running machine: stop the VM first (vm_stop), then revert"
        case .machineChanged(let n, let vm, let changes):
            "live snapshot \(String(reflecting: n)) was taken on another machine than VM \(vm) would start now ("
                + changes.map { "\($0.field) \($0.old) → \($0.new)" }.joined(separator: "; ")
                + "), so QEMU could not load it: put it back (" + changes.map(\.fix).joined(separator: "; ")
                + "), or revert a disk-only snapshot with the VM stopped, or take a new live snapshot"
        case .failed(let m): "snapshot failed: \(m)"
        }
    }
}

/// What decides whether QEMU can load a live snapshot into the machine a VM starts: written as `machine.json` in the
/// snapshot's directory when a live snapshot is taken, and compared before a live revert. glov starts the unversioned
/// `-machine virt`, so the QEMU version matters as well as the build (glov-qemu or not).
public struct SnapshotMachine: Codable, Sendable, Equatable {
    /// First line of `qemu-system-aarch64 --version`.
    public var qemu: String
    /// Whether the virtio-balloon device was added (`VMConfig.balloon` on a QEMU that supports it).
    public var balloon: Bool
    public var cpus: Int
    public var memory: String

    public init(qemu: String, balloon: Bool, cpus: Int, memory: String) {
        self.qemu = qemu; self.balloon = balloon; self.cpus = cpus; self.memory = memory
    }

    /// The machine `config` gets from the QEMU at `qemu` whose version line is `version`.
    public init(config: VMConfig, version: String) {
        self.init(qemu: version, balloon: config.balloon && VirtualMachine.isBalloonBuild(version: version), cpus: config.cpus, memory: config.memory)
    }

    public struct Change: Sendable, Equatable {
        public var field: String, old: String, new: String, fix: String
    }

    /// How `now` differs from this snapshot's machine, each with how to put it back for VM `vm`, whose balloon setting is
    /// `balloonSetting` now: with it on, a device that is gone went with glov-qemu, which the QEMU change already names.
    public func changes(to now: SnapshotMachine, vm: String, balloonSetting: Bool) -> [Change] {
        var changes: [Change] = []
        if qemu != now.qemu {
            let fix = VirtualMachine.isBalloonBuild(version: qemu) && !VirtualMachine.isBalloonBuild(version: now.qemu)
                ? "brew install akunzai/tap/glov-qemu"
                : !VirtualMachine.isBalloonBuild(version: qemu) && VirtualMachine.isBalloonBuild(version: now.qemu)
                    ? "brew uninstall glov-qemu" : "install the QEMU it was taken with"
            changes.append(Change(field: "QEMU", old: "'\(qemu)'", new: "'\(now.qemu)'", fix: fix))
        }
        if balloon != now.balloon, !(balloon && balloonSetting) {
            changes.append(Change(
                field: "balloon device", old: balloon ? "on" : "off", new: now.balloon ? "on" : "off",
                fix: "glov vm configure \(vm) --balloon \(balloon ? "on" : "off")"))
        }
        if cpus != now.cpus {
            changes.append(Change(field: "cpus", old: "\(cpus)", new: "\(now.cpus)", fix: "glov vm configure \(vm) --cpus \(cpus)"))
        }
        if VMConfig.bytes(memory) != VMConfig.bytes(now.memory) {
            changes.append(Change(field: "memory", old: memory, new: now.memory, fix: "glov vm configure \(vm) --memory \(memory)"))
        }
        return changes
    }
}

public enum Snapshots {
    public static func validate(name: String) throws {
        let ok = (1...64).contains(name.utf8.count) && name.utf8.allSatisfy {
            ($0 >= 0x30 && $0 <= 0x39) || ($0 >= 0x41 && $0 <= 0x5A) || ($0 >= 0x61 && $0 <= 0x7A) || $0 == 0x2E || $0 == 0x5F || $0 == 0x2D
        } && name.first != "."
        guard ok else { throw SnapshotError.invalidName(name) }
    }

    /// Reads the `snapshots` array of `qemu-img info --output=json` or of a QMP block node's `image` (same keys in both).
    public static func parse(_ snapshots: JSONValue?) -> [SnapshotInfo] {
        guard case .array(let items)? = snapshots else { return [] }
        let formatter = ISO8601DateFormatter()
        return items.compactMap { item in
            guard case .string(let name)? = item["name"] else { return nil }
            func int(_ key: String) -> Int {
                if case .int(let v)? = item[key] { return v }
                if case .double(let v)? = item[key] { return Int(v) }
                return 0
            }
            let size = int("vm-state-size")
            return SnapshotInfo(
                name: name, created: formatter.string(from: Date(timeIntervalSince1970: TimeInterval(int("date-sec")))),
                vmStateSize: size, live: size > 0)
        }
    }

    /// Snapshots in the output of `qemu-img info --output=json`.
    public static func parseImageInfo(_ json: Data) throws -> [SnapshotInfo] {
        parse(try JSONDecoder().decode(JSONValue.self, from: json)["snapshots"])
    }

    /// Snapshots of `node` in the reply to QMP `query-named-block-nodes`.
    public static func parseNamedBlockNodes(_ reply: JSONValue, node: String) -> [SnapshotInfo] {
        guard case .array(let nodes) = reply else { return [] }
        guard let n = nodes.first(where: { $0["node-name"] == .string(node) }) else { return [] }
        return parse(n["image"]?["snapshots"])
    }

    // MARK: stopped VM: qemu-img

    static func qemuImgPath() throws -> String { try VirtualMachine.find("qemu-img") }

    static func qemuImg(_ args: [String]) async throws -> Data { try await run(try qemuImgPath(), args) }

    static func run(_ exe: String, _ args: [String]) async throws -> Data {
        // Not Task.detached: that still runs on the cooperative pool, and this blocks until qemu-img exits.
        try await Blocking.run {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: exe)
            p.arguments = args
            let out = Pipe(), err = Pipe()
            p.standardOutput = out
            p.standardError = err
            p.standardInput = FileHandle.nullDevice
            try p.run()
            // qemu-img prints little; reading stderr after stdout cannot fill a pipe here.
            let data = out.fileHandleForReading.readDataToEndOfFile()
            let errData = err.fileHandleForReading.readDataToEndOfFile()
            p.waitUntilExit()
            guard p.terminationStatus == 0 else {
                let text = String(decoding: errData, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                throw SnapshotError.failed(text.isEmpty ? "\((exe as NSString).lastPathComponent) exited with status \(p.terminationStatus)" : text)
            }
            return data
        }
    }

    private static func disk(_ config: VMConfig) throws -> String {
        guard let disk = config.disk else { throw SnapshotError.noDisk }
        return disk
    }

    public static func listStopped(_ config: VMConfig) async throws -> [SnapshotInfo] {
        try await parseImageInfo(qemuImg(["info", "--output=json", try disk(config)]))
    }

    private static func tpmCopy(_ config: VMConfig, _ name: String) -> String { config.snapshotsDirectory + "/" + name + "/tpm" }

    /// The machine a live snapshot was taken on (`SnapshotMachine`), next to the TPM copy.
    public static func machinePath(_ config: VMConfig, _ name: String) -> String {
        config.snapshotsDirectory + "/" + name + "/machine.json"
    }

    static func saveMachine(_ machine: SnapshotMachine, _ config: VMConfig, name: String) throws {
        let fm = FileManager.default
        let dir = config.snapshotsDirectory + "/" + name
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try encoder.encode(machine).write(to: URL(fileURLWithPath: machinePath(config, name)), options: .atomic)
    }

    /// The machine saved with live snapshot `name`, or nil for one taken before glov recorded it.
    public static func savedMachine(_ config: VMConfig, name: String) throws -> SnapshotMachine? {
        let path = machinePath(config, name)
        guard let data = FileManager.default.contents(atPath: path) else { return nil }
        do { return try JSONDecoder().decode(SnapshotMachine.self, from: data) } catch {
            throw SnapshotError.failed("cannot read \(path): \(error)")
        }
    }

    /// Refuses a revert to live snapshot `name` when the machine it was taken on differs from the one `config` would
    /// start now on a QEMU whose version line is `version`. A snapshot without a saved machine, or an unknown version,
    /// is not checked.
    public static func requireMachine(_ config: VMConfig, name: String, version: String?) throws {
        guard let saved = try savedMachine(config, name: name), let version else { return }
        let changes = saved.changes(to: SnapshotMachine(config: config, version: version), vm: config.name, balloonSetting: config.balloon)
        if !changes.isEmpty { throw SnapshotError.machineChanged(name, vm: config.name, changes: changes) }
    }

    /// `requireMachine` for the QEMU `VirtualMachine` would start now. Asking QEMU its version runs it, so off the pool.
    public static func requireMachine(_ config: VMConfig, name: String) async throws {
        let version = await Blocking.run { (try? VirtualMachine.findQEMU()).flatMap(VirtualMachine.qemuVersion(of:)) }
        try requireMachine(config, name: name, version: version)
    }

    /// The TPM state files; the socket and log are not state.
    private static func tpmFiles(_ dir: String) -> [String] {
        let fm = FileManager.default
        return ((try? fm.contentsOfDirectory(atPath: dir)) ?? []).filter { name in
            var isDir: ObjCBool = false
            return name != "swtpm.sock" && name != "swtpm.log" && fm.fileExists(atPath: dir + "/" + name, isDirectory: &isDir) && !isDir.boolValue
        }
    }

    public static func createStopped(_ config: VMConfig, name: String) async throws {
        try validate(name: name)
        let disk = try disk(config)
        let existing = try await listStopped(config)
        if existing.contains(where: { $0.name == name }) { throw SnapshotError.exists(name) }
        // Left behind by a snapshot of this name deleted outside glov; a disk-only snapshot has no machine to check.
        try? FileManager.default.removeItem(atPath: machinePath(config, name))
        _ = try await qemuImg(["snapshot", "-c", name, disk])
        guard config.tpm else { return }
        let fm = FileManager.default
        let target = tpmCopy(config, name)
        do {
            try? fm.removeItem(atPath: target)
            try fm.createDirectory(atPath: target, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            for file in tpmFiles(config.tpmDirectory) { try fm.copyItem(atPath: config.tpmDirectory + "/" + file, toPath: target + "/" + file) }
        } catch {
            _ = try? await qemuImg(["snapshot", "-d", name, disk])
            try? fm.removeItem(atPath: config.snapshotsDirectory + "/" + name)
            throw error
        }
    }

    /// Applies a disk-only snapshot and restores the TPM state saved with it.
    public static func revertStopped(_ config: VMConfig, name: String) async throws {
        try validate(name: name)
        let disk = try disk(config)
        let known = try await listStopped(config)
        guard let snapshot = known.first(where: { $0.name == name }) else { throw SnapshotError.notFound(name, known: known.map(\.name)) }
        _ = try await qemuImg(["snapshot", "-a", name, disk])
        guard config.tpm, !snapshot.live else { return }
        let fm = FileManager.default
        let source = tpmCopy(config, name)
        guard fm.fileExists(atPath: source) else { return }
        try fm.createDirectory(atPath: config.tpmDirectory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        for file in tpmFiles(config.tpmDirectory) { try fm.removeItem(atPath: config.tpmDirectory + "/" + file) }
        for file in tpmFiles(source) { try fm.copyItem(atPath: source + "/" + file, toPath: config.tpmDirectory + "/" + file) }
    }

    public static func deleteStopped(_ config: VMConfig, name: String) async throws {
        try validate(name: name)
        let disk = try disk(config)
        let known = try await listStopped(config)
        guard known.contains(where: { $0.name == name }) else { throw SnapshotError.notFound(name, known: known.map(\.name)) }
        _ = try await qemuImg(["snapshot", "-d", name, disk])
        try? FileManager.default.removeItem(atPath: config.snapshotsDirectory + "/" + name)
    }
}

// MARK: running VM: QMP jobs

extension Snapshots {
    /// Starts a snapshot job on `qmp` and polls it until it concludes. If QEMU goes away meanwhile, fails at once.
    static func runJob(on qmp: QMPClient, _ command: String, name: String, timeout: Duration) async throws {
        try validate(name: name)
        let node = VMConfig.systemNode
        let jobID = "gp-\(command)-\(UUID().uuidString.prefix(8))"
        var args: [String: JSONValue] = ["job-id": .string(jobID), "tag": .string(name), "devices": .array([.string(node)])]
        if command != "snapshot-delete" { args["vmstate"] = .string(node) }
        do {
            _ = try await qmp.execute(command, .object(args))
            let deadline = ContinuousClock.now + timeout
            while true {
                if case .array(let jobs) = try await qmp.execute("query-jobs"),
                    let job = jobs.first(where: { $0["id"] == .string(jobID) }), job["status"] == "concluded"
                {
                    _ = try? await qmp.execute("job-dismiss", ["id": .string(jobID)])
                    if case .string(let message)? = job["error"] { throw SnapshotError.failed(message) }
                    return
                }
                guard ContinuousClock.now < deadline else { throw SnapshotError.failed("\(command) did not finish within \(timeout)") }
                try await Task.sleep(for: .milliseconds(100))
            }
        } catch QMPError.closed {
            throw SnapshotError.failed("qemu exited while \(command) was running; see the VM's qemu.log")
        }
    }
}

extension VirtualMachine {
    public func listSnapshots() async throws -> [SnapshotInfo] {
        guard config.disk != nil else { throw SnapshotError.noDisk }
        let reply = try await qmpClient().execute("query-named-block-nodes")
        return Snapshots.parseNamedBlockNodes(reply, node: VMConfig.systemNode)
    }

    /// Runs `snapshot-save`, `snapshot-load` or `snapshot-delete` for `name` and waits for the job to conclude.
    /// `snapshot-load` is only for a QEMU that was just started paused: loading over a running guest aborts QEMU.
    public func runSnapshotJob(_ command: String, name: String, timeout: Duration = .seconds(600)) async throws {
        guard config.disk != nil else { throw SnapshotError.noDisk }
        try await Snapshots.runJob(on: try qmpClient(), command, name: name, timeout: timeout)
    }

    /// Takes live snapshot `name` and records the machine it was taken on (`Snapshots.machinePath`), so a revert after
    /// the VM's machine changed is refused rather than failing in QEMU. `version` reads a QEMU's version line.
    public func createSnapshot(_ name: String, version: @escaping @Sendable (String) -> String? = VirtualMachine.qemuVersion(of:)) async throws {
        try Snapshots.validate(name: name)
        if try await listSnapshots().contains(where: { $0.name == name }) { throw SnapshotError.exists(name) }
        let dir = config.snapshotsDirectory + "/" + name
        try? FileManager.default.removeItem(atPath: dir)
        try await runSnapshotJob("snapshot-save", name: name)
        guard let qemu = runningQEMUPath(), let line = await Blocking.run({ version(qemu) }) else { return }
        do { try Snapshots.saveMachine(SnapshotMachine(config: config, version: line), config, name: name) } catch {
            try? await runSnapshotJob("snapshot-delete", name: name)
            try? FileManager.default.removeItem(atPath: dir)
            throw error
        }
    }

    /// Checks that `name` is a live snapshot that can be reverted to. The revert itself restarts QEMU
    /// (`start(loadSnapshot:)`), because `snapshot-load` into a running guest aborts QEMU.
    public func requireLiveSnapshot(_ name: String) async throws {
        try Snapshots.validate(name: name)
        let known = try await listSnapshots()
        guard let snapshot = known.first(where: { $0.name == name }) else { throw SnapshotError.notFound(name, known: known.map(\.name)) }
        guard snapshot.live else { throw SnapshotError.needsStoppedVM(name) }
    }

    public func deleteSnapshot(_ name: String) async throws {
        try Snapshots.validate(name: name)
        let known = try await listSnapshots()
        guard known.contains(where: { $0.name == name }) else { throw SnapshotError.notFound(name, known: known.map(\.name)) }
        try await runSnapshotJob("snapshot-delete", name: name)
        try? FileManager.default.removeItem(atPath: config.snapshotsDirectory + "/" + name)
    }
}
