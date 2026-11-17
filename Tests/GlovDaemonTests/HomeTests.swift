import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

@Suite struct HomeTests {
    func makeHome() throws -> GlovHome {
        let home = GlovHome(path: try shortTempDir())
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        return home
    }

    func config(_ name: String, in home: GlovHome) -> VMConfig {
        VMConfig(name: name, directory: home.directory(of: name), cpus: 4, memory: "8G", disk: "/d/disk.qcow2",
            firmwareCode: home.path + "/code.fd", tpm: false, sshPort: 2222,
            extraDrives: [.init(path: "/i/win.iso", cdrom: true, bootIndex: 1)])
    }

    @Test func vmJSONRoundTrips() throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let original = config("win11", in: home)
        try FileManager.default.createDirectory(atPath: original.directory, withIntermediateDirectories: true)
        try JSONEncoder().encode(original).write(to: URL(fileURLWithPath: home.directory(of: "win11") + "/vm.json"))
        #expect(try home.loadConfig("win11") == original)
    }

    @Test func aDirectoryWithoutVMJSONIsNotAVM() throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try FileManager.default.createDirectory(atPath: home.directory(of: "scratch"), withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: home.path + "/glovd.log", contents: nil)
        try home.create(config("a", in: home))
        #expect(home.vmNames() == ["a"])
    }

    /// A directory another create has just made (no vm.json yet) is refused, and its files are left alone, so a failing
    /// create never removes another VM's directory.
    @Test func aNewVMDirectoryIsMadeOnceAndAnExistingOneIsLeftAlone() throws {
        let home = GlovHome(path: try shortTempDir() + "/home")
        defer { try? FileManager.default.removeItem(atPath: (home.path as NSString).deletingLastPathComponent) }
        let dir = try home.makeVMDirectory("a")
        #expect(try FileManager.default.attributesOfItem(atPath: dir)[.posixPermissions] as? Int == 0o700)
        FileManager.default.createFile(atPath: dir + "/disk.img", contents: Data("in progress".utf8))
        #expect(throws: HomeError.exists("a")) { try home.makeVMDirectory("a") }
        #expect(FileManager.default.contents(atPath: dir + "/disk.img") == Data("in progress".utf8))
        #expect(throws: HomeError.invalidName("../x")) { try home.makeVMDirectory("../x") }
    }

    @Test func createRefusesToOverwriteAndRejectsBadNames() throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try home.create(config("a", in: home))
        #expect(throws: HomeError.exists("a")) { try home.create(config("a", in: home)) }
        #expect(throws: HomeError.invalidName("../x")) { try home.create(config("../x", in: home)) }
        #expect(throws: HomeError.invalidName(".hidden")) { try home.create(config(".hidden", in: home)) }
    }

    @Test func createMakesABlankVarsStoreTheSizeOfTheFirmware() throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try home.create(config("a", in: home))
        #expect(try FileManager.default.attributesOfItem(atPath: home.directory(of: "a") + "/vars.fd")[.size] as? Int == 4096)
    }

    @Test func createRequiresTheFirmwareFile() throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        var c = config("a", in: home)
        c.firmwareCode = "/nonexistent/code.fd"
        #expect(throws: HomeError.missingFirmware("/nonexistent/code.fd")) { try home.create(c) }
        #expect(home.vmNames().isEmpty)
    }

    @Test func resolvesExactNamesThenUniquePrefixes() throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        for n in ["win11", "win11-test", "ubuntu"] { try home.create(config(n, in: home)) }
        #expect(try home.resolve("win11") == "win11")  // exact wins over the longer name sharing the prefix
        #expect(try home.resolve("ub") == "ubuntu")
        #expect(throws: HomeError.ambiguous("wi", matches: ["win11", "win11-test"])) { try home.resolve("wi") }
        #expect(throws: HomeError.noSuchVM("x", known: ["ubuntu", "win11", "win11-test"])) { try home.resolve("x") }
        #expect(throws: HomeError.self) { try home.resolve("") }
    }

    @Test func environmentOverridesTheDefaultRoot() {
        #expect(GlovHome.resolve(environment: ["GLOV_HOME": "/tmp/gp.x"]).path == "/tmp/gp.x")
        #expect(GlovHome.resolve(explicit: "/tmp/gp.y", environment: ["GLOV_HOME": "/tmp/gp.x"]).path == "/tmp/gp.y")
        #expect(GlovHome.resolve(environment: [:]).path.hasSuffix("/Library/Application Support/glov"))
    }

    @Test func launchAgentPlistKeepsTheDaemonAliveAndInteractive() throws {
        let home = GlovHome(path: "/h/glov")
        let data = try LaunchAgent.plist(executable: "/usr/local/bin/glovd", home: home)
        let plist = try #require(PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any])
        #expect(plist["Label"] as? String == "org.glov.daemon")
        #expect(plist["ProgramArguments"] as? [String] == ["/usr/local/bin/glovd", "--home", "/h/glov"])
        #expect(plist["RunAtLoad"] as? Bool == true)
        #expect(plist["KeepAlive"] as? Bool == true)
        #expect(plist["ProcessType"] as? String == "Interactive")
        #expect(plist["AbandonProcessGroup"] as? Bool == true)
        #expect(plist["StandardOutPath"] as? String == "/h/glov/glovd.log")
        #expect(plist["StandardErrorPath"] as? String == "/h/glov/glovd.log")
    }
}

