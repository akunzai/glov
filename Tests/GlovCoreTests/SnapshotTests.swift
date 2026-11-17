import Foundation
import Testing

@testable import GlovCore

let qemuImgAvailable = (try? VirtualMachine.find("qemu-img")) != nil

@Suite struct SnapshotTests {
    @Test func namesAreLimitedToSafeCharacters() throws {
        for ok in ["a", "before-build", "v1.2_x", String(repeating: "a", count: 64)] { try Snapshots.validate(name: ok) }
        for bad in ["", "a b", "a/b", "../x", "..", ".", ".hidden", "a,b", "né", String(repeating: "a", count: 65), "x\n"] {
            #expect(throws: SnapshotError.invalidName(bad)) { try Snapshots.validate(name: bad) }
        }
    }

    @Test func systemDiskHasAnExplicitNodeNameDistinctFromTheDriveID() throws {
        let args = try VMConfig(name: "n", directory: "/vm/n", disk: "/vm/n/d.qcow2", firmwareCode: "/fw").qemuArguments()
        let drive = try #require(args.first { $0.hasPrefix("if=none,id=disk0") })
        #expect(drive.contains(",node-name=sys0,"))
        #expect(!args.contains("-S"))
        #expect(try VMConfig(name: "n", directory: "/vm/n", firmwareCode: "/fw").qemuArguments(paused: true).contains("-S"))
    }

    @Test func parsesQemuImgInfo() throws {
        let json = """
            {"virtual-size": 1048576, "filename": "d.qcow2", "format": "qcow2", "snapshots": [
              {"id": "1", "name": "disk-only", "vm-state-size": 0, "date-sec": 1700000000, "date-nsec": 5, "vm-clock-sec": 0, "vm-clock-nsec": 0},
              {"id": "2", "name": "live", "vm-state-size": 123456, "date-sec": 1700000100, "date-nsec": 0, "vm-clock-sec": 3, "vm-clock-nsec": 4}]}
            """
        let list = try Snapshots.parseImageInfo(Data(json.utf8))
        #expect(list == [
            SnapshotInfo(name: "disk-only", created: "2023-11-14T22:13:20Z", vmStateSize: 0, live: false),
            SnapshotInfo(name: "live", created: "2023-11-14T22:15:00Z", vmStateSize: 123456, live: true),
        ])
        #expect(try Snapshots.parseImageInfo(Data(#"{"format":"qcow2"}"#.utf8)).isEmpty)
    }

    @Test func parsesTheSystemNodeOfQueryNamedBlockNodes() throws {
        let reply = try JSONDecoder().decode(JSONValue.self, from: Data("""
            [{"node-name": "sys0", "image": {"format": "qcow2", "snapshots": [
               {"id": "1", "name": "s1", "vm-state-size": 99, "date-sec": 1700000000, "date-nsec": 0, "vm-clock-sec": 1, "vm-clock-nsec": 2}]}},
             {"node-name": "other", "image": {"snapshots": [{"name": "nope", "vm-state-size": 1, "date-sec": 0}]}},
             {"node-name": "#block1", "image": {"format": "file"}}]
            """.utf8))
        #expect(Snapshots.parseNamedBlockNodes(reply, node: "sys0").map(\.name) == ["s1"])
        #expect(Snapshots.parseNamedBlockNodes(reply, node: "#block1").isEmpty)
        #expect(Snapshots.parseNamedBlockNodes(reply, node: "missing").isEmpty)
    }

    @Test(.enabled(if: qemuImgAvailable, "qemu-img is required"))
    func stoppedSnapshotsRestoreTheDiskAndTheTPMState() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let disk = dir + "/d.qcow2"
        let img = try Snapshots.qemuImgPath()
        _ = try await Snapshots.run(img, ["create", "-f", "qcow2", disk, "1M"])
        let config = VMConfig(name: "t", directory: dir, disk: disk, firmwareCode: "/fw", tpm: true)
        let fm = FileManager.default
        try fm.createDirectory(atPath: config.tpmDirectory, withIntermediateDirectories: true)
        func tpm(_ text: String) throws { try Data(text.utf8).write(to: URL(fileURLWithPath: config.tpmDirectory + "/tpm2-00.permall")) }
        func tpmText() throws -> String { try String(contentsOfFile: config.tpmDirectory + "/tpm2-00.permall", encoding: .utf8) }
        try tpm("one")
        fm.createFile(atPath: config.tpmDirectory + "/swtpm.sock", contents: nil)

        try await Snapshots.createStopped(config, name: "first")
        let listed = try await Snapshots.listStopped(config)
        #expect(listed.map(\.name) == ["first"] && listed[0].live == false && listed[0].vmStateSize == 0)
        #expect(fm.fileExists(atPath: config.snapshotsDirectory + "/first/tpm/tpm2-00.permall"))
        #expect(!fm.fileExists(atPath: config.snapshotsDirectory + "/first/tpm/swtpm.sock"))

        await #expect(throws: SnapshotError.exists("first")) { try await Snapshots.createStopped(config, name: "first") }
        await #expect(throws: SnapshotError.invalidName("a b")) { try await Snapshots.createStopped(config, name: "a b") }

        try tpm("two")
        try await Snapshots.revertStopped(config, name: "first")
        #expect(try tpmText() == "one")
        #expect(fm.fileExists(atPath: config.tpmDirectory + "/swtpm.sock"), "the socket is not state and stays")

        await #expect(throws: SnapshotError.notFound("nope", known: ["first"])) { try await Snapshots.revertStopped(config, name: "nope") }
        try await Snapshots.deleteStopped(config, name: "first")
        #expect(try await Snapshots.listStopped(config).isEmpty)
        #expect(!fm.fileExists(atPath: config.snapshotsDirectory + "/first"))
    }

    @Test func aSnapshotJobFailsAtOnceWhenQEMUGoesAwayMidJob() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let client = try await QMPClient.connect(socketPath: try FakeQMPServer(dir: dir).path)
        let started = ContinuousClock.now
        do {
            try await Snapshots.runJob(on: client, "snapshot-save", name: "s", timeout: .seconds(600))
            Issue.record("the job cannot conclude")
        } catch let e as SnapshotError {
            #expect(e.description.contains("qemu exited while snapshot-save was running"), "\(e)")
        }
        #expect(ContinuousClock.now - started < .seconds(5))
    }
}
