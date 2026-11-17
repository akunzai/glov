import Foundation
import Testing

@testable import GlovCore

/// A stopped VZ VM with a small raw disk and EFI variable store holding known bytes.
private struct VZDiskFixture {
    let config: VMConfig

    init() throws {
        let dir = try shortTempDir()
        config = VMConfig(
            name: "ubuntu", directory: dir, engine: .vz, disk: dir + "/disk.img", firmwareCode: "", firmwareVars: dir + "/efi-vars.fd", tpm: false)
        try write(disk: "disk-1", vars: "vars-1")
    }

    func write(disk: String, vars: String) throws {
        try Data(disk.utf8).write(to: URL(fileURLWithPath: config.disk!))
        try Data(vars.utf8).write(to: URL(fileURLWithPath: config.firmwareVars))
    }

    var contents: (disk: String, vars: String) {
        (String(decoding: FileManager.default.contents(atPath: config.disk!) ?? Data(), as: UTF8.self),
            String(decoding: FileManager.default.contents(atPath: config.firmwareVars) ?? Data(), as: UTF8.self))
    }

    func cleanup() { try? FileManager.default.removeItem(atPath: config.directory) }
}

@Suite struct VZSnapshotTests {
    @Test func aStoppedSnapshotClonesTheDiskAndVarsAndRevertingBringsThemBack() throws {
        let f = try VZDiskFixture()
        defer { f.cleanup() }
        #expect(try VZSnapshots.list(f.config).isEmpty)

        try VZSnapshots.createStopped(f.config, name: "clean")
        let list = try VZSnapshots.list(f.config)
        #expect(list.map(\.name) == ["clean"])
        #expect(list.first?.live == false)
        #expect(list.first?.vmStateSize == 0)

        try f.write(disk: "disk-2", vars: "vars-2")
        try VZSnapshots.revertStopped(f.config, name: "clean")
        #expect(f.contents == ("disk-1", "vars-1"))

        // Reverting again starts from the snapshot, not from the first revert's result.
        try f.write(disk: "disk-3", vars: "vars-3")
        try VZSnapshots.revertStopped(f.config, name: "clean")
        #expect(f.contents == ("disk-1", "vars-1"))

        try VZSnapshots.delete(f.config, name: "clean")
        #expect(try VZSnapshots.list(f.config).isEmpty)
        #expect(!FileManager.default.fileExists(atPath: f.config.snapshotsDirectory + "/clean"))
    }

    @Test func namesDuplicatesAndUnknownSnapshotsAreRefusedLikeQEMU() throws {
        let f = try VZDiskFixture()
        defer { f.cleanup() }
        #expect(throws: SnapshotError.invalidName("../x")) { try VZSnapshots.createStopped(f.config, name: "../x") }
        #expect(throws: SnapshotError.invalidName(".partial-a")) { try VZSnapshots.revertStopped(f.config, name: ".partial-a") }
        try VZSnapshots.createStopped(f.config, name: "a")
        #expect(throws: SnapshotError.exists("a")) { try VZSnapshots.createStopped(f.config, name: "a") }
        #expect(throws: SnapshotError.notFound("b", known: ["a"])) { try VZSnapshots.revertStopped(f.config, name: "b") }
        #expect(throws: SnapshotError.notFound("b", known: ["a"])) { try VZSnapshots.delete(f.config, name: "b") }
    }

    @Test func anUnfinishedSnapshotIsNotListed() throws {
        let f = try VZDiskFixture()
        defer { f.cleanup() }
        let staging = try VZSnapshots.begin(f.config, name: "half")
        try FileManager.default.createDirectory(atPath: VZSnapshots.directory(f.config, "nojson"), withIntermediateDirectories: true)
        #expect(try VZSnapshots.list(f.config).isEmpty)
        try VZSnapshots.commit(f.config, name: "half", staging: staging, stateSize: 42)
        #expect(try VZSnapshots.list(f.config).map(\.live) == [true])
        #expect(!FileManager.default.fileExists(atPath: staging))
    }

    @Test func aVMWithoutADiskHasNothingToSnapshot() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let config = VMConfig(name: "n", directory: dir, engine: .vz, firmwareCode: "", tpm: false)
        #expect(throws: SnapshotError.noDisk) { try VZSnapshots.createStopped(config, name: "a") }
        #expect(throws: SnapshotError.noDisk) { try VZSnapshots.list(config) }
    }

    @Test func aDirectoryOffAPFSIsRefusedBeforeAnythingIsCopied() throws {
        try VZSnapshots.requireAPFS(NSTemporaryDirectory())
        #expect {
            try VZSnapshots.requireAPFS("/dev")
        } throws: { error in
            "\(error)".contains("needs APFS") && "\(error)".contains("devfs")
        }
    }
}
