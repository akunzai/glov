import Foundation
import Testing
import Virtualization

@testable import GlovCore

@Suite struct EngineConfigTests {
    @Test func aVMJsonWithoutAnEngineIsQEMU() throws {
        let old = """
            {"name":"win11","directory":"/vm/win11","cpus":6,"memory":"16G","firmwareCode":"/fw/code.fd",
             "firmwareVars":"/vm/win11/vars.fd","tpm":true,"extraDrives":[]}
            """
        let c = try JSONDecoder().decode(VMConfig.self, from: Data(old.utf8))
        #expect(c.engine == .qemu)
        #expect(c.macAddress == nil)
    }

    @Test func aVZConfigRoundTripsWithItsEngineAndMAC() throws {
        let c = VMConfig(
            name: "ubuntu", directory: "/vm/ubuntu", engine: .vz, disk: "/vm/ubuntu/disk.img", firmwareCode: "", tpm: false,
            extraDrives: [.init(path: "/vm/ubuntu/seed.iso", cdrom: true)], macAddress: "02:00:00:00:00:01")
        let json = try JSONEncoder().encode(c)
        #expect(String(decoding: json, as: UTF8.self).contains("\"engine\":\"vz\""))
        #expect(try JSONDecoder().decode(VMConfig.self, from: json) == c)
    }

    @Test func anOlderVMJsonIsNotIsolatedAndAnIsolatedOneKeepsItsAllowlist() throws {
        let old = """
            {"name":"ubuntu","directory":"/vm/ubuntu","engine":"vz","cpus":2,"memory":"4G","firmwareCode":"",
             "firmwareVars":"/vm/ubuntu/efi-vars.fd","tpm":false,"extraDrives":[]}
            """
        let c = try JSONDecoder().decode(VMConfig.self, from: Data(old.utf8))
        #expect(!c.isolated && c.egress == nil)
        let isolated = """
            {"name":"ubuntu","directory":"/vm/ubuntu","engine":"vz","cpus":2,"memory":"4G","firmwareCode":"",
             "firmwareVars":"/vm/ubuntu/efi-vars.fd","tpm":false,"extraDrives":[],
             "isolated":true,"egress":{"allow":["api.example.com","*.ubuntu.com"]}}
            """
        let i = try JSONDecoder().decode(VMConfig.self, from: Data(isolated.utf8))
        #expect(i.isolated && i.egress?.allow == ["api.example.com", "*.ubuntu.com"])
        #expect(try JSONDecoder().decode(VMConfig.self, from: JSONEncoder().encode(i)) == i)
    }

    @Test func memorySizesReadLikeQEMU() {
        #expect(VMConfig.bytes("4G") == 4 << 30)
        #expect(VMConfig.bytes("512m") == 512 << 20)
        #expect(VMConfig.bytes("2048") == 2048 << 20)
        #expect(VMConfig.bytes("1T") == 1 << 40)
        #expect(VMConfig.bytes("") == nil)
        #expect(VMConfig.bytes("lots") == nil)
        #expect(VMConfig.bytes("0G") == nil)
        #expect(VMConfig.bytes("99999999999999999999G") == nil)
    }

    @Test func unsupportedToolsNameTheEngine() {
        #expect(VMConfig.Engine.vz.unsupported("screenshot").description == "screenshot: not supported for VZ (Linux) VMs yet")
    }
}

@Suite struct VZConfigurationTests {
    func config(_ dir: String, agent: Bool = true, mac: String? = "02:11:22:33:44:55") -> VMConfig {
        VMConfig(
            name: "ubuntu", directory: dir, engine: .vz, cpus: 3, memory: "2G", disk: dir + "/disk.img", firmwareCode: "",
            firmwareVars: dir + "/efi-vars.fd", tpm: false, extraDrives: [.init(path: dir + "/seed.iso", cdrom: true)],
            guestService: agent, macAddress: mac)
    }

