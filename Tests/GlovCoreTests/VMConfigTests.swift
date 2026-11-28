import Foundation
import Testing

@testable import GlovCore

@Suite struct VMConfigTests {
    func windows() -> VMConfig {
        VMConfig(
            name: "win11", directory: "/vm/win11", cpus: 6, memory: "16G", disk: "/vm/win11/disk.qcow2",
            firmwareCode: "/fw/code.fd", tpm: true, sshPort: 2222)
    }

    @Test func windowsGuestArguments() throws {
        let a = try windows().qemuArguments()
        let expected = [
            "-name", "glov-win11",
            "-machine", "virt,gic-version=3,highmem=on",
            "-accel", "hvf,kernel-irqchip=on",
            "-cpu", "host", "-smp", "6", "-m", "16G",
            "-nodefaults", "-serial", "none",
            "-drive", "if=pflash,format=raw,readonly=on,file=/fw/code.fd",
            "-drive", "if=pflash,format=raw,file=/vm/win11/vars.fd",
            "-chardev", "socket,id=chrtpm,path=/vm/win11/tpm/swtpm.sock",
            "-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
            "-device", "tpm-tis-device,tpmdev=tpm0,ppi=off",
            "-drive", "if=none,id=disk0,node-name=sys0,file=/vm/win11/disk.qcow2,format=qcow2,cache=writeback,discard=unmap",
            "-device", "nvme,drive=disk0,serial=glov0,bootindex=0",
            "-netdev", "user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22",
            "-device", "virtio-net-pci,netdev=net0",
            "-device", "virtio-serial-pci,id=vser0",
            "-chardev", "socket,id=guest0,path=/vm/win11/guest.sock,server=on,wait=off",
            "-device", "virtserialport,bus=vser0.0,chardev=guest0,name=org.glov.guest.0",
            "-device", "qemu-xhci,id=xhci", "-device", "usb-kbd", "-device", "usb-tablet",
            "-device", "ramfb",
            "-display", "none",
            "-qmp", "unix:/vm/win11/qmp.sock,server,wait=off",
            "-pidfile", "/vm/win11/qemu.pid",
        ]
        #expect(a == expected)
    }

    @Test func tpmDiskAndNetworkAreOptional() throws {
        let c = VMConfig(name: "t", directory: "/v", disk: nil, firmwareCode: "/c.fd", tpm: false, sshPort: nil)
        let a = try c.qemuArguments()
        #expect(!a.contains { $0.contains("tpm") || $0.contains("nvme") || $0.contains("netdev") || $0.contains("virtio-net") })
        #expect(a.contains("ramfb"))
    }

    @Test func portForwardsJoinTheSSHForwardOnOneNIC() throws {
        var c = windows()
        c.portForwards = [.init(host: 13389, guest: 3389), .init(host: 8080, guest: 80)]
        let a = try c.qemuArguments()
        #expect(a.filter { $0 == "-netdev" }.count == 1)
        #expect(a.contains("user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:13389-:3389,hostfwd=tcp:127.0.0.1:8080-:80"))
    }

    @Test func portForwardsAloneAttachANIC() throws {
        let c = VMConfig(name: "t", directory: "/v", firmwareCode: "/c.fd", tpm: false, portForwards: [.init(host: 8080, guest: 80)])
        let a = try c.qemuArguments()
        #expect(a.contains("user,id=net0,hostfwd=tcp:127.0.0.1:8080-:80"))
        #expect(a.contains("virtio-net-pci,netdev=net0"))
    }

    @Test func displayIsNeverAnythingButNone() throws {
        let a = try windows().qemuArguments()
        #expect(a.filter { $0 == "-display" }.count == 1)
        #expect(a[a.firstIndex(of: "-display")! + 1] == "none")
    }

    @Test func extraDrivesAttachOverUsb() throws {
        var c = windows()
        c.extraDrives = [
            .init(path: "/m/win.iso", removable: true, bootIndex: 1),
            .init(path: "/m/virtio.iso", cdrom: true),
        ]
        let a = try c.qemuArguments()
        #expect(a.contains("if=none,id=extra0,file=/m/win.iso,format=raw,readonly=on"))
        #expect(a.contains("usb-storage,id=extra0-usb,drive=extra0,removable=on,bootindex=1"))
        #expect(a.contains("if=none,id=extra1,file=/m/virtio.iso,format=raw,readonly=on,media=cdrom"))
        #expect(a.contains("usb-storage,id=extra1-usb,drive=extra1"))
    }

