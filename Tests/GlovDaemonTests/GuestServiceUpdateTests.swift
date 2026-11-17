import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI
@testable import GlovDaemon

@Suite struct GuestServiceUpdateTests {
    // MARK: binary headers

    private func pe(machine: UInt16) -> Data {
        var d = Data(count: 0x100)
        d[0] = 0x4d; d[1] = 0x5a; d[0x3c] = 0x80
        d.replaceSubrange(0x80..<0x84, with: [0x50, 0x45, 0, 0])
        d[0x84] = UInt8(machine & 0xff); d[0x85] = UInt8(machine >> 8)
        return d
    }

    private func elf(machine: UInt16, class64: Bool = true, little: Bool = true) -> Data {
        var d = Data(count: 64)
        d.replaceSubrange(0..<4, with: [0x7f, 0x45, 0x4c, 0x46])
        d[4] = class64 ? 2 : 1; d[5] = little ? 1 : 2
        d[18] = UInt8(machine & 0xff); d[19] = UInt8(machine >> 8)
        return d
    }

    @Test func readsOSAndArchitectureFromTheExecutableHeader() throws {
        #expect(try GuestServiceBinary.inspect(pe(machine: 0xAA64)) == GuestServiceBinary(os: "windows", arch: "arm64"))
        #expect(try GuestServiceBinary.inspect(pe(machine: 0x8664)) == GuestServiceBinary(os: "windows", arch: "amd64"))
        #expect(try GuestServiceBinary.inspect(elf(machine: 183)) == GuestServiceBinary(os: "linux", arch: "arm64"))
        #expect(try GuestServiceBinary.inspect(elf(machine: 62)) == GuestServiceBinary(os: "linux", arch: "amd64"))
    }

    @Test func refusesWhatIsNotAWindowsOrLinuxExecutable() {
        #expect(throws: GuestServiceBinary.Unreadable.self) { try GuestServiceBinary.inspect(Data([0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0, 0, 1])) }  // Mach-O
        #expect(throws: GuestServiceBinary.Unreadable.self) { try GuestServiceBinary.inspect(Data("#!/bin/sh\n".utf8)) }
        #expect(throws: GuestServiceBinary.Unreadable.self) { try GuestServiceBinary.inspect(Data()) }
        #expect(throws: GuestServiceBinary.Unreadable.self) { try GuestServiceBinary.inspect(Data([0x4d, 0x5a])) }  // PE pointer missing
    }

    @Test func parsesHashOutputOfBothHashers() {
        let h = String(repeating: "ab", count: 32)
        #expect(GuestServiceUpdate.parseHash("\(h)  /usr/local/bin/glov-guest\n") == h)
        #expect(GuestServiceUpdate.parseHash("\r\n\(h.uppercased())\r\n") == h)
        #expect(GuestServiceUpdate.parseHash("sha256sum: x: No such file or directory") == nil)
    }

    @Test func explainsWhyAVMCannotBeUpdated() {
        func reason(_ info: Results.VMInfo) -> String {
            do { _ = try GuestServiceUpdate.requireReadyGuestService(info); return "" } catch { return "\(error)" }
        }
        #expect(reason(.init(name: "a", state: "stopped")).contains("not running"))
        #expect(reason(.init(name: "a", state: "installing")).contains("still installing"))
        #expect(reason(.init(name: "a", state: "install-failed", installFailure: "boom")).contains("boom"))
        #expect(reason(.init(name: "a", state: "running", agent: .init(state: "disabled"))).contains("no Guest Service"))
        #expect(reason(.init(name: "a", state: "running", agent: .init(state: "connecting"))).contains("not ready"))
        #expect(reason(.init(name: "a", state: "running", agent: .init(state: "ready", os: "linux"))) == "")
    }