    func withVMDir(_ body: (String) throws -> Void) throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        for f in ["disk.img", "seed.iso"] { FileManager.default.createFile(atPath: dir + "/" + f, contents: Data(count: 1 << 20)) }
        try body(dir)
    }

    @Test func machineIdentifierAndVariableStoreAreCreatedOnceAndKept() throws {
        try withVMDir { dir in
            let c = config(dir)
            try VZConfiguration.prepareState(c)
            let id = try Data(contentsOf: URL(fileURLWithPath: c.machineIdentifierPath))
            #expect(VZGenericMachineIdentifier(dataRepresentation: id) != nil)
            #expect(FileManager.default.fileExists(atPath: c.firmwareVars))
            try VZConfiguration.prepareState(c)
            #expect(try Data(contentsOf: URL(fileURLWithPath: c.machineIdentifierPath)) == id)
        }
    }

    @Test func buildsTheDeviceListFromTheConfig() throws {
        try withVMDir { dir in
            let c = config(dir)
            try VZConfiguration.prepareState(c)
            let v = try VZConfiguration.make(c)
            #expect(v.cpuCount == 3)
            #expect(v.memorySize == 2 << 30)
            #expect(v.platform is VZGenericPlatformConfiguration)
            let id = try Data(contentsOf: URL(fileURLWithPath: c.machineIdentifierPath))
            #expect((v.platform as? VZGenericPlatformConfiguration)?.machineIdentifier.dataRepresentation == id)
            #expect((v.bootLoader as? VZEFIBootLoader)?.variableStore?.url.path == c.firmwareVars)

            let disks = v.storageDevices.compactMap { $0 as? VZVirtioBlockDeviceConfiguration }
            #expect(disks.count == 2)
            let attachments = disks.compactMap { $0.attachment as? VZDiskImageStorageDeviceAttachment }
            #expect(attachments.map(\.url.path) == [dir + "/disk.img", dir + "/seed.iso"])
            #expect(attachments.map(\.isReadOnly) == [false, true])

            let nic = try #require(v.networkDevices.first as? VZVirtioNetworkDeviceConfiguration)
            #expect(v.networkDevices.count == 1)
            #expect(nic.attachment is VZNATNetworkDeviceAttachment)
            #expect(nic.macAddress.string == "02:11:22:33:44:55")

            #expect(v.serialPorts.count == 1 && v.serialPorts.first is VZVirtioConsoleDeviceSerialPortConfiguration)
            #expect(v.serialPorts.first?.attachment is VZFileHandleSerialPortAttachment)
            #expect(FileManager.default.fileExists(atPath: c.consoleLogPath))

            #expect(v.socketDevices.count == 1 && v.socketDevices.first is VZVirtioSocketDeviceConfiguration)
            #expect(v.entropyDevices.count == 1)
            #expect(v.memoryBalloonDevices.first is VZVirtioTraditionalMemoryBalloonDeviceConfiguration)

            let gpu = try #require(v.graphicsDevices.first as? VZVirtioGraphicsDeviceConfiguration)
            #expect(gpu.scanouts.map { [$0.widthInPixels, $0.heightInPixels] } == [[1280, 800]])
            #expect(v.keyboards.first is VZUSBKeyboardConfiguration)
            #expect(v.pointingDevices.first is VZUSBScreenCoordinatePointingDeviceConfiguration)
        }
    }

    @Test func eachShareBecomesAVirtiofsDeviceTaggedByItsNameAndAROShareIsReadOnly() throws {
        try withVMDir { dir in
            try FileManager.default.createDirectory(atPath: dir + "/a", withIntermediateDirectories: true)
            try FileManager.default.createDirectory(atPath: dir + "/b", withIntermediateDirectories: true)
            var c = config(dir)
            c.shares = [
                .init(name: "proj", hostPath: dir + "/a", guestPath: "/mnt/proj", mode: .rw),
                .init(name: "ref.v1", hostPath: dir + "/b", guestPath: "/srv/ref", mode: .ro),
            ]
            try VZConfiguration.prepareState(c)
            let devices = try #require(try VZConfiguration.make(c).directorySharingDevices as? [VZVirtioFileSystemDeviceConfiguration])
            #expect(devices.map(\.tag) == ["proj", "ref.v1"])
            let shared = try devices.map { try #require(($0.share as? VZSingleDirectoryShare)?.directory) }
            #expect(shared.map(\.url.path) == [dir + "/a", dir + "/b"])
            #expect(shared.map(\.isReadOnly) == [false, true])

            c.shares[0].hostPath = dir + "/gone"
            #expect(throws: GlovError.self) { try VZConfiguration.make(c) }
            c.shares[0] = .init(name: String(repeating: "x", count: 37), hostPath: dir + "/a", guestPath: "/mnt/x")
            #expect(throws: GlovError.self) { try VZConfiguration.make(c) }
        }
    }

    @Test func anIsolatedVMHasNoNetworkDeviceButKeepsItsSocketDevice() throws {
        try withVMDir { dir in
            var c = config(dir, agent: false)
            c.isolated = true
            try VZConfiguration.prepareState(c)
            let v = try VZConfiguration.make(c)
            #expect(v.networkDevices.isEmpty)
            #expect(v.socketDevices.count == 1)
        }
    }

    @Test func noSocketDeviceWithoutTheGuestServiceAndARandomMACWithoutOne() throws {
        try withVMDir { dir in
            let c = config(dir, agent: false, mac: nil)
            try VZConfiguration.prepareState(c)
            let v = try VZConfiguration.make(c)
            #expect(v.socketDevices.isEmpty)
            #expect((v.networkDevices.first as? VZVirtioNetworkDeviceConfiguration)?.macAddress.isLocallyAdministeredAddress == true)
        }
    }

    @Test func badMemoryOrMACIsRefusedBeforeVZSeesIt() throws {
        try withVMDir { dir in
            var c = config(dir)
            try VZConfiguration.prepareState(c)
            c.memory = "lots"
            #expect(throws: GlovError.self) { try VZConfiguration.make(c) }
            c.memory = "2G"
            c.macAddress = "not-a-mac"
            #expect(throws: GlovError.self) { try VZConfiguration.make(c) }
        }
    }

    @Test func startingWithoutTheEntitlementSaysHowToSign() async throws {
        try await confirmation(expectedCount: VZConfiguration.hasVirtualizationEntitlement ? 0 : 1) { refused in
            guard !VZConfiguration.hasVirtualizationEntitlement else { return }  // a signed runner would really boot; covered manually
            let dir = try shortTempDir()
            defer { try? FileManager.default.removeItem(atPath: dir) }
            let activity = ActivityAssertion(begin: { NSObject() }, end: { _ in })
            let vm = VZMachine(config: config(dir), activity: activity)
            do { try await vm.start() } catch let e as GlovError {
                #expect(e == .missingVirtualizationEntitlement)
                #expect(e.description.contains("tools/linux-vm/sign-daemon.sh"))
                refused()
            }
            #expect(await vm.isRunning == false)
            #expect(!activity.isHeld)
        }
    }
}

