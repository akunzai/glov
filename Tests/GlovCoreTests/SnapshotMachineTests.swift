import Foundation
import Testing

@testable import GlovCore

@Suite struct SnapshotMachineTests {
    static let glovQEMU = "QEMU emulator version 11.1.3 (glov-balloon)"
    static let brewQEMU = "QEMU emulator version 11.1.3"

    func config(_ dir: String, balloon: Bool = true, cpus: Int = 4, memory: String = "4G") -> VMConfig {
        VMConfig(name: "win", directory: dir, cpus: cpus, memory: memory, disk: dir + "/disk.qcow2", firmwareCode: "/fw/code.fd", balloon: balloon)
    }

    func withDir(_ body: (String) throws -> Void) throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("snapmachine-\(UUID().uuidString)").path
        defer { try? FileManager.default.removeItem(atPath: dir) }
        try body(dir)
    }

    @Test func theMachineRecordsTheDeviceOnlyOnABalloonBuild() throws {
        let c = config("/vm/win")
        #expect(SnapshotMachine(config: c, version: Self.glovQEMU) == SnapshotMachine(qemu: Self.glovQEMU, balloon: true, cpus: 4, memory: "4G"))
        #expect(!SnapshotMachine(config: c, version: Self.brewQEMU).balloon)
        let m = SnapshotMachine(config: c, version: Self.glovQEMU)
        #expect(try JSONDecoder().decode(SnapshotMachine.self, from: JSONEncoder().encode(m)) == m)
    }

    @Test func theSameMachineIsNotRefused() throws {
        try withDir { dir in
            let c = config(dir)
            try Snapshots.saveMachine(SnapshotMachine(config: c, version: Self.glovQEMU), c, name: "live1")
            #expect(FileManager.default.fileExists(atPath: dir + "/snapshots/live1/machine.json"))
            try Snapshots.requireMachine(c, name: "live1", version: Self.glovQEMU)
            // The same size written differently is the same machine.
            try Snapshots.requireMachine(config(dir, memory: "4096M"), name: "live1", version: Self.glovQEMU)
        }
    }

    @Test func aSnapshotWithoutASavedMachineOrAnUnknownQEMUIsNotChecked() throws {
        try withDir { dir in
            try Snapshots.requireMachine(config(dir), name: "old", version: Self.brewQEMU)
            let c = config(dir)
            try Snapshots.saveMachine(SnapshotMachine(config: c, version: Self.glovQEMU), c, name: "live1")
            try Snapshots.requireMachine(config(dir, balloon: false), name: "live1", version: nil)
        }
    }

    @Test func anUnreadableSavedMachineFailsInsteadOfSkippingTheCheck() throws {
        try withDir { dir in
            try FileManager.default.createDirectory(atPath: dir + "/snapshots/live1", withIntermediateDirectories: true)
            FileManager.default.createFile(atPath: dir + "/snapshots/live1/machine.json", contents: Data("{".utf8))
            #expect(throws: SnapshotError.self) { try Snapshots.requireMachine(config(dir), name: "live1", version: Self.glovQEMU) }
        }
    }

    func refusal(saved: VMConfig, savedVersion: String, now: VMConfig, version: String) throws -> SnapshotError? {
        try Snapshots.saveMachine(SnapshotMachine(config: saved, version: savedVersion), saved, name: "live1")
        do { try Snapshots.requireMachine(now, name: "live1", version: version) } catch let e as SnapshotError { return e }
        return nil
    }

    @Test func turningTheBalloonOffIsNamedWithItsFix() throws {
        try withDir { dir in
            let e = try refusal(saved: config(dir), savedVersion: Self.glovQEMU, now: config(dir, balloon: false), version: Self.glovQEMU)
            #expect(e == .machineChanged("live1", vm: "win", changes: [
                .init(field: "balloon device", old: "on", new: "off", fix: "glov vm configure win --balloon on"),
            ]))
            #expect(e?.description == #"live snapshot "live1" was taken on another machine than VM win would start now (balloon device on → off), so QEMU could not load it: put it back (glov vm configure win --balloon on), or revert a disk-only snapshot with the VM stopped, or take a new live snapshot"#)
        }
    }

    @Test func turningTheBalloonOnIsNamedWithItsFix() throws {
        try withDir { dir in
            let e = try refusal(saved: config(dir, balloon: false), savedVersion: Self.glovQEMU, now: config(dir), version: Self.glovQEMU)
            #expect(e == .machineChanged("live1", vm: "win", changes: [
                .init(field: "balloon device", old: "off", new: "on", fix: "glov vm configure win --balloon off"),
            ]))
        }
    }

    @Test func removingGlovQEMUNamesTheBuildNotTheBalloonSetting() throws {
        try withDir { dir in
            let e = try refusal(saved: config(dir), savedVersion: Self.glovQEMU, now: config(dir), version: Self.brewQEMU)
            #expect(e == .machineChanged("live1", vm: "win", changes: [
                .init(field: "QEMU", old: "'\(Self.glovQEMU)'", new: "'\(Self.brewQEMU)'", fix: "brew install akunzai/tap/glov-qemu"),
            ]))
        }
    }

    @Test func installingGlovQEMUUnderABalloonVMNamesBoth() throws {
        try withDir { dir in
            let e = try refusal(saved: config(dir), savedVersion: Self.brewQEMU, now: config(dir), version: Self.glovQEMU)
            #expect(e == .machineChanged("live1", vm: "win", changes: [
                .init(field: "QEMU", old: "'\(Self.brewQEMU)'", new: "'\(Self.glovQEMU)'", fix: "brew uninstall glov-qemu"),
                .init(field: "balloon device", old: "off", new: "on", fix: "glov vm configure win --balloon off"),
            ]))
        }
    }

    @Test func anotherQEMUVersionCPUsAndMemoryAreEachNamed() throws {
        try withDir { dir in
            let e = try refusal(
                saved: config(dir, balloon: false), savedVersion: "QEMU emulator version 11.1.2",
                now: config(dir, balloon: false, cpus: 6, memory: "8G"), version: Self.brewQEMU)
            #expect(e == .machineChanged("live1", vm: "win", changes: [
                .init(field: "QEMU", old: "'QEMU emulator version 11.1.2'", new: "'\(Self.brewQEMU)'", fix: "install the QEMU it was taken with"),
                .init(field: "cpus", old: "4", new: "6", fix: "glov vm configure win --cpus 4"),
                .init(field: "memory", old: "4G", new: "8G", fix: "glov vm configure win --memory 4G"),
            ]))
        }
    }
}
