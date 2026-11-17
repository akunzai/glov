import Foundation

/// Snapshots of a VZ guest, one directory each under `config.snapshotsDirectory`: APFS clones of the raw disk and the
/// EFI variable store, `snapshot.json` (a `SnapshotInfo`), and for a live snapshot the saved machine state
/// (`VZVirtualMachine.saveMachineStateTo`). A directory is complete once `snapshot.json` is in it; one being written
/// is named `.partial-<name>`, which no valid snapshot name can be.
public enum VZSnapshots {
    static let diskFile = "disk.img"
    static let varsFile = "efi-vars.fd"
    static let stateFile = "machine.vzvmsave"
    static let infoFile = "snapshot.json"
    /// An isolated VM's egress policy when the snapshot was taken, which reverting restores.
    public static let egressFile = "egress.json"

    public static func directory(_ config: VMConfig, _ name: String) -> String { config.snapshotsDirectory + "/" + name }

    /// The saved machine state of a live snapshot.
    public static func statePath(_ config: VMConfig, _ name: String) -> String { directory(config, name) + "/" + stateFile }

    /// The complete snapshots, oldest first.
    public static func list(_ config: VMConfig) throws -> [SnapshotInfo] {
        guard config.disk != nil else { throw SnapshotError.noDisk }
        let fm = FileManager.default
        let names = (try? fm.contentsOfDirectory(atPath: config.snapshotsDirectory)) ?? []
        return names.compactMap { name -> SnapshotInfo? in
            guard (try? Snapshots.validate(name: name)) != nil,
                let data = fm.contents(atPath: directory(config, name) + "/" + infoFile),
                var info = try? JSONDecoder().decode(SnapshotInfo.self, from: data)
            else { return nil }
            info.name = name
            return info
        }.sorted { ($0.created, $0.name) < ($1.created, $1.name) }
    }

    /// The snapshot `name`, or `SnapshotError.notFound`.
    public static func find(_ config: VMConfig, _ name: String) throws -> SnapshotInfo {
        try Snapshots.validate(name: name)
        let known = try list(config)
        guard let found = known.first(where: { $0.name == name }) else { throw SnapshotError.notFound(name, known: known.map(\.name)) }
        return found
    }

    /// A disk-only snapshot of a stopped VM.
    public static func createStopped(_ config: VMConfig, name: String) throws {
        let staging = try begin(config, name: name)
        do { try commit(config, name: name, staging: staging, stateSize: 0) } catch {
            try? FileManager.default.removeItem(atPath: staging)
            throw error
        }
    }

    /// Checks `name` is free and the VM directory can hold clones, and returns an empty staging directory for the snapshot.
    /// A live snapshot saves its machine state into `staging + "/" + stateFile` before `commit`.
    static func begin(_ config: VMConfig, name: String) throws -> String {
        try Snapshots.validate(name: name)
        if try list(config).contains(where: { $0.name == name }) { throw SnapshotError.exists(name) }
        try requireAPFS(config.directory)
        let staging = config.snapshotsDirectory + "/.partial-" + name
        let fm = FileManager.default
        try? fm.removeItem(atPath: staging)
        try fm.createDirectory(atPath: staging, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        return staging
    }

    /// Clones the disk and the variable store into `staging` and makes it the snapshot `name`. The guest must not run.
    static func commit(_ config: VMConfig, name: String, staging: String, stateSize: Int) throws {
        guard let disk = config.disk else { throw SnapshotError.noDisk }
        try clone(disk, to: staging + "/" + diskFile)
        try clone(config.firmwareVars, to: staging + "/" + varsFile)
        let info = SnapshotInfo(name: name, created: ISO8601DateFormatter().string(from: Date()), vmStateSize: stateSize, live: stateSize > 0)
        try JSONEncoder().encode(info).write(to: URL(fileURLWithPath: staging + "/" + infoFile))
        if let egress = config.egress { try JSONEncoder().encode(egress).write(to: URL(fileURLWithPath: staging + "/" + egressFile)) }
        try FileManager.default.moveItem(atPath: staging, toPath: directory(config, name))
    }

    /// Puts the disk and the variable store of a disk-only snapshot back; the VM must be stopped.
    public static func revertStopped(_ config: VMConfig, name: String) throws {
        _ = try find(config, name)
        try restoreFiles(config, name)
    }

    /// Replaces the disk and the variable store with fresh clones of the snapshot's, so the snapshot itself stays as taken.
    static func restoreFiles(_ config: VMConfig, _ name: String) throws {
        guard let disk = config.disk else { throw SnapshotError.noDisk }
        let dir = directory(config, name)
        for (source, target) in [(dir + "/" + diskFile, disk), (dir + "/" + varsFile, config.firmwareVars)] {
            let temp = target + ".revert"
            try? FileManager.default.removeItem(atPath: temp)
            try clone(source, to: temp)
            guard rename(temp, target) == 0 else {
                let message = String(cString: strerror(errno))
                try? FileManager.default.removeItem(atPath: temp)
                throw SnapshotError.failed("cannot replace \(target): \(message)")
            }
        }
    }

    /// The egress policy saved with snapshot `name`; nil for a VM that is not isolated or a snapshot older than the file.
    public static func savedEgress(_ config: VMConfig, _ name: String) -> VMConfig.Egress? {
        FileManager.default.contents(atPath: directory(config, name) + "/" + egressFile).flatMap { try? JSONDecoder().decode(VMConfig.Egress.self, from: $0) }
    }

    public static func delete(_ config: VMConfig, name: String) throws {
        _ = try find(config, name)
        try FileManager.default.removeItem(atPath: directory(config, name))
    }

    /// Fails unless `path` is on APFS: a snapshot clones multi-GiB disks, which only APFS does without copying them.
    static func requireAPFS(_ path: String) throws {
        var fs = statfs()
        guard statfs(path, &fs) == 0 else { throw SnapshotError.failed("cannot inspect \(path): \(String(cString: strerror(errno)))") }
        let type = withUnsafeBytes(of: fs.f_fstypename) { String(decoding: $0.prefix { $0 != 0 }, as: UTF8.self) }
        guard type == "apfs" else {
            throw SnapshotError.failed("VZ snapshots clone the disk, which needs APFS; \(path) is on \(type)")
        }
    }

    static func clone(_ source: String, to target: String) throws {
        guard clonefile(source, target, 0) == 0 else {
            throw SnapshotError.failed("cannot clone \(source) to \(target): \(String(cString: strerror(errno)))")
        }
    }
}