@Suite struct ActivityAssertionTests {
    final class Log: @unchecked Sendable {
        let lock = NSLock()
        var events: [String] = []
        func add(_ e: String) { lock.withLock { events.append(e) } }
    }

    @Test func holdsOneActivityWhileAnyHolderRemains() {
        let log = Log()
        let a = ActivityAssertion(begin: { log.add("begin"); return NSObject() }, end: { _ in log.add("end") })
        a.acquire()
        a.acquire()
        #expect(a.isHeld)
        a.release()
        #expect(a.isHeld)
        a.release()
        #expect(!a.isHeld)
        a.release()  // an extra release is harmless
        a.acquire()
        #expect(log.events == ["begin", "end", "begin"])
    }
}

@Suite struct ConsoleLogTests {
    /// A restart that ends a hang must not wipe what the hung boot wrote; an empty log (a boot that wrote nothing) keeps the one before.
    @Test func aStartKeepsTheLastBootsConsoleLog() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).path
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let config = VMConfig(name: "lin", directory: dir, engine: .vz, firmwareCode: "", tpm: false)
        func read(_ path: String) -> String? { try? String(contentsOfFile: path, encoding: .utf8) }

        VZConfiguration.keepPreviousConsoleLog(config)  // nothing yet
        #expect(read(config.previousConsoleLogPath) == nil)
        try "first boot\n".write(toFile: config.consoleLogPath, atomically: true, encoding: .utf8)
        VZConfiguration.keepPreviousConsoleLog(config)
        #expect(read(config.previousConsoleLogPath) == "first boot\n")
        #expect(read(config.consoleLogPath) == nil)
        try "second boot\n".write(toFile: config.consoleLogPath, atomically: true, encoding: .utf8)
        VZConfiguration.keepPreviousConsoleLog(config)
        #expect(read(config.previousConsoleLogPath) == "second boot\n")
        FileManager.default.createFile(atPath: config.consoleLogPath, contents: nil)
        VZConfiguration.keepPreviousConsoleLog(config)
        #expect(read(config.previousConsoleLogPath) == "second boot\n")
    }
}