@Suite struct DriveSpecTests {
    @Test func parsesPathAndOptions() throws {
        #expect(try VMConfig.Drive(spec: "/i/win.iso,cdrom,bootindex=1") == .init(path: "/i/win.iso", cdrom: true, bootIndex: 1))
        #expect(try VMConfig.Drive(spec: "/i/virtio.iso,cdrom,removable") == .init(path: "/i/virtio.iso", cdrom: true, removable: true))
        #expect(try VMConfig.Drive(spec: "/i/x.img") == .init(path: "/i/x.img"))
    }

    @Test func rejectsUnknownOptions() {
        #expect(throws: DriveSpecError.self) { try VMConfig.Drive(spec: "/i/x,fast") }
        #expect(throws: DriveSpecError.self) { try VMConfig.Drive(spec: "/i/x,bootindex=a") }
        #expect(throws: DriveSpecError.self) { try VMConfig.Drive(spec: ",cdrom") }
    }
}

@Suite struct PortForwardSpecTests {
    @Test func parsesHostAndGuest() throws {
        #expect(try VMConfig.PortForward(spec: "13389:3389") == .init(host: 13389, guest: 3389))
    }

    @Test func rejectsMalformedSpecs() {
        for spec in ["8080", "8080:", ":80", "a:80", "0:80", "8080:65536", "1:2:3"] {
            #expect(throws: DriveSpecError.self) { try VMConfig.PortForward(spec: spec) }
        }
    }
}