    @Test func bundledTargetValidatesManifestAndExecutable() throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        let dir = root + "/windows-arm64"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let path = dir + "/glov-guest.exe"
        try pe(machine: 0xAA64).write(to: URL(fileURLWithPath: path))
        let hash = try GuestServiceBinary.sha256(path: path)
        try Data("{\"version\":\"build-7\",\"sha256\":\"\(hash)\"}".utf8).write(to: URL(fileURLWithPath: dir + "/manifest.json"))
        let target = try GuestServiceDistribution.target(os: "windows", directory: root)
        #expect(target.path == path && target.version == "build-7")
        try pe(machine: 0x8664).write(to: URL(fileURLWithPath: path))
        #expect(throws: (any Error).self) { try GuestServiceDistribution.target(os: "windows", directory: root) }
        #expect(throws: (any Error).self) { try GuestServiceDistribution.target(os: "linux", directory: root) }
    }

    @Test func installsBothGuestServicesRemovesObsoleteBundleAndPreservesAssetsWhenSourceIsInvalid() throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        for (os, data) in [("windows", pe(machine: 0xAA64)), ("linux", elf(machine: 183))] {
            let dir = root + "/source/guest-services/\(os)-arm64"
            try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
            let path = dir + (os == "windows" ? "/glov-guest.exe" : "/glov-guest")
            try data.write(to: URL(fileURLWithPath: path))
            let hash = try GuestServiceBinary.sha256(path: path)
            try Data("{\"version\":\"build-8\",\"sha256\":\"\(hash)\"}".utf8).write(to: URL(fileURLWithPath: dir + "/manifest.json"))
        }
        let obsolete = root + "/installed/agents"
        try FileManager.default.createDirectory(atPath: obsolete, withIntermediateDirectories: true)
        try Data("old bundle".utf8).write(to: URL(fileURLWithPath: obsolete + "/marker"))
        try GuestServiceDistribution.install(from: root + "/source", to: root + "/installed")
        #expect(!FileManager.default.fileExists(atPath: obsolete))
        #expect(try GuestServiceDistribution.target(os: "windows", directory: root + "/installed/guest-services").version == "build-8")
        #expect(try GuestServiceDistribution.target(os: "linux", directory: root + "/installed/guest-services").version == "build-8")
        try Data("broken".utf8).write(to: URL(fileURLWithPath: root + "/source/guest-services/linux-arm64/manifest.json"))
        try FileManager.default.createDirectory(atPath: obsolete, withIntermediateDirectories: true)
        #expect(throws: (any Error).self) { try GuestServiceDistribution.install(from: root + "/source", to: root + "/installed") }
        #expect(FileManager.default.fileExists(atPath: obsolete))
        #expect(try GuestServiceDistribution.target(os: "windows", directory: root + "/installed/guest-services").version == "build-8")
    }

    @Test func bundledDistributionLivesBesideTheExecutable() {
        let executable = URL(fileURLWithPath: Bundle.main.executablePath ?? CommandLine.arguments[0]).resolvingSymlinksInPath()
        #expect(GuestServiceDistribution.directory == executable.deletingLastPathComponent().path + "/guest-services")
    }

    // MARK: against a fake agent

    private let old = String(repeating: "11", count: 32)

    /// A guest whose installed Guest Service hashes to `old` until the detached install runs, then to `staged`, which is what the
    /// copy in the guest hashes to. The install drops the Guest Service's connection, as the restart does.
    private func guest(_ f: GuestServiceFixture, os: String, staged: String, version: String = "0.0-fake", fastRestart: Bool = false) {
        f.fake.os = os
        let installed = Box(old)
        f.fake.script = { [fake = f.fake] argv in
            let installedQuery = argv.contains { $0.contains("Win32_Service") || $0 == GuestServiceUpdate.linuxInstalled }
            switch argv.first ?? "" {
            case "sha256sum", "powershell.exe": return [.out("stdout", "\(installedQuery ? installed.value : staged)  x\n"), .exit(0)]
            case "systemd-run", "schtasks":
                if argv.first == "schtasks", !argv.contains("/run") { return [.exit(0)] }
                installed.value = staged
                if fastRestart {
                    DispatchQueue.global().asyncAfter(deadline: .now() + 0.01) { fake.sendReady() }
                } else { restart(fake) }
                return [.exit(0)]
            default: return [.out("stdout", argv.last == "--version" ? "\(version)\n" : ""), .exit(0)]
            }
        }
    }

    /// The Guest Service goes away shortly after the launching command returned, as it does when the install stops it.
    private func restart(_ fake: FakeGuestService) { DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) { fake.dropConnection() } }

    private func update(_ f: GuestServiceFixture, binary: Data) throws -> GuestServiceUpdate {
        let path = f.shared + "/agent-bin"
        try FileManager.default.createDirectory(atPath: f.shared, withIntermediateDirectories: true)
        try binary.write(to: URL(fileURLWithPath: path))
        var u = GuestServiceUpdate(vm: "win11", binary: path, home: f.home, client: f.client)
        // The daemon has no VM process here, so it reports the VM stopped. Report the fake agent: ready, except after the
        // install ran and until the host has reconnected (the restart drops the connection a little later, which a slow
        // host may not have seen yet).
        let before = f.fake.accepted
        u.adjustStatus = { [fake = f.fake] i in
            var i = i
            i.state = "running"
            let ran = fake.requests("exec.start").contains {
                "\($0["argv"] ?? [])".contains("systemd-run") || "\($0["argv"] ?? [])".contains("start-stop-daemon")
                    || "\($0["argv"] ?? [])".contains("\"/run\"")
            }
            let restarting = ran && fake.accepted <= before
            i.agent = .init(state: restarting ? "connecting" : "ready", os: fake.os, version: "0.0-fake")
            return i
        }
        u.timeout = 10
        u.pollInterval = .milliseconds(50)
        return u
    }

    private func started(_ f: GuestServiceFixture) -> [String] { f.fake.requests("exec.start").map { "\($0["argv"] ?? [])" } }

    @Test(.timeLimit(.minutes(1)))
    func checksBundledGuestServiceOncePerConnectionAndRefreshesAfterReconnect() async throws {
        let root = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: root) }
        let dir = root + "/windows-arm64"
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let path = dir + "/glov-guest.exe"
        try pe(machine: 0xAA64).write(to: URL(fileURLWithPath: path))
        let hash = try GuestServiceBinary.sha256(path: path)
        try Data("{\"version\":\"build-7\",\"sha256\":\"\(hash)\"}".utf8).write(to: URL(fileURLWithPath: dir + "/manifest.json"))
        let f = try await GuestServiceFixture(guestServiceDirectory: root)
        defer { f.cleanup() }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        let installed = Box(hash)
        f.fake.script = { _ in [.out("stdout", installed.value), .exit(0)] }
        f.fake.dropConnection()
        for _ in 0..<200 {
            if await a.status().update?.state == "up-to-date" { break }
            try await Task.sleep(for: .milliseconds(25))
        }
        #expect(await a.status().update?.state == "up-to-date")
        #expect(await a.status().update?.targetVersion == "build-7")
        let count = f.fake.requests("exec.start").count
        for _ in 0..<5 { _ = await a.status() }
        #expect(f.fake.requests("exec.start").count == count)
        installed.value = old
        f.fake.dropConnection()
        for _ in 0..<200 {
            if await a.status().update?.state == "update-available" { break }
            try await Task.sleep(for: .milliseconds(25))
        }
        #expect(await a.status().update?.state == "update-available")
        try FileManager.default.removeItem(atPath: path)
        let unavailable = await a.checkGuestService()
        #expect(unavailable.state == "unknown" && unavailable.problem != nil)
        #expect(f.fake.requests("file.write").isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func confirmsSameVersionRestartEvenWhenNoPollSeesGuestServiceDisconnect() async throws {
        let f = try await GuestServiceFixture(running: true)
        defer { f.cleanup() }
        let u = try update(f, binary: pe(machine: 0xAA64))
        guest(f, os: "windows", staged: try GuestServiceBinary.sha256(path: u.binary), fastRestart: true)
        let outcome: GuestServiceUpdate.Outcome = try await f.client.call(
            Method.guestServiceUpdate, Params.GuestService(vm: "win11", binary: u.binary, timeout: 2))
        #expect(outcome == .updated(from: "0.0-fake", to: "0.0-fake"))
    }

    @Test(.timeLimit(.minutes(1)))
    func stoppedAndDisabledVMStatusAlwaysExplainsUnknown() async throws {
        for enabled in [true, false] {
            let f = try await GuestServiceFixture(agent: enabled)
            defer { f.cleanup() }
            let status: Results.VMInfo = try await f.client.call(Method.vmStatus, Params.VM(vm: "win11"))
            #expect(status.agent?.update?.state == "unknown")
            #expect(status.agent?.update?.problem?.isEmpty == false)
            let check: Results.GuestServiceCheck = try await f.client.call(Method.guestServiceCheck, Params.GuestService(vm: "win11"))
            #expect(check.state == "unknown" && check.problem != nil)
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func linuxSharesAreCheckedAgainAfterUpdatingTheGuestService() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let config = VMConfig(name: "lin", directory: home.directory(of: "lin"), engine: .vz, firmwareCode: "", tpm: false,
            shares: [.init(name: "proj", hostPath: "/h/proj", guestPath: "/mnt/proj")])
        try home.create(config)
        let fake = try FakeGuestService(socketPath: config.guestSocketPath)
        fake.os = "linux"
        defer { fake.stop() }
        let binary = home.path + "/new-agent"
        try elf(machine: 183).write(to: URL(fileURLWithPath: binary))
        let sum = try GuestServiceBinary.sha256(path: binary)
        let installed = Box(old)
        fake.script = { argv in
            switch argv.first {
            case "stat": return [.out("stdout", "0 755 directory\n"), .exit(0)]
            case "mountpoint": return [.exit(0)]
            case "sha256sum": return [.out("stdout", (argv.last == GuestServiceUpdate.linuxInstalled ? installed.value : sum) + "\n"), .exit(0)]
            case "systemd-run": installed.value = sum; restart(fake); return [.exit(0)]
            default: return [.out("stdout", argv.last == "--version" ? "0.0-fake\n" : ""), .exit(0)]
            }
        }
        let a = VMGuestService(config: config, guestServiceDirectory: home.path + "/guest-services")
        await a.start()
        await eventually { await a.status().shares?.first?.state == "mounted" }
        _ = try await a.updateGuestService(.init(vm: "lin", binary: binary, timeout: 10), home: home)
        await eventually { await a.status().shares?.first?.state != "mounting" }
        #expect(await a.status().shares?.first?.state == "mounted")
        await a.stop()
    }

    @Test(.timeLimit(.minutes(1)))
    func daemonRejectsBusyUpdatesAndForceStillBlocksNewJobs() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: pe(machine: 0xAA64))
        guest(f, os: "windows", staged: try GuestServiceBinary.sha256(path: u.binary))
        let script = f.fake.script
        f.fake.script = { argv in argv == ["hold"] ? [.hold] : script(argv) }
        let started: Results.JobStarted = try await f.client.call(Method.execStart, Params.Exec(vm: "win11", argv: ["hold"]))
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        let error = await #expect(throws: GuestServiceUpdateError.self) {
            try await a.updateGuestService(.init(vm: "win11", binary: u.binary), home: f.home)
        }
        #expect("\(error!)".contains(started.jobId))
        #expect(f.fake.requests("file.write").isEmpty)
        let forced = Task { try await a.updateGuestService(.init(vm: "win11", binary: u.binary, force: true), home: f.home) }
        for _ in 0..<100 {
            if await a.isUpdating { break }
            try await Task.sleep(for: .milliseconds(10))
        }
        let blocked = await f.failure(Method.execStart, try JSONValue(encoding: Params.Exec(vm: "win11", argv: ["another"])))
        #expect(blocked?.message.contains("update") == true)
        _ = try await forced.value
        #expect(await a.isUpdating == false)
    }

    @Test(.timeLimit(.minutes(1)))
    func refusesABinaryForTheWrongOSOrCPUBeforeCopyingAnything() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        guest(f, os: "windows", staged: old)
        let err = await #expect(throws: GuestServiceUpdateError.self) { try await update(f, binary: elf(machine: 183)).run() }
        #expect("\(err!)".contains("linux") && "\(err!)".contains("runs windows"))
        let amd = await #expect(throws: GuestServiceUpdateError.self) { try await update(f, binary: pe(machine: 0x8664)).run() }
        #expect("\(amd!)".contains("amd64"))
        #expect(f.fake.requests("file.write").isEmpty && f.fake.requests("exec.start").isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func checksContentWithoutCopyingOrInstallingEvenWhenVersionsMatch() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: pe(machine: 0xAA64))
        guest(f, os: "windows", staged: try GuestServiceBinary.sha256(path: u.binary))
        let result = await u.check()
        #expect(result.state == "update-available")
        #expect(result.currentVersion == "0.0-fake")
        #expect(f.fake.requests("file.write").isEmpty)
        #expect(!started(f).contains { $0.contains("schtasks") })
        f.fake.script = { [sum = try GuestServiceBinary.sha256(path: u.binary)] _ in [.out("stdout", sum), .exit(0)] }
        #expect(await u.check().state == "up-to-date")
    }

    @Test(.timeLimit(.minutes(1)))
    func doesNothingWhenTheInstalledGuestServiceIsTheSameFile() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: elf(machine: 183))
        let sum = try GuestServiceBinary.sha256(path: u.binary)
        guest(f, os: "linux", staged: sum)
        f.fake.script = { _ in [.out("stdout", "\(sum)  /usr/local/bin/glov-guest\n"), .exit(0)] }
        #expect(try await u.run() == .upToDate(version: "0.0-fake"))
        #expect(f.fake.requests("file.write").isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func rejectsAGuestServiceThatStillReportsTheOldVersion() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: elf(machine: 183))
        // The staged binary says new-9; the Guest Service that comes back says 0.0-fake.
        guest(f, os: "linux", staged: try GuestServiceBinary.sha256(path: u.binary), version: "new-9")
        let err = await #expect(throws: GuestServiceUpdateError.self) { try await u.run() }
        #expect("\(err!)".contains("still reports version 0.0-fake instead of new-9"))
    }

    @Test(.timeLimit(.minutes(1)))
    func reportsAnInstallThatLeftTheOldFileInPlace() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: pe(machine: 0xAA64))
        f.fake.os = "windows"
        // Windows rolled back: the Guest Service restarts but the installed file is still the old one.
        let sum = try GuestServiceBinary.sha256(path: u.binary)
        f.fake.script = { [old, fake = f.fake] argv in
            switch argv.first ?? "" {
            case "powershell.exe": return [.out("stdout", (argv.contains { $0.contains("Win32_Service") } ? old : sum) + "\n"), .exit(0)]
            case "schtasks" where argv.contains("/run"): restart(fake); return [.exit(0)]
            default: return [.out("stdout", argv.last == "--version" ? "0.0-fake\n" : ""), .exit(0)]
            }
        }
        let err = await #expect(throws: GuestServiceUpdateError.self) { try await u.run() }
        #expect("\(err!)".contains("not the new binary") && "\(err!)".contains("guest-service.log"))
    }

    @Test(.timeLimit(.minutes(1)))
    func updatesALinuxGuestAndConfirmsTheRestart() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: elf(machine: 183))
        guest(f, os: "linux", staged: try GuestServiceBinary.sha256(path: u.binary))
        // The version is unchanged, so success rests on the observed restart and the matching file.
        #expect(try await u.run() == .updated(from: "0.0-fake", to: "0.0-fake"))
        let starts = started(f)
        #expect(starts.contains { $0.contains("systemd-run") && $0.contains("--no-block") && $0.contains("/usr/local/bin/glov-guest.next") })
        #expect(starts.contains { $0.contains("chmod") } && starts.last?.contains("rm") == true)
        #expect(f.fake.files["/usr/local/bin/glov-guest.next"] == elf(machine: 183))
        #expect(!FileManager.default.fileExists(atPath: f.home.path + "/scratch/agent-update-"))
    }

    @Test(.timeLimit(.minutes(1)))
    func updatesAWindowsGuestThroughASystemTask() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let u = try update(f, binary: pe(machine: 0xAA64))
        guest(f, os: "windows", staged: try GuestServiceBinary.sha256(path: u.binary))
        #expect(try await u.run() == .updated(from: "0.0-fake", to: "0.0-fake"))
        let starts = started(f)
        #expect(starts.contains { $0.contains("/ru") && $0.contains("SYSTEM") && $0.contains("glov-guest.exe install") && $0.contains("ProgramData") })
        #expect(starts.contains { $0.contains("/delete") })
        #expect(f.fake.files[#"C:\ProgramData\glov\update\glov-guest.exe"#] == pe(machine: 0xAA64))
    }
}

final class Box: @unchecked Sendable {
    private let lock = NSLock()
    private var _value: String
    init(_ v: String) { _value = v }
    var value: String { get { lock.withLock { _value } } set { lock.withLock { _value = newValue } } }
}
