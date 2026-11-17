import Foundation
import Testing

@testable import GlovCore

@Suite struct ReattachTests {
    @Test func aLivePidThatIsNotQemuIsIgnored() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let config = VMConfig(name: "t", directory: dir, firmwareCode: "/x", tpm: false)
        try "\(getpid())\n".write(toFile: config.pidFilePath, atomically: true, encoding: .utf8)
        // `find` needs qemu on PATH; without it the VM cannot be built, so the test has nothing to check.
        guard let vm = try? VirtualMachine(config: config) else { return }
        #expect(await vm.liveQEMUPid() == nil)
        #expect(try await vm.attach() == false)
        #expect(await vm.isRunning == false)
    }
}

/// Homebrew deletes the old QEMU keg on upgrade while the VM still runs from it.
@Suite struct DeletedExecutableTests {
    @Test func aQemuWhoseExecutableWasDeletedIsStillRecognisedAndItsPidFileKept() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let exe = dir + "/qemu-system-aarch64"
        try FileManager.default.copyItem(atPath: "/bin/sleep", toPath: exe)
        // macOS kills a copied platform binary on launch; an ad-hoc signature makes it an ordinary one.
        let sign = Process()
        sign.executableURL = URL(fileURLWithPath: "/usr/bin/codesign")
        sign.arguments = ["-f", "-s", "-", exe]
        try await runToExit(sign)
        #expect(sign.terminationStatus == 0)
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = ["30"]
        try p.run()
        defer { p.terminate(); p.waitUntilExit() }
        // Deleted before it has finished loading, the process is killed; QEMU has long been running when Homebrew deletes it.
        try await Task.sleep(for: .milliseconds(500))
        try FileManager.default.removeItem(atPath: exe)
        #expect(p.isRunning)

        let config = VMConfig(name: "t", directory: dir, firmwareCode: "/x", tpm: false)
        try "\(p.processIdentifier)\n".write(toFile: config.pidFilePath, atomically: true, encoding: .utf8)
        #expect(VirtualMachine.liveQEMUPid(pidFile: config.pidFilePath) == p.processIdentifier)

        guard let vm = try? VirtualMachine(config: config) else { return }
        await #expect(throws: GlovError.self) { try await vm.start() }
        // Stopping a VM this actor never started must not drop another live QEMU's pid file.
        await vm.stop(timeout: .zero)
        #expect(FileManager.default.fileExists(atPath: config.pidFilePath))
    }
}