@Suite struct HomeSaveTests {
    @Test func updateRewritesAnExistingVMOnly() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        let c = VMConfig(name: "a", directory: home.directory(of: "a"), firmwareCode: home.path + "/code.fd", tpm: false)
        #expect(throws: HomeError.self) { try home.update("a") { $0.cpus = 7 } }
        #expect(!FileManager.default.fileExists(atPath: home.directory(of: "a")), "no lock file is left for a missing VM")
        try home.create(c)
        #expect(try home.update("a") { $0.cpus = 7 }.cpus == 7)
        #expect(try home.loadConfig("a").cpus == 7)
        struct Refused: Error {}
        #expect(throws: Refused.self) { try home.update("a") { $0.cpus = 9; throw Refused() } }
        #expect(try home.loadConfig("a").cpus == 7, "a change that throws writes nothing")
    }

    /// glov adds shares in its own process while glovd removes egress hosts: no removed host comes back, and no
    /// share is lost.
    @Test func concurrentUpdatesFromSeparateWritersAreAllKept() async throws {
        let home = GlovHome(path: try shortTempDir())
        let folders = try shortTempDir()  // outside the home, which a share may not overlap
        defer {
            try? FileManager.default.removeItem(atPath: home.path)
            try? FileManager.default.removeItem(atPath: folders)
        }
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        var c = VMConfig(name: "a", directory: home.directory(of: "a"), firmwareCode: home.path + "/code.fd", tpm: false)
        let hosts = (0..<16).map { "h\($0).example.com" }
        c.egress = .init(allow: hosts)
        try home.create(c)
        for i in 0..<16 { try FileManager.default.createDirectory(atPath: folders + "/\(i)", withIntermediateDirectories: false) }
        try await withThrowingTaskGroup(of: Void.self) { group in
            for i in 0..<16 {
                // Each on a thread of its own with its own lock descriptor, as separate processes would be.
                group.addTask {
                    _ = try await Blocking.run { try home.addShare(vm: "a", .init(name: "s\(i)", hostPath: folders + "/\(i)", guestPath: "C:\\s\(i)")) }
                }
                group.addTask {
                    _ = try await Blocking.run { try home.update("a") { $0.egress?.allow.removeAll { $0 == hosts[i] } } }
                }
            }
            try await group.waitForAll()
        }
        let config = try home.loadConfig("a")
        #expect(config.egress?.allow == [])
        #expect(Set(config.shares.map(\.name)) == Set((0..<16).map { "s\($0)" }))
    }

    @Test func daemonInstallRunsASignedCopyInTheHome() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        // Any Mach-O stands in for glovd; codesign re-signs the copy ad hoc.
        let installed = try await offPool { try LaunchAgent.installExecutable(from: "/usr/bin/true", home: home) }
        #expect(installed == home.path + "/bin/glovd")
        #expect(FileManager.default.contentsEqual(atPath: installed, andPath: "/usr/bin/true") == false)  // re-signed
        let shown = try await offPool { try LinuxProvisioner.runTool("/usr/bin/codesign", ["-d", "--entitlements", "-", installed]) }
        #expect(shown.status == 0 && shown.output.contains("com.apple.security.virtualization"))
        #expect(try await offPool { try LinuxProvisioner.runTool(installed, []) }.status == 0)
        #expect(try FileManager.default.contentsOfDirectory(atPath: home.path + "/bin") == ["glovd"])

        // A second install replaces it in place.
        #expect(try await offPool { try LaunchAgent.installExecutable(from: "/usr/bin/true", home: home) } == installed)
    }

    @Test func aFailedSignatureLeavesTheInstalledDaemonAlone() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let installed = try await offPool { try LaunchAgent.installExecutable(from: "/usr/bin/true", home: home) }
        let before = try Data(contentsOf: URL(fileURLWithPath: installed))
        #expect(throws: LaunchctlError.self) {
            try LaunchAgent.installExecutable(from: "/usr/bin/false", home: home, run: { _, _ in (1, "no identity") })
        }
        #expect(try Data(contentsOf: URL(fileURLWithPath: installed)) == before)
        #expect(try FileManager.default.contentsOfDirectory(atPath: home.path + "/bin") == ["glovd"])
    }

    @Test func theInstalledDaemonCarriesItsLocalNetworkUsageDescription() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let built = try builtBinary("glovd")
        let info = CFBundleCopyInfoDictionaryForURL(URL(fileURLWithPath: built) as CFURL) as? [String: Any]
        #expect(info?["CFBundleIdentifier"] as? String == LaunchAgent.label)
        #expect((info?["NSLocalNetworkUsageDescription"] as? String)?.isEmpty == false)
        // The ad hoc signature binds the plist and takes its identifier, which the Local Network setting lists.
        let installed = try await offPool { try LaunchAgent.installExecutable(from: built, home: home) }
        let shown = try await offPool { try LinuxProvisioner.runTool("/usr/bin/codesign", ["-dv", installed]) }
        #expect(shown.output.contains("Identifier=\(LaunchAgent.label)") && shown.output.contains("Info.plist entries="), "\(shown.output)")
    }

    @Test func theEmbeddedEntitlementMatchesTheSigningScript() throws {
        let repo = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let file = try String(contentsOf: repo.appendingPathComponent("tools/linux-vm/glovd.entitlements"), encoding: .utf8)
        #expect(LaunchAgent.entitlements == file)
    }
}