    @Test func networkAttachesANICWithoutForwards() throws {
        var c = VMConfig(name: "t", directory: "/v", firmwareCode: "/c.fd", tpm: false)
        c.network = true
        let a = try c.qemuArguments()
        #expect(a.contains("user,id=net0") && a.contains("virtio-net-pci,netdev=net0"))
    }

    @Test func finishingAnInstallDropsOnlyItsDrives() throws {
        var c = windows()
        c.extraDrives = [.init(path: "/m/win.iso", cdrom: true, bootIndex: 1), .init(path: "/m/virtio.iso", cdrom: true),
            .init(path: "/vm/win11/secrets/unattend.cdr", removable: true), .init(path: "/m/mine.iso", cdrom: true)]
        c.install = .init(drives: ["/m/win.iso", "/m/virtio.iso", "/vm/win11/secrets/unattend.cdr"])
        #expect(c.installDriveIndices == [0, 1, 2])
        let done = c.finishingInstall()
        #expect(done.install == nil && done.extraDrives == [.init(path: "/m/mine.iso", cdrom: true)])
        #expect(done.installDriveIndices.isEmpty)
    }

    @Test func installStateRoundTripsWithAReadableDate() throws {
        var c = windows()
        c.network = true
        c.install = .init(drives: ["/m/win.iso"], started: Date(timeIntervalSince1970: 1_793_000_000), failure: "timed out")
        let data = try JSONEncoder().encode(c)
        #expect(String(decoding: data, as: UTF8.self).contains(#""started":"2026-10-26T07:33:20Z""#))
        #expect(try JSONDecoder().decode(VMConfig.self, from: data) == c)
    }

    @Test func commasInPathsAreEscaped() throws {
        var c = windows()
        c.disk = "/vm/a,b.qcow2"
        #expect(try c.qemuArguments().contains { $0.contains("file=/vm/a,,b.qcow2,format=qcow2") })
    }

    @Test func overlongSocketPathIsRejected() {
        let c = VMConfig(name: "t", directory: "/tmp/" + String(repeating: "d", count: 100), firmwareCode: "/c.fd", tpm: false)
        #expect(throws: GlovError.self) { try c.qemuArguments() }
    }

    @Test func guestServiceChannelIsOptional() throws {
        var c = windows()
        c.guestService = false
        #expect(!(try c.qemuArguments()).contains { $0.contains("virtio-serial") || $0.contains("guest") })
    }

    @Test func overlongGuestSocketPathIsRejectedEvenWhenQMPFits() {
        // "qmp.sock" and "guest.sock" differ in length: 93 + "/guest.sock" is 104 bytes, "/qmp.sock" is 102.
        let c = VMConfig(name: "t", directory: "/tmp/" + String(repeating: "d", count: 88), firmwareCode: "/c.fd", tpm: false)
        #expect(c.qmpSocketPath.utf8.count <= 103 && c.guestSocketPath.utf8.count > 103)
        #expect(throws: GlovError.self) { try c.qemuArguments() }
        var off = c
        off.guestService = false
        #expect(throws: Never.self) { try off.qemuArguments() }
    }

    @Test func guestServiceDefaultsOnWithAnAllowlistNextToTheVMs() {
        let c = windows()
        #expect(c.guestService && c.agentControl)
        #expect(c.hostPathAllowlist == ["/vm/win11/shared", "/vm/scratch"])
    }

    @Test func aVMJsonWithoutTheGuestServiceKeysStillDecodes() throws {
        let old = """
            {"name":"win11","directory":"/vm/win11","cpus":6,"memory":"16G","disk":"/vm/win11/disk.qcow2",
             "firmwareCode":"/fw/code.fd","firmwareVars":"/vm/win11/vars.fd","tpm":true,"sshPort":2222,"extraDrives":[]}
            """
        let c = try JSONDecoder().decode(VMConfig.self, from: Data(old.utf8))
        #expect(c == windows())
        #expect(!c.network && c.install == nil)
        #expect(!c.balloon, "a VM made before the balloon existed keeps its devices")
        #expect(c.uuid == nil && c.macAddress == nil && c.diskSerial == nil)
    }

