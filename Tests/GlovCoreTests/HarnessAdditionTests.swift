import Foundation
import Testing

@testable import GlovCore

@Suite struct HarnessAdditionTests {
    private func sh(_ script: String, home: String, _ args: [String], stdin: String = "") async throws -> Int32 {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-c", script, "sh"] + args
        p.environment = ["HOME": home, "PATH": "/usr/bin:/bin"]
        let input = Pipe()
        p.standardInput = input
        let (exited, exit) = AsyncStream<Void>.makeStream()
        p.terminationHandler = { _ in exit.yield(); exit.finish() }
        try p.run()
        // A script that refuses exits before reading its input (EPIPE); the deprecated `write(_:)` raises on that and
        // kills the test process. Off the cooperative pool, since a pipe write blocks.
        let toChild = input.fileHandleForWriting
        await Blocking.run {
            try? toChild.write(contentsOf: Data(stdin.utf8))
            try? toChild.close()
        }
        for await _ in exited {}
        return p.terminationStatus
    }

    @Test func aProfileFileIsWrittenOnlyWhereNothingIs() async throws {
        let home = NSTemporaryDirectory() + "glov-add-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: home) }
        #expect(try await sh(HarnessAddition.writeFileScript, home: home, [".codex/config.toml"], stdin: "a = 1\n") == 0)
        #expect(try String(contentsOfFile: home + "/.codex/config.toml", encoding: .utf8) == "a = 1\n")
        let mode = try FileManager.default.attributesOfItem(atPath: home + "/.codex/config.toml")[.posixPermissions] as? Int
        #expect(mode == 0o600)
        // The owner's own file stays.
        #expect(try await sh(HarnessAddition.writeFileScript, home: home, [".codex/config.toml"], stdin: "b = 2\n") == 10)
        #expect(try String(contentsOfFile: home + "/.codex/config.toml", encoding: .utf8) == "a = 1\n")
        // So does a link, even a dangling one.
        try FileManager.default.createSymbolicLink(atPath: home + "/x.json", withDestinationPath: home + "/missing")
        #expect(try await sh(HarnessAddition.writeFileScript, home: home, ["x.json"], stdin: "{}") == 10)
        #expect(!FileManager.default.fileExists(atPath: home + "/missing"))
    }

    @Test func aSecretLinkIsMadeWithItsDirectory() async throws {
        let home = NSTemporaryDirectory() + "glov-add-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: home) }
        #expect(try await sh(HarnessAddition.linkScript, home: home, [".gemini/token", "/run/glov/secrets/x"]) == 0)
        #expect(try FileManager.default.destinationOfSymbolicLink(atPath: home + "/.gemini/token") == "/run/glov/secrets/x")
    }

    /// A VM created without --harness has no signed-in guest user, so `vm harness add` refuses it up front.
    @Test func aVMCreatedWithoutAHarnessIsRefused() {
        let want = "VM plain was created without --harness, which glov vm harness add cannot make up for: "
            + "create a harness VM with glov vm create <vm> --engine vz --isolated --harness claude"
        #expect(HarnessAddition.refusal(vm: "plain", harnesses: nil, harness: "claude") == want)
        #expect(HarnessAddition.refusal(vm: "plain", harnesses: [], harness: "claude") == want)
        #expect(HarnessAddition.refusal(vm: "box", harnesses: ["codex"], harness: "claude") == nil)
    }
}
