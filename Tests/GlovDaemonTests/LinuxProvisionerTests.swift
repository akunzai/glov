import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

@Suite struct LinuxProvisionerTests {
    @Test func buildsDiskKeySeedAndConfigFromARawImage() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let image = home.path + "/cloud.raw"
        FileManager.default.createFile(atPath: image, contents: Data(repeating: 7, count: 4096))
        let agent = home.path + "/glov-guest"
        FileManager.default.createFile(atPath: agent, contents: Data("ELF".utf8), attributes: [.posixPermissions: 0o755])

        let c = try await offPool {
            try LinuxProvisioner(name: "ubuntu", image: image, cpus: 4, memory: "8G", diskSize: 1 << 20, guestServiceBinary: agent).create(in: home)
        }
        let dir = home.directory(of: "ubuntu")
        #expect(c.engine == .vz && c.cpus == 4 && c.memory == "8G" && !c.tpm && c.sshPort == nil)
        #expect(c.disk == dir + "/disk.img")
        #expect(c.extraDrives.map(\.path) == [dir + "/seed.iso"])
        #expect(c.macAddress.map { VZMACPattern.matches($0) } == true)

        let fm = FileManager.default
        #expect(try fm.attributesOfItem(atPath: dir + "/disk.img")[.size] as? Int == 1 << 20)
        #expect(fm.contents(atPath: dir + "/disk.img")?.prefix(4096) == Data(repeating: 7, count: 4096))
        #expect(try fm.attributesOfItem(atPath: dir + "/secrets")[.posixPermissions] as? Int == 0o700)
        #expect(try fm.attributesOfItem(atPath: dir + "/secrets/id_ed25519")[.posixPermissions] as? Int == 0o600)
        #expect(try String(contentsOfFile: dir + "/secrets/id_ed25519.pub", encoding: .utf8).hasPrefix("ssh-ed25519 "))
        #expect(fm.fileExists(atPath: dir + "/seed.iso"))
        #expect(!fm.fileExists(atPath: dir + "/seed"))  // the staging directory is gone
        #expect(throws: HomeError.exists("ubuntu")) { try LinuxProvisioner(name: "ubuntu", image: image).create(in: home) }
    }

    @Test func aQcow2ImageIsConvertedWithGlovImage() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let image = home.path + "/cloud.qcow2"
        FileManager.default.createFile(atPath: image, contents: Data([0x51, 0x46, 0x49, 0xfb]) + Data(count: 60))
        var p = LinuxProvisioner(name: "u", image: image, diskSize: 8192)
        p.imageTool = { "/fake/glov-image" }
        let calls = Calls()
        p.log = { calls.add("log", [$0]) }
        p.run = { exe, args in
            calls.add(URL(fileURLWithPath: exe).lastPathComponent, args)
            FileManager.default.createFile(atPath: args.last!, contents: Data(count: 1024))
            return (0, "")
        }
        let dir = home.directory(of: "u")
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        try p.makeDisk(at: dir + "/disk.img")
        let convert = try #require(calls.list.first { $0.0 == "glov-image" })
        #expect(convert.1 == ["convert", image, dir + "/disk.img"])
        #expect(calls.list.first?.1 == ["converting cloud.qcow2 to a raw disk"], "said before the conversion starts")
        #expect(try FileManager.default.attributesOfItem(atPath: dir + "/disk.img")[.size] as? Int == 8192)
    }

    @Test func aMissingImageOrGuestServiceIsRefusedBeforeAnythingIsWritten() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        #expect(throws: ProvisionError.self) { try LinuxProvisioner(name: "u", image: home.path + "/none.img").create(in: home) }
        FileManager.default.createFile(atPath: home.path + "/x.img", contents: Data(count: 16))
        #expect(throws: ProvisionError.self) {
            try LinuxProvisioner(name: "u", image: home.path + "/x.img", guestServiceBinary: home.path + "/missing-agent").create(in: home)
        }
        #expect(!FileManager.default.fileExists(atPath: home.directory(of: "u")))
    }

    @Test func randomMACsAreLocallyAdministeredUnicast() {
        for _ in 0..<50 {
            let mac = LinuxProvisioner.randomMACAddress()
            #expect(VZMACPattern.matches(mac))
            let first = UInt8(mac.prefix(2), radix: 16)!
            #expect(first & 0x03 == 0x02)
        }
    }
}

private enum VZMACPattern {
    static func matches(_ s: String) -> Bool { s.wholeMatch(of: /[0-9a-f]{2}(:[0-9a-f]{2}){5}/) != nil }
}

private final class Calls: @unchecked Sendable {
    private let lock = NSLock()
    private var _list: [(String, [String])] = []
    var list: [(String, [String])] { lock.withLock { _list } }
    func add(_ exe: String, _ args: [String]) { lock.withLock { _list.append((exe, args)) } }
}