    @Test func perVMIdentifiersPassUUIDSMBIOSMACAndDiskSerialToQEMU() throws {
        var c = windows()
        let uuid = "12345678-1234-4123-8123-123456789abc"
        let mac = "02:aa:bb:cc:dd:ee"
        let serial = "glov-0123456789ab"
        c.uuid = uuid
        c.macAddress = mac
        c.diskSerial = serial
        let a = try c.qemuArguments()
        let uuidIndex = try #require(a.firstIndex(of: "-uuid"))
        #expect(a[uuidIndex + 1] == uuid)
        let smbiosIndex = try #require(a.firstIndex(of: "-smbios"))
        #expect(a[smbiosIndex + 1] == "type=1,serial=\(uuid)")
        #expect(a.contains("nvme,drive=disk0,serial=\(serial),bootindex=0"))
        #expect(a.contains("virtio-net-pci,netdev=net0,mac=\(mac)"))
        #expect(try JSONDecoder().decode(VMConfig.self, from: JSONEncoder().encode(c)) == c)
    }

    @Test func randomDiskSerialFitsNVMeLimitAndVaries() {
        let a = VMConfig.randomDiskSerial(), b = VMConfig.randomDiskSerial()
        #expect(a != b)
        for s in [a, b] {
            #expect(s.hasPrefix("glov-") && s.count == 17 && s.utf8.count <= 20)
            let hex = s.dropFirst(5)
            #expect(hex.count == 12 && hex.allSatisfy { ("0"..."9").contains($0) || ("a"..."f").contains($0) })
        }
    }

    @Test func theBalloonDeviceNeedsBothTheSwitchAndAQEMUThatFreesMemory() throws {
        var c = windows()
        let device = ["-device", "virtio-balloon-pci,id=balloon0"]
        func hasDevice(_ a: [String]) -> Bool { a.indices.dropLast().contains { Array(a[$0...$0 + 1]) == device } }
        #expect(!hasDevice(try c.qemuArguments(balloonSupported: true)), "off by default")
        c.balloon = true
        #expect(!hasDevice(try c.qemuArguments()), "Homebrew's qemu would keep the memory anyway")
        let a = try c.qemuArguments(balloonSupported: true)
        #expect(hasDevice(a))
        #expect(a.firstIndex(of: "virtio-balloon-pci,id=balloon0")! < a.firstIndex(of: "qemu-xhci,id=xhci")!)
        #expect(try JSONDecoder().decode(VMConfig.self, from: JSONEncoder().encode(c)) == c)
    }

    @Test func onlyGlovQEMUCountsAsABalloonBuild() {
        #expect(VirtualMachine.isBalloonBuild(version: "QEMU emulator version 11.1.3 (glov-balloon)\nCopyright (c) 2003-2026 Fabrice Bellard"))
        #expect(!VirtualMachine.isBalloonBuild(version: "QEMU emulator version 11.1.3\nCopyright (c) 2003-2026 Fabrice Bellard"))
        #expect(!VirtualMachine.isBalloonBuild(version: ""))
    }

