import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

/// `glov mcp` as a child process against an in-process daemon whose Guest Service is a `FakeGuestService`.
@Suite struct MCPGuestServiceTests {
    private func call(_ session: MCPSession, _ tool: String, _ arguments: [String: Any]) async throws -> (text: String, isError: Bool) {
        let result = try await session.request("tools/call", ["name": tool, "arguments": arguments])
        let text = (result["content"] as? [[String: Any]])?.compactMap { $0["text"] as? String }.joined(separator: "\n") ?? ""
        return (text, result["isError"] as? Bool == true)
    }

    @Test(.timeLimit(.minutes(1)))
    func vmStatusIncludesGuestServiceUpdateDetails() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        _ = await a.checkGuestService()
        let session = try MCPSession(home: f.home)
        defer { session.terminate() }
        _ = try await session.initialize()
        let status = try await call(session, "vm_status", ["vm": "win11"])
        #expect(!status.isError)
        let raw = try #require(JSONSerialization.jsonObject(with: Data(status.text.utf8)) as? [String: Any])
        #expect(raw["agent"] == nil)
        let guest = try #require(raw["guestService"] as? [String: Any])
        #expect(guest["version"] as? String == "0.0-fake")
        let info = try JSONDecoder().decode(Results.VMInfo.self, from: Data(status.text.utf8))
        #expect(info.agent?.update?.state == "unknown")
        #expect(info.agent?.update?.problem?.contains("tools/build.sh") == true)
    }

    @Test(.timeLimit(.minutes(1)))
    func execFileAndCopyToolsGiveLeanReplies() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let session = try MCPSession(home: f.home)
        defer { session.terminate() }
        _ = try await session.initialize()

        f.fake.script = { _ in (1...120).map { .out("stdout", "line \($0)\n") } + [.exit(2)] }
        let run = try await call(session, "exec", ["vm": "win11", "argv": ["powershell.exe", "-NoProfile", "-Command", "x"]])
        #expect(!run.isError)
        #expect(run.text.hasPrefix("job j1: exit code 2\n120 lines of output, last 50 below; full log: "))
        #expect(run.text.contains("line 120") && run.text.contains("line 71") && !run.text.contains("line 70\n"))
        #expect(run.text.contains(f.config.directory + "/jobs/j1.log"))

        f.fake.script = { _ in [.out("stdout", "building\n"), .hold] }
        let started = try await call(session, "exec_start", ["vm": "win11", "argv": ["msbuild"]])
        #expect(started.text.contains("job j2 started"))
        let read = try await call(session, "exec_read", ["vm": "win11", "jobId": "j2", "cursor": 0])
        #expect(read.text.hasPrefix("running; cursor 9;") && read.text.hasSuffix("---\nbuilding\n"))
        let killed = try await call(session, "exec_kill", ["vm": "win11", "jobId": "j2"])
        #expect(!killed.isError)

        let wrote = try await call(session, "file_write", ["vm": "win11", "path": "C:\\n.txt", "text": "hi"])
        #expect(!wrote.isError)
        let read2 = try await call(session, "file_read", ["vm": "win11", "path": "C:\\n.txt"])
        #expect(read2.text == "hi")

        let denied = try await call(session, "copy_to_vm", ["vm": "win11", "hostPath": "/etc/hosts", "guestPath": "C:\\h"])
        #expect(denied.isError && denied.text.contains("allowlist"))
    }

    @Test(.timeLimit(.minutes(1)))
    func shareSyncSaysWhatMoved() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let host = f.home.path + "/proj"
        try FileManager.default.createDirectory(atPath: host, withIntermediateDirectories: true)
        try Data("a".utf8).write(to: URL(fileURLWithPath: host + "/a.txt"))
        try f.home.update("win11") { config in config = try config.adding(.init(name: "proj", hostPath: host, guestPath: "C:\\w\\proj")) }
        f.fake.files["C:\\w\\proj\\old.txt"] = Data("o".utf8)
        let session = try MCPSession(home: f.home)
        defer { session.terminate() }
        _ = try await session.initialize()
        let r = try await call(session, "share_sync", ["vm": "win11", "share": "proj"])
        #expect(!r.isError)
        #expect(r.text.hasPrefix("proj -> C:\\w\\proj: 1 written, 1 deleted, 0 unchanged, 1 bytes in "))
        #expect(r.text.hasSuffix("\n+ a.txt\n- old.txt"))
        let missing = try await call(session, "share_sync", ["vm": "win11", "share": "other"])
        #expect(missing.isError && missing.text.contains("no share named"))
    }

    @Test(.enabled(if: (try? VirtualMachine.find("qemu-img")) != nil, "qemu-img is required"), .timeLimit(.minutes(1)))
    func snapshotToolsRoundTripOnAStoppedVM() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let disk = home.path + "/d.qcow2"
        _ = try await Snapshots.run(try Snapshots.qemuImgPath(), ["create", "-f", "qcow2", disk, "1M"])
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        try home.create(VMConfig(name: "win11", directory: home.directory(of: "win11"), disk: disk, firmwareCode: home.path + "/code.fd", tpm: false))
        let server = try DaemonServer(home: home, dispatcher: Dispatcher(home: home))
        server.start()
        defer { server.stop() }
        let session = try MCPSession(home: home)
        defer { session.terminate() }
        _ = try await session.initialize()

        #expect(try await call(session, "snapshot_list", ["vm": "win11"]).text == "no snapshots")
        let created = try await call(session, "snapshot_create", ["vm": "win11", "name": "base"])
        #expect(created.text == "snapshot base created" && !created.isError)
        let listed = try await call(session, "snapshot_list", ["vm": "win11"])
        #expect(listed.text.hasPrefix("base  ") && listed.text.hasSuffix("disk only"))
        let reverted = try await call(session, "snapshot_revert", ["vm": "win11", "name": "base"])
        #expect(reverted.text == "reverted; VM win11 is stopped")
        let bad = try await call(session, "snapshot_create", ["vm": "win11", "name": "a b"])
        #expect(bad.isError && bad.text.contains("invalid snapshot name"))
        #expect(try await call(session, "snapshot_delete", ["vm": "win11", "name": "base"]).text == "snapshot base deleted")
        #expect(try await call(session, "snapshot_list", ["vm": "win11"]).text == "no snapshots")
    }
}
