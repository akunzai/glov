import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

/// `glov vm create` (what needs no download) and `glov vm delete` as child processes, with no daemon running.
@Suite struct CLIVMTests {
    private func makeHome() throws -> GlovHome {
        let home = GlovHome(path: try shortTempDir())
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        return home
    }

    @Test(.timeLimit(.minutes(1))) func registeringADiskIsUnchanged() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let r = try await runglov(
            ["vm", "create", "--no-start", "win11", "--firmware-code", home.path + "/code.fd", "--disk", "/d/disk.qcow2", "--ssh-port", "2222", "--forward", "8080:80"],
            home: home)
        #expect(r.status == 0, "\(r.err)")
        let c = try home.loadConfig("win11")
        let uuid = try #require(c.uuid)
        #expect(UUID(uuidString: uuid) != nil && uuid == uuid.lowercased())
        let mac = try #require(c.macAddress)
        #expect(mac.wholeMatch(of: /^([0-9a-f]{2}:){5}[0-9a-f]{2}$/) != nil)
        let serial = try #require(c.diskSerial)
        #expect(serial.hasPrefix("glov-") && serial.count == 17)
        #expect(c == VMConfig(
            name: "win11", directory: home.directory(of: "win11"), cpus: 2, memory: "4G", disk: "/d/disk.qcow2", firmwareCode: home.path + "/code.fd",
            tpm: true, sshPort: 2222, portForwards: [.init(host: 8080, guest: 80)],
            macAddress: mac, uuid: uuid, diskSerial: serial))
        #expect(!c.network && c.install == nil && c.extraDrives.isEmpty)
    }

    @Test(.timeLimit(.minutes(1))) func aHarnessAllowsItsRuntimeHostsAndListsItsSecrets() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let image = home.path + "/cloud.raw"
        FileManager.default.createFile(atPath: image, contents: Data(count: 4096))
        let agent = home.path + "/glov-guest"
        FileManager.default.createFile(atPath: agent, contents: Data("ELF".utf8), attributes: [.posixPermissions: 0o755])
        let r = try await runglov(
            ["vm", "create", "--no-start", "iso", "--engine", "vz", "--image", image, "--guest-service", agent, "--disk-size", "1M", "--isolated",
             "--harness", "claude", "--harness", "codex"], home: home)
        #expect(r.status == 0, "\(r.err)")
        let c = try home.loadConfig("iso")
        #expect(c.egress == .init(allow: ["api.anthropic.com", "api.openai.com"]))
        #expect(c.harnesses == ["claude", "codex"])
        // Its conversation records go to a share of their own in the glov home, outside the VM's directory.
        let records = HarnessSessions.hostPath(home: home.path, vm: "iso")
        #expect(c.shares == [.init(name: HarnessSessions.shareName, hostPath: HostPath.realpath(records)!, guestPath: "/mnt/glov-sessions")])
        #expect(r.out.contains("conversation records: \(records) (for review on this Mac; never resume them there)"), "\(r.out)")
        #expect((try FileManager.default.attributesOfItem(atPath: records)[.posixPermissions] as? Int) == 0o700)
        let none = try await runglov(
            ["vm", "create", "--no-start", "iso2", "--engine", "vz", "--image", image, "--guest-service", agent, "--disk-size", "1M", "--isolated",
             "--harness", "claude", "--no-sessions"], home: home)
        let iso2 = try home.loadConfig("iso2")
        #expect(none.status == 0 && iso2.shares.isEmpty, "\(none.err)")
        #expect(r.out.contains("harness claude: secret anthropic (a placeholder in ANTHROPIC_API_KEY, sent to api.anthropic.com) is not granted: glov secret grant iso anthropic (define it first if glov secret list lacks it: glov secret set anthropic --host api.anthropic.com)"), "\(r.out)")
        #expect(r.out.contains("harness codex: secret openai (a placeholder in CODEX_API_KEY, sent to api.openai.com) is not granted: glov secret grant iso openai (define it first if glov secret list lacks it: glov secret set openai --host api.openai.com)"), "\(r.out)")
    }

    @Test(.timeLimit(.minutes(1))) func anIsolatedVMCanStartOpenAndGlovClaudeKeepsAnExistingVMsMode() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let image = home.path + "/cloud.raw"
        FileManager.default.createFile(atPath: image, contents: Data(count: 4096))
        let agent = home.path + "/glov-guest"
        FileManager.default.createFile(atPath: agent, contents: Data("ELF".utf8), attributes: [.posixPermissions: 0o755])
        let r = try await runglov(
            ["vm", "create", "--no-start", "iso", "--engine", "vz", "--image", image, "--guest-service", agent, "--disk-size", "1M", "--isolated",
             "--harness", "claude", "--no-sessions", "--egress", "open"], home: home)
        #expect(r.status == 0, "\(r.err)")
        #expect(try home.loadConfig("iso").egress == .init(allow: ["api.anthropic.com"], open: true))
        #expect(r.out.contains("(engine vz, isolated, egress open)") && r.out.contains("egress open: any public host through the egress proxy"), "\(r.out)")
        let plain = try await runglov(["vm", "create", "--no-start", "nic", "--engine", "vz", "--egress", "open"], home: home)
        #expect(plain.status != 0 && plain.err.contains("--egress needs --isolated"), "\(plain.err)")

        // glov claude in a project whose VM exists restricted refuses --egress open instead of switching it.
        let project = HostPath.realpath(home.path)! + "/proj"
        try FileManager.default.createDirectory(atPath: project, withIntermediateDirectories: true)
        let vm = ProjectAgent.vmName(project: project)
        try home.create(VMConfig(
            name: vm, directory: home.directory(of: vm), engine: .vz, firmwareCode: "",
            shares: [.init(name: ProjectAgent.shareName, hostPath: project, guestPath: project)], isolated: true, egress: .init()))
        let claude = try await runglov(["claude", "--egress", "open"], home: home, directory: project)
        #expect(claude.status != 0 && claude.err.contains("VM \(vm) already exists with egress restricted: change it with glov vm configure \(vm) --egress open"), "\(claude.err)")
    }

    @Test(.timeLimit(.minutes(1))) func harnessInstallNeedsAnIsolatedVMCreatedWithAHarness() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try home.create(VMConfig(name: "nic", directory: home.directory(of: "nic"), engine: .vz, firmwareCode: ""))
        try home.create(VMConfig(name: "bare", directory: home.directory(of: "bare"), engine: .vz, firmwareCode: "", isolated: true, egress: .init()))
        for (vm, message) in [
            ("nic", "VM nic is not isolated: a harness VM is created with glov vm create nic --engine vz --isolated --harness <name>"),
            ("bare", "VM bare has no harness: it was created without --harness"),
            ("nope", "no VM named"),
        ] {
            let r = try await runglov(["vm", "harness", "install", vm], home: home)
            #expect(r.status != 0 && r.err.contains(message), "\(vm): \(r.err)")
        }
    }

    @Test(.timeLimit(.minutes(1))) func aWindowsInstallRefusesOptionsThatCannotWork() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let cases: [([String], String)] = [
            (["vm", "create", "--no-start", "w", "--guest-service", "a.exe", "--ssh-port", "2222"], "--ssh-port needs --ssh"),
            (["vm", "create", "--no-start", "w", "--guest-service", "a.exe", "--no-tpm"], "needs a TPM"),
            (["vm", "create", "--no-start", "w", "--guest-service", "a.exe", "--edition", "Education"], "cannot install edition Education"),
            (["vm", "create", "--no-start", "w", "--guest-service", "a.exe", "--disk-size", "lots"], "cannot read --disk-size"),
            (["vm", "create", "--no-start", "w", "--disk", "/d.qcow2", "--firmware-code", "/c.fd", "--ssh"], "for a new Windows install"),
            (["vm", "create", "--no-start", "w", "--guest-service", "a.exe", "--time-zone", "Mars Standard Time"], "unknown Windows time zone"),
            (["vm", "create", "--no-start", "w", "--disk", "/d.qcow2", "--firmware-code", "/c.fd", "--time-zone", "Taipei Standard Time"], "for a new Windows install"),
            (["vm", "create", "--no-start", "w", "--engine", "vz", "--time-zone", "Taipei Standard Time"], "are for --engine qemu"),
            (["vm", "create", "--no-start", "w", "--disk", "/d.qcow2"], "--firmware-code is required with --disk"),
            (["vm", "create", "--no-start", "w", "--engine", "vz", "--ssh"], "are for --engine qemu"),
            (["vm", "create", "--no-start", "w", "--engine", "vz", "--harness", "claude"], "--harness needs --isolated"),
            (["vm", "create", "--no-start", "w", "--engine", "vz", "--isolated", "--harness", "aider"], "unknown harness aider: use antigravity, claude or codex"),
            (["vm", "create", "--no-start", "w", "--guest-service", "a.exe", "--harness", "claude"], "--harness needs --isolated"),
        ]
        for (args, message) in cases {
            let r = try await runglov(args, home: home)
            #expect(r.status != 0 && r.err.contains(message), "\(args): \(r.err)")
        }
        #expect(home.vmNames().isEmpty)
    }

    @Test(.timeLimit(.minutes(1))) func deleteAsksThenRemovesTheWholeDirectory() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        for n in ["a", "ab"] {
            try home.create(VMConfig(name: n, directory: home.directory(of: n), firmwareCode: home.path + "/code.fd", tpm: false))
        }
        let secrets = home.directory(of: "a") + "/secrets"
        try FileManager.default.createDirectory(atPath: secrets, withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: secrets + "/admin-password", contents: Data("x".utf8))

        var r = try await runglov(["vm", "delete", "a"], home: home, stdin: "n\n")
        #expect(r.status != 0 && r.err.contains("This cannot be undone. [y/N]") && r.err.contains("not deleted"))
        #expect(FileManager.default.fileExists(atPath: secrets + "/admin-password"))
        r = try await runglov(["vm", "delete", "a"], home: home)  // no answer is no
        #expect(r.status != 0 && FileManager.default.fileExists(atPath: home.directory(of: "a")))

        r = try await runglov(["vm", "delete", "a"], home: home, stdin: "yes\n")
        #expect(r.status == 0, "\(r.err)")
        #expect(!FileManager.default.fileExists(atPath: home.directory(of: "a")))

        r = try await runglov(["vm", "delete", "a"], home: home, stdin: "y\n")
        #expect(r.status != 0 && r.err.contains("no VM named \"a\""), "a prefix of ab is not enough: \(r.err)")
        r = try await runglov(["vm", "delete", "ab", "--yes"], home: home)
        #expect(r.status == 0 && home.vmNames().isEmpty, "\(r.err)")
    }

    @Test func deleteRefusesARunningVM() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try home.create(VMConfig(name: "a", directory: home.directory(of: "a"), firmwareCode: home.path + "/code.fd", tpm: false))
        var asked = false
        await #expect(throws: ProvisionError.self) {
            _ = try await VMDeletion.delete("a", in: home, isRunning: { _ in true }, confirm: { _ in asked = true; return true })
        }
        #expect(!asked && FileManager.default.fileExists(atPath: home.directory(of: "a")))
        #expect(VMDeletion.isYes("Y ") && VMDeletion.isYes("yes") && !VMDeletion.isYes("") && !VMDeletion.isYes(nil) && !VMDeletion.isYes("no"))
    }

    @Test(.timeLimit(.minutes(1))) func windowsMediaAndInstallsDefaultToEnglish() async throws {
        #expect(WindowsProvisioner.defaultLanguage == "en-US")
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        for args in [["host", "media", "--help"], ["vm", "create", "--no-start", "--help"]] {
            let r = try await runglov(args, home: home)
            #expect(r.status == 0 && (r.out.contains("default: en-US") || r.out.contains("(default en-US)")), "\(args): \(r.out)")
        }
    }

    @Test func languageTagsAreSpelledAsWindowsDoes() {
        #expect(WindowsAssets.languageTag("zh-tw") == "zh-TW" && WindowsAssets.languageTag("EN-us") == "en-US")
        #expect(WindowsAssets.languageTag("sr-latn-rs") == "sr-latn-rs")
    }

    @Test(.timeLimit(.minutes(1))) func windowsInstallFailsFastBeforeDownloadingWhenPrerequisitesAreMissing() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        if WindowsProvisioner.missingHostTools().isEmpty {
            let missingFW = try await runglov(
                ["vm", "create", "--no-start", "w", "--firmware-code", home.path + "/missing.fd"], home: home)
            #expect(missingFW.status != 0 && missingFW.err.contains("UEFI firmware not found:"), "\(missingFW.err)")

            let badAgent = home.path + "/bad-agent"
            FileManager.default.createFile(atPath: badAgent, contents: Data([0x7f, 0x45, 0x4c, 0x46]))
            let invalidAgent = try await runglov(
                ["vm", "create", "--no-start", "w", "--firmware-code", home.path + "/code.fd", "--guest-service", badAgent], home: home)
            #expect(invalidAgent.status != 0 && invalidAgent.err.contains("not a Windows executable:"), "\(invalidAgent.err)")
        } else {
            let missingTools = try await runglov(["vm", "create", "--no-start", "w"], home: home)
            #expect(missingTools.status != 0 && missingTools.err.contains("missing host tools for Windows VMs:") && missingTools.err.contains("brew install"), "\(missingTools.err)")
        }
        #expect(home.vmNames().isEmpty)
    }
}