    @Test func glovQEMUIsPreferredOverQEMUOnPath() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("glov-qemu-\(UUID().uuidString)").path
        defer { try? FileManager.default.removeItem(atPath: dir) }
        try FileManager.default.createDirectory(atPath: dir + "/bin", withIntermediateDirectories: true)
        let qemu = dir + "/bin/qemu-system-aarch64"
        FileManager.default.createFile(atPath: qemu, contents: Data("#!/bin/sh\necho 'QEMU emulator version 11.1.3 (glov-balloon)'\n".utf8),
            attributes: [.posixPermissions: 0o755])
        #expect(try VirtualMachine.findQEMU(prefixes: ["/nonexistent/glov-qemu", dir]) == qemu)
        #expect(VirtualMachine.supportsBalloon(qemu: qemu))
        #expect(!VirtualMachine.supportsBalloon(qemu: dir + "/bin/missing"))
        let qemuImg = dir + "/bin/qemu-img"
        FileManager.default.createFile(atPath: qemuImg, contents: Data("#!/bin/sh\nexit 0\n".utf8), attributes: [.posixPermissions: 0o755])
        #expect(try VirtualMachine.findQEMUImg(prefixes: ["/nonexistent/glov-qemu", dir]) == qemuImg)
    }

    @Test func theVersionCacheAsksAgainWhenTheBinaryBehindThePathChanges() {
        final class Box: @unchecked Sendable { var target = "/Cellar/glov-qemu/11.1.3/bin/qemu@1"; var asked = 0 }
        let box = Box()
        let cache = QEMUVersionCache(identity: { _ in box.target })
        let ask = { (_: String) -> String? in box.asked += 1; return "version \(box.asked)" }
        let link = "/opt/homebrew/opt/glov-qemu/bin/qemu-system-aarch64"
        #expect(cache.value(for: link, compute: ask) == "version 1")
        #expect(cache.value(for: link, compute: ask) == "version 1")
        box.target = "/Cellar/glov-qemu/11.2.0/bin/qemu@2"  // brew upgrade repointed the link
        #expect(cache.value(for: link, compute: ask) == "version 2")
        #expect(box.asked == 2)
    }

    @Test func aBinarysIdentityFollowsSymlinksAndItsModificationDate() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("qemu-id-\(UUID().uuidString)").path
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let fm = FileManager.default
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
        fm.createFile(atPath: dir + "/a", contents: Data())
        fm.createFile(atPath: dir + "/b", contents: Data())
        try fm.createSymbolicLink(atPath: dir + "/link", withDestinationPath: dir + "/a")
        let first = QEMUVersionCache.binaryIdentity(dir + "/link")
        #expect(first == QEMUVersionCache.binaryIdentity(dir + "/a"))
        try fm.setAttributes([.modificationDate: Date(timeIntervalSince1970: 1_000_000)], ofItemAtPath: dir + "/a")
        #expect(QEMUVersionCache.binaryIdentity(dir + "/link") != first)
        try fm.removeItem(atPath: dir + "/link")
        try fm.createSymbolicLink(atPath: dir + "/link", withDestinationPath: dir + "/b")
        #expect(QEMUVersionCache.binaryIdentity(dir + "/link") == QEMUVersionCache.binaryIdentity(dir + "/b"))
    }

    @Test func portForwardsRoundTripThroughJSON() throws {
        var c = windows()
        c.portForwards = [.init(host: 8080, guest: 80)]
        #expect(try JSONDecoder().decode(VMConfig.self, from: JSONEncoder().encode(c)) == c)
    }

    @Test func roundTripsThroughJSON() throws {
        var config = windows()
        config.dns = ["1.1.1.1", "2606:4700:4700::1111"]
        let data = try JSONEncoder().encode(config)
        #expect(try JSONDecoder().decode(VMConfig.self, from: data) == config)
    }

    @Test func parseDNSValidatesCanonicalizesDeduplicatesAndHandlesAuto() throws {
        #expect(VMConfig.canonicalDNSAddress(" 1.1.1.1 ") == "1.1.1.1")
        #expect(VMConfig.canonicalDNSAddress("[2606:4700:4700:0:0:0:0:1111]") == "2606:4700:4700::1111")
        #expect(VMConfig.canonicalDNSAddress("dns.google") == nil)
        #expect(VMConfig.canonicalDNSAddress("1.1.1.1;calc") == nil)

        #expect(try VMConfig.parseDNS(["1.1.1.1, 8.8.8.8", "1.1.1.1", "[2606:4700:4700::1111]"], allowAuto: false) == ["1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"])
        #expect(try VMConfig.parseDNS(["AUTO"], allowAuto: true) == [])
        #expect(throws: GlovError.self) { try VMConfig.parseDNS(["auto"], allowAuto: false) }
        #expect(throws: GlovError.self) { try VMConfig.parseDNS(["auto", "1.1.1.1"], allowAuto: true) }
        #expect(throws: GlovError.self) { try VMConfig.parseDNS(["1.1.1.1,"], allowAuto: true) }
        #expect(throws: GlovError.self) { try VMConfig.parseDNS(["not-an-ip"], allowAuto: false) }
    }
}
