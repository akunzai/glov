import Foundation
import Testing

@testable import GlovCore

@Suite struct HarnessSessionsTests {
    @Test func recordsLiveNextToTheHomeAndEachHarnessGetsItsOwnFolder() {
        #expect(HarnessSessions.hostPath(home: "/x/glov/", vm: "agent") == "/x/glov/.sessions/agent")
        #expect(HarnessSessions.target(harness: "codex", dir: ".codex/sessions") == "/mnt/glov-sessions/codex/sessions")
        for p in HarnessProfile.all {
            let dirs = p.sessions ?? []
            #expect(!dirs.isEmpty, "\(p.name)")
            // A record directory is never one glov writes or copies into.
            for d in dirs {
                #expect(!p.files.contains { $0.path.hasPrefix(d + "/") } && !(p.hostConfig ?? []).contains { $0.guest == d || $0.guest.hasPrefix(d + "/") }, "\(p.name) \(d)")
            }
        }
    }

    @Test func theLinkScriptMovesWhatIsThereAndRefusesAnUnmountedShare() async throws {
        let script = HarnessSessions.linkScript(harness: "claude", dir: ".claude/projects")
        #expect(script.contains("mountpoint -q /mnt/glov-sessions ||"))
        #expect(script.contains("t='/mnt/glov-sessions/claude/projects'; d=\"$HOME/.claude/projects\""))
        // Run it against a stand-in home and share: an existing folder's records move, and the folder becomes a link.
        let root = FileManager.default.temporaryDirectory.appendingPathComponent("sessions-" + UUID().uuidString).path
        defer { try? FileManager.default.removeItem(atPath: root) }
        try FileManager.default.createDirectory(atPath: root + "/home/.claude/projects/p", withIntermediateDirectories: true)
        try Data("r".utf8).write(to: URL(fileURLWithPath: root + "/home/.claude/projects/p/s.jsonl"))
        let local = script.replacingOccurrences(of: "mountpoint -q /mnt/glov-sessions", with: "true")
            .replacingOccurrences(of: "/mnt/glov-sessions", with: root + "/share")
        for _ in 0..<2 {  // and again, as a second install does
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/bin/sh")
            p.arguments = ["-c", local]
            p.environment = ["HOME": root + "/home", "PATH": "/usr/bin:/bin"]
            try await runToExit(p)
            #expect(p.terminationStatus == 0)
        }
        #expect(try FileManager.default.destinationOfSymbolicLink(atPath: root + "/home/.claude/projects") == root + "/share/claude/projects")
        #expect(try String(contentsOfFile: root + "/share/claude/projects/p/s.jsonl", encoding: .utf8) == "r")
        #expect(HarnessSessions.usage(root + "/share")?.files == 1)
        // Without the share again, the link becomes a plain folder.
        let unlink = HarnessSessions.unlinkScript(dir: ".claude/projects").replacingOccurrences(of: "/mnt/glov-sessions", with: root + "/share")
        let u = Process()
        u.executableURL = URL(fileURLWithPath: "/bin/sh")
        u.arguments = ["-c", unlink]
        u.environment = ["HOME": root + "/home", "PATH": "/usr/bin:/bin"]
        try await runToExit(u)
        var isDir = ObjCBool(false)
        #expect((try? FileManager.default.destinationOfSymbolicLink(atPath: root + "/home/.claude/projects")) == nil)
        #expect(FileManager.default.fileExists(atPath: root + "/home/.claude/projects", isDirectory: &isDir) && isDir.boolValue)
        #expect(HarnessSessions.usage(root + "/missing") == nil)
    }
}
