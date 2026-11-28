import CryptoKit
import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

@Suite struct WindowsProvisionerTests {
    /// A home with fake media, firmware and agent (a PE file starts with "MZ").
    private struct Fixture {
        let home: GlovHome
        var p: WindowsProvisioner

        init(name: String = "win") throws {
            home = GlovHome(path: try shortTempDir())
            let fm = FileManager.default
            for f in ["win.iso", "virtio.iso", "code.fd"] { fm.createFile(atPath: home.path + "/" + f, contents: Data(count: 4096)) }
            fm.createFile(atPath: home.path + "/agent.exe", contents: Data("MZ fake agent".utf8))
            p = WindowsProvisioner(
                name: name, iso: home.path + "/win.iso", virtio: home.path + "/virtio.iso", guestServiceBinary: home.path + "/agent.exe",
                firmwareCode: home.path + "/code.fd")
            p.diskSize = 64 << 20
            p.balloonSupported = { false }
        }

        func cleanup() { try? FileManager.default.removeItem(atPath: home.path) }
    }

    private static let toolsAvailable = (try? VirtualMachine.find("qemu-img")) != nil

    @Test(.enabled(if: toolsAvailable, "qemu-img is required")) func createsDiskSecretsUnattendImageAndConfig() async throws {
        let f = try Fixture()
        defer { f.cleanup() }
        let c = try await offPool { try f.p.create(in: f.home) }
        let dir = f.home.directory(of: "win")
        let fm = FileManager.default

        #expect(c.engine == .qemu && c.cpus == 6 && c.memory == "12G" && c.tpm && c.network && c.sshPort == nil)
        #expect(!c.balloon, "without glov-qemu a new Windows VM gets no balloon device")
        #expect(c.disk == dir + "/disk.qcow2" && c.firmwareCode == f.home.path + "/code.fd")
        #expect(c.extraDrives == [
            .init(path: f.home.path + "/win.iso", cdrom: true, bootIndex: 1), .init(path: f.home.path + "/virtio.iso", cdrom: true),
            .init(path: dir + "/secrets/unattend.cdr", removable: true),
        ])
        #expect(c.install == .init(drives: c.extraDrives.map(\.path)))
        #expect(fm.contents(atPath: dir + "/disk.qcow2")?.prefix(4) == Data([0x51, 0x46, 0x49, 0xfb]))
        #expect(try fm.attributesOfItem(atPath: dir + "/vars.fd")[.size] as? Int == 4096)

        // Secrets: private directory and files; the staging directory with the plain-text answer file is gone.
        #expect(try fm.attributesOfItem(atPath: dir)[.posixPermissions] as? Int == 0o700)
        #expect(try fm.attributesOfItem(atPath: dir + "/secrets")[.posixPermissions] as? Int == 0o700)
        #expect(try fm.attributesOfItem(atPath: dir + "/secrets/admin-password")[.posixPermissions] as? Int == 0o600)
        #expect(try fm.attributesOfItem(atPath: dir + "/secrets/unattend.cdr")[.posixPermissions] as? Int == 0o600)
        let password = try String(contentsOfFile: dir + "/secrets/admin-password", encoding: .utf8)
        #expect(password.count == 24 && password.allSatisfy { $0.isLetter || $0.isNumber })
        #expect(!fm.fileExists(atPath: dir + "/secrets/unattend"))
        #expect(!fm.fileExists(atPath: dir + "/secrets/id_ed25519"), "no key without --ssh")
        #expect(throws: HomeError.exists("win")) { try f.p.create(in: f.home) }
    }

    @Test(.enabled(if: toolsAvailable, "qemu-img is required")) func glovQEMUAtCreateTimeTurnsTheBalloonAndAutoOn() async throws {
        var f = try Fixture()
        defer { f.cleanup() }
        f.p.balloonSupported = { true }
        let c = try await offPool { try f.p.create(in: f.home) }
        #expect(c.balloon && c.balloonAuto && c.balloonTarget == nil)
    }

    @Test(.enabled(if: toolsAvailable, "qemu-img is required")) func sshAddsAKeyAndTheForward() async throws {
        var f = try Fixture()
        defer { f.cleanup() }
        f.p.sshPort = 2299
        f.p.cpus = 4
        f.p.memory = "8G"
        let c = try await offPool { try f.p.create(in: f.home) }
        let dir = f.home.directory(of: "win")
        #expect(c.sshPort == 2299 && c.cpus == 4 && c.memory == "8G")
        #expect(try FileManager.default.attributesOfItem(atPath: dir + "/secrets/id_ed25519")[.posixPermissions] as? Int == 0o600)
        #expect(try String(contentsOfFile: dir + "/secrets/id_ed25519.pub", encoding: .utf8).hasPrefix("ssh-ed25519 "))
        #expect(try c.qemuArguments().contains("user,id=net0,hostfwd=tcp:127.0.0.1:2299-:22"))
    }

    @Test func unattendFilesCarryThePasswordAndOnlyWithSSHTheKey() throws {
        let f = try Fixture()
        defer { f.cleanup() }
        let fm = FileManager.default
        let without = f.home.path + "/u1", with = f.home.path + "/u2"
        try f.p.writeUnattendFiles(to: without, password: "Secret123", publicKey: nil)
        try f.p.writeUnattendFiles(to: with, password: "Secret123", publicKey: "ssh-ed25519 AAAAC3Nza glov@win\n")

        #expect(try fm.contentsOfDirectory(atPath: without).sorted() == ["autounattend.xml", "glov-guest.exe", "setup.ps1"])
        #expect(try fm.contentsOfDirectory(atPath: with).sorted() == ["autounattend.xml", "glov-guest.exe", "setup-openssh-server.ps1", "setup.ps1"])
        #expect(try fm.attributesOfItem(atPath: without + "/autounattend.xml")[.posixPermissions] as? Int == 0o600)
        #expect(fm.contents(atPath: with + "/glov-guest.exe") == Data("MZ fake agent".utf8))

        let xml = try String(contentsOfFile: without + "/autounattend.xml", encoding: .utf8)
        #expect(xml.components(separatedBy: "<Value>Secret123</Value>").count == 3, "account and AutoLogon")
        #expect(!xml.contains("@"))

        let plain = try String(contentsOfFile: without + "/setup.ps1", encoding: .utf8)
        #expect(plain.contains(#"& "$guestServiceDir\glov-guest.exe" install"#))
        #expect(plain.contains(#"$env:windir\Panther\unattend*.xml"#), "Setup's answer file copies are deleted")
        #expect(!plain.contains("setup-openssh-server") && !plain.contains("@"))
        #expect(plain.hasSuffix(#"} catch { Write-Warning "balloon service: $_" }"# + "\nSet-Content -Path \(WindowsAssets.setupDoneMarker) -Value (Get-Date -Format o)\nStop-Transcript\n"))
        // The balloon service comes after the Guest Service, and a failure there does not stop the setup.
        #expect(plain.range(of: "glov-guest.exe\" install")!.lowerBound < plain.range(of: #"& "$guestServiceDir\blnsvr.exe" -i"#)!.lowerBound)
        #expect(plain.contains(#"$blnsvr = "${virtio}:\Balloon\w11\ARM64\blnsvr.exe""#))
        let ssh = try String(contentsOfFile: with + "/setup.ps1", encoding: .utf8)
        #expect(ssh.contains(#"& "$PSScriptRoot\setup-openssh-server.ps1" -UseWin32OpenSSH -PublicKey 'ssh-ed25519 AAAAC3Nza glov@win' -DisablePasswordAuth"#))
        #expect(ssh.hasSuffix("-DisablePasswordAuth\nSet-Content -Path C:\\glov-setup.done -Value (Get-Date -Format o)\nStop-Transcript\n"))
        // The Guest Service is installed before OpenSSH, so an OpenSSH failure leaves the VM reachable.
        #expect(ssh.range(of: "glov-guest.exe\" install")!.lowerBound < ssh.range(of: "setup-openssh-server.ps1\" -Use")!.lowerBound)
    }

    @Test func autounattendFollowsLanguageAndEdition() throws {
        let zh = try WindowsAssets.autounattend(password: "p", language: "zh-TW", edition: "Professional")
        #expect(zh.contains("<UILanguage>zh-TW</UILanguage>") && zh.contains("<InputLocale>0409:00000409;0404:00000404</InputLocale>"))
        #expect(zh.contains("<Value>Windows 11 Pro</Value>") && zh.contains("<Key>VK7JG-NPHTM-C97JM-9MPGT-3V66T</Key>"))
        let en = try WindowsAssets.autounattend(password: "a<b&c", language: "en-US", edition: "core")
        #expect(en.contains("<UserLocale>en-US</UserLocale>") && en.contains("<InputLocale>0409:00000409</InputLocale>"))
        #expect(!en.contains("0404"), "en-US has the US keyboard only")
        #expect(en.contains("<Value>Windows 11 Home</Value>"))
        #expect(en.contains("<Value>a&lt;b&amp;c</Value>"))
        let zoned = try WindowsAssets.autounattend(password: "p", language: "en-US", edition: "Professional", timeZone: "Taipei Standard Time")
        for xml in [zh, en, zoned] {
            let parser = XMLParser(data: Data(xml.utf8))
            #expect(parser.parse(), "\(parser.parserError.map { "\($0)" } ?? "")")
        }
        #expect(throws: ProvisionError.self) { try WindowsAssets.autounattend(password: "p", language: "zh-TW", edition: "Education") }
    }

    @Test func theTimeZoneIsTheChosenOneOrSetupsDefault() throws {
        #expect(WindowsProvisioner.defaultLanguage == "en-US")
        let zoned = try WindowsAssets.autounattend(password: "p", language: "en-US", edition: "Professional", timeZone: "Taipei Standard Time")
        #expect(zoned.contains("<TimeZone>Taipei Standard Time</TimeZone>"))
        for language in ["en-US", "zh-TW"] {
            #expect(!(try WindowsAssets.autounattend(password: "p", language: language, edition: "Professional")).contains("<TimeZone>"), "the language sets no zone")
        }
    }

    @Test func theHostZoneMapsToAWindowsZone() throws {
        #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: "Asia/Taipei") == "Taipei Standard Time")
        #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: "America/New_York") == "Eastern Standard Time")
        #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: "America/Toronto") == "Eastern Standard Time", "a per-territory ID")
        for id in ["Asia/Calcutta", "Asia/Kolkata"] {
            #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: id) == "India Standard Time")
        }
        #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: "Europe/Kyiv") == "FLE Standard Time")
        #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: "Nowhere/Land") == nil)
        #expect(try WindowsAssets.timeZone(requested: nil, hostIdentifier: "") == nil)
        // Every listed IANA ID maps, and only to a listed Windows ID.
        #expect(WindowsZones.byIANA.count > 400 && WindowsZones.windowsIDs.count > 100)
        for id in WindowsZones.byIANA.keys { #expect(WindowsZones.windowsID(forIANA: id) != nil) }
        for (alias, target) in WindowsZones.aliases { #expect(WindowsZones.byIANA[target] != nil, "\(alias) -> \(target)") }
    }

    @Test func aRequestedZoneOverridesTheHostAndIsChecked() throws {
        #expect(try WindowsAssets.timeZone(requested: "Tokyo Standard Time", hostIdentifier: "Asia/Taipei") == "Tokyo Standard Time")
        #expect(try WindowsAssets.timeZone(requested: "tokyo standard time", hostIdentifier: "") == "Tokyo Standard Time")
        #expect(throws: ProvisionError.self) { try WindowsAssets.timeZone(requested: "Mars Standard Time", hostIdentifier: "Asia/Taipei") }
    }

    @Test func theProvisionerWritesItsTimeZoneToTheAnswerFile() throws {
        var f = try Fixture()
        defer { f.cleanup() }
        f.p.timeZone = "Taipei Standard Time"
        try f.p.writeUnattendFiles(to: f.home.path + "/zoned", password: "x", publicKey: nil)
        #expect(try String(contentsOfFile: f.home.path + "/zoned/autounattend.xml", encoding: .utf8).contains("<TimeZone>Taipei Standard Time</TimeZone>"))
        f.p.timeZone = nil
        try f.p.writeUnattendFiles(to: f.home.path + "/unzoned", password: "x", publicKey: nil)
        #expect(try !String(contentsOfFile: f.home.path + "/unzoned/autounattend.xml", encoding: .utf8).contains("<TimeZone>"))
    }

    @Test func theVendoredOpenSSHScriptIsByteForByteTheGistRevision() {
        let digest = SHA256.hash(data: Data(WindowsAssets.openSSHScript.utf8)).map { String(format: "%02x", $0) }.joined()
        #expect(digest == "eb092ef4e39993d09712f070677a3de45f1dfe060f6ef22e5eab6815c09487af")
    }

    @Test func badInputsAreRefusedBeforeAnythingIsWritten() throws {
        var f = try Fixture()
        defer { f.cleanup() }
        f.p.edition = "Education"
        #expect(throws: ProvisionError.self) { try f.p.create(in: f.home) }
        f.p.edition = "Professional"
        FileManager.default.createFile(atPath: f.home.path + "/linux-agent", contents: Data([0x7f, 0x45, 0x4c, 0x46]))
        f.p.guestServiceBinary = f.home.path + "/linux-agent"
        #expect(throws: ProvisionError.self) { try f.p.create(in: f.home) }
        f.p.guestServiceBinary = f.home.path + "/agent.exe"
        f.p.iso = f.home.path + "/missing.iso"
        #expect(throws: ProvisionError.self) { try f.p.create(in: f.home) }
        #expect(!FileManager.default.fileExists(atPath: f.home.directory(of: "win")))
    }

    @Test func aFailingToolLeavesNoVMDirectory() async throws {
        var f = try Fixture()
        defer { f.cleanup() }
        guard Self.toolsAvailable else { return }
        f.p.run = { exe, args in exe.hasSuffix("hdiutil") ? (1, "boom") : try LinuxProvisioner.runTool(exe, args) }
        await #expect(throws: ProvisionError.self) { try await offPool { try f.p.create(in: f.home) } }
        #expect(!FileManager.default.fileExists(atPath: f.home.directory(of: "win")))
    }

    @Test func passwordsAreRandom() {
        let a = WindowsProvisioner.randomPassword(), b = WindowsProvisioner.randomPassword()
        #expect(a != b && a.count == 24)
    }

    @Test func freePortIsBindable() throws {
        let port = try WindowsProvisioner.freePort()
        #expect((1024...65535).contains(port))
    }

    @Test func missingHostToolsDeduplicatePackagesAndFormatMessages() {
        let none: (String) throws -> String = { throw GlovError.executableNotFound($0) }
        let allMissing = WindowsProvisioner.missingHostTools(
            findQEMU: { try none("qemu-system-aarch64") },
            findQEMUImg: { try none("qemu-img") },
            find: none)
        #expect(allMissing.map(\.tool) == ["qemu-system-aarch64", "qemu-img", "swtpm", "wimlib-imagex"])
        #expect(WindowsProvisioner.brewPackages(for: allMissing) == ["qemu", "swtpm", "wimlib"])
        #expect(
            WindowsProvisioner.missingHostToolsMessage(allMissing)
                == "missing host tools for Windows VMs: qemu-system-aarch64, qemu-img, swtpm, wimlib-imagex (brew install qemu swtpm wimlib)")
        #expect(
            WindowsProvisioner.missingHostToolsNote(allMissing)
                == "note: Windows VMs need qemu-system-aarch64, qemu-img, swtpm, wimlib-imagex (brew install qemu swtpm wimlib); Linux VMs need none of them")

        let partial = WindowsProvisioner.missingHostTools(
            findQEMU: { "/opt/homebrew/opt/glov-qemu/bin/qemu-system-aarch64" },
            findQEMUImg: { try none("qemu-img") },
            find: { name in
                if name == "wimlib-imagex" { return "/opt/homebrew/bin/wimlib-imagex" }
                throw GlovError.executableNotFound(name)
            })
        #expect(partial.map(\.tool) == ["qemu-img", "swtpm"])
        #expect(WindowsProvisioner.brewPackages(for: partial) == ["qemu", "swtpm"])
        #expect(WindowsProvisioner.missingHostToolsMessage(partial) == "missing host tools for Windows VMs: qemu-img, swtpm (brew install qemu swtpm)")

        let onlySwtpm = WindowsProvisioner.missingHostTools(
            findQEMU: { "/opt/homebrew/bin/qemu-system-aarch64" },
            findQEMUImg: { "/opt/homebrew/bin/qemu-img" },
            find: { name in
                if name == "swtpm" { throw GlovError.executableNotFound(name) }
                return "/opt/homebrew/bin/\(name)"
            })
        #expect(onlySwtpm.map(\.tool) == ["swtpm"])
        #expect(WindowsProvisioner.missingHostToolsMessage(onlySwtpm) == "missing host tools for Windows VMs: swtpm (brew install swtpm)")

        let onlyWimlib = WindowsProvisioner.missingHostTools(
            findQEMU: { "/opt/homebrew/bin/qemu-system-aarch64" },
            findQEMUImg: { "/opt/homebrew/bin/qemu-img" },
            find: { name in
                if name == "wimlib-imagex" { throw GlovError.executableNotFound(name) }
                return "/opt/homebrew/bin/\(name)"
            })
        #expect(onlyWimlib.map(\.tool) == ["wimlib-imagex"])
        #expect(WindowsProvisioner.missingHostToolsMessage(onlyWimlib) == "missing host tools for Windows VMs: wimlib-imagex (brew install wimlib)")

        let present = WindowsProvisioner.missingHostTools(
            findQEMU: { "/opt/homebrew/bin/qemu-system-aarch64" },
            findQEMUImg: { "/opt/homebrew/bin/qemu-img" },
            find: { "/opt/homebrew/bin/\($0)" })
        #expect(present.isEmpty)
        #expect(WindowsProvisioner.missingHostToolsMessage(present) == nil)
        #expect(WindowsProvisioner.missingHostToolsNote(present) == nil)
    }

    @Test func checkHostToolsValidatesToolsAndFirmwareBeforeDownload() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fw = dir + "/code.fd"
        FileManager.default.createFile(atPath: fw, contents: Data(count: 64))

        var firmwareLookedUp = false
        let missingError = #expect(throws: ProvisionError.self) {
            try WindowsProvisioner.checkHostTools(
                firmwareCode: nil,
                findQEMU: { "/opt/homebrew/bin/qemu-system-aarch64" },
                findQEMUImg: { "/opt/homebrew/bin/qemu-img" },
                find: { name in
                    if name == "swtpm" { throw GlovError.executableNotFound(name) }
                    return "/opt/homebrew/bin/\(name)"
                },
                defaultFirmware: { firmwareLookedUp = true; return fw })
        }
        #expect(missingError?.description == "missing host tools for Windows VMs: swtpm (brew install swtpm)")
        #expect(!firmwareLookedUp, "missing host tools fails before probing firmware")

        let allPresent: (String?) throws -> String = { requested in
            try WindowsProvisioner.checkHostTools(
                firmwareCode: requested,
                findQEMU: { "/opt/homebrew/bin/qemu-system-aarch64" },
                findQEMUImg: { "/opt/homebrew/bin/qemu-img" },
                find: { "/opt/homebrew/bin/\($0)" },
                defaultFirmware: { firmwareLookedUp = true; return fw })
        }
        #expect(try allPresent(fw) == fw)
        #expect(!firmwareLookedUp)
        let missingFWError = #expect(throws: ProvisionError.self) {
            _ = try allPresent(dir + "/missing.fd")
        }
        #expect(missingFWError?.description == "UEFI firmware not found: \(dir)/missing.fd")
        #expect(try allPresent(nil) == fw)
        #expect(firmwareLookedUp)

        let defaultFWError = #expect(throws: ProvisionError.self) {
            try WindowsProvisioner.checkHostTools(
                firmwareCode: nil,
                findQEMU: { "/opt/homebrew/bin/qemu-system-aarch64" },
                findQEMUImg: { "/opt/homebrew/bin/qemu-img" },
                find: { "/opt/homebrew/bin/\($0)" },
                defaultFirmware: { throw ProvisionError("UEFI firmware not found at /missing/code.fd; pass --firmware-code") })
        }
        #expect(defaultFWError?.description == "UEFI firmware not found at /missing/code.fd; pass --firmware-code")
    }

    @Test func checkGuestServiceBinaryRequiresWindowsPEHeader() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let exe = dir + "/glov-guest.exe"
        let elf = dir + "/glov-guest"
        FileManager.default.createFile(atPath: exe, contents: Data("MZ fake agent".utf8))
        FileManager.default.createFile(atPath: elf, contents: Data([0x7f, 0x45, 0x4c, 0x46]))

        try WindowsProvisioner.checkGuestServiceBinary(exe)
        #expect(throws: ProvisionError.self) { try WindowsProvisioner.checkGuestServiceBinary(elf) }
        #expect(throws: ProvisionError.self) { try WindowsProvisioner.checkGuestServiceBinary(dir + "/missing.exe") }
    }
}
