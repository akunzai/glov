import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct VMConfigureTests {
    @Test func vzConfigurationUpdatesCPUsMemoryAndGrowsRawDiskWithoutShrinking() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let dir = home.directory(of: "lin")
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let disk = dir + "/disk.img"
        FileManager.default.createFile(atPath: disk, contents: nil)
        let handle = try FileHandle(forWritingTo: URL(fileURLWithPath: disk))
        try handle.truncate(atOffset: 32 << 30)
        try handle.close()

        let config = VMConfig(name: "lin", directory: dir, engine: .vz, cpus: 2, memory: "4G", disk: disk, firmwareCode: "", tpm: false)
        try home.create(config)

        let configurator = VMConfigurator()
        #expect(throws: ProvisionError.self) { try configurator.configure(config) }
        #expect(throws: ProvisionError.self) { try configurator.configure(config, cpus: 0) }
        #expect(throws: ProvisionError.self) { try configurator.configure(config, memory: "bad") }
        #expect(throws: ProvisionError.self) { try configurator.configure(config, diskSize: "16G") }

        let updated = try configurator.configure(config, cpus: 6, memory: "8G", diskSize: "64G")
        #expect(updated.cpus == 6)
        #expect(updated.memory == "8G")
        let size = try FileManager.default.attributesOfItem(atPath: disk)[.size] as? UInt64
        #expect(size == (64 << 30))
    }

    @Test func qemuConfigurationResizesQcow2RecordsPartitionMarkerAndRefusesSnapshotsOrShrinking() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let dir = home.directory(of: "win")
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let fw = home.path + "/code.fd"
        let disk = dir + "/disk.qcow2"
        FileManager.default.createFile(atPath: fw, contents: Data(count: 64))
        FileManager.default.createFile(atPath: disk, contents: Data("qcow2".utf8))
        let config = VMConfig(name: "win", directory: dir, engine: .qemu, cpus: 4, memory: "8G", disk: disk, firmwareCode: fw)
        try home.create(config)

        let calls = ToolCalls()
        var configurator = VMConfigurator()
        configurator.findTool = { "/fake/" + $0 }
        var snapshotsJSON = "[]"
        configurator.run = { exe, args in
            calls.add(URL(fileURLWithPath: exe).lastPathComponent, args)
            if args.first == "info" {
                return (0, #"{"virtual-size": \#(100 << 30), "snapshots": \#(snapshotsJSON)}"#)
            }
            return (0, "")
        }

        // Refuse shrinking below 100 GiB.
        #expect(throws: ProvisionError.self) { try configurator.configure(config, diskSize: "80G") }
        #expect(!FileManager.default.fileExists(atPath: VMConfigurator.pendingPartitionResizePath(dir)))

        // Refuse resizing when snapshots exist.
        snapshotsJSON = #"[{"name":"clean","date-sec":1700000000,"vm-state-size":0}]"#
        #expect(throws: ProvisionError.self) { try configurator.configure(config, diskSize: "150G") }
        #expect(!FileManager.default.fileExists(atPath: VMConfigurator.pendingPartitionResizePath(dir)))

        // Grow to 150 GiB when no snapshots exist.
        snapshotsJSON = "[]"
        let updated = try configurator.configure(config, cpus: 8, memory: "16G", diskSize: "150G")
        #expect(updated.cpus == 8 && updated.memory == "16G")
        #expect(FileManager.default.fileExists(atPath: VMConfigurator.pendingPartitionResizePath(dir)))
        #expect(calls.list.contains { $0.0 == "qemu-img" && $0.1 == ["resize", disk, String(150 << 30)] })
    }

    @Test(.timeLimit(.minutes(1)))
    func dispatcherReconfiguresStoppedVMs() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let name = "lin"
        let dir = home.directory(of: name)
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let disk = dir + "/disk.img"
        FileManager.default.createFile(atPath: disk, contents: nil)
        let handle = try FileHandle(forWritingTo: URL(fileURLWithPath: disk))
        try handle.truncate(atOffset: 64 << 30)
        try handle.close()
        let config = VMConfig(name: name, directory: dir, engine: .vz, cpus: 4, memory: "4G", disk: disk, firmwareCode: "", tpm: false)
        try home.create(config)

        let dispatcher = Dispatcher(home: home)
        let configured = await dispatcher.handle(
            .init(id: 1, method: Method.vmConfigure, params: try JSONValue(encoding: Params.Configure(vm: name, cpus: 6, memory: "8G", diskSize: "80G"))))
        #expect(configured.error == nil)
        let reloaded = try home.loadConfig(name)
        #expect(reloaded.cpus == 6 && reloaded.memory == "8G")
        #expect((try FileManager.default.attributesOfItem(atPath: disk)[.size] as? UInt64) == (80 << 30))
    }

    @Test func balloonSwitchesOnlyForQEMUVMs() throws {
        let win = VMConfig(name: "win", directory: "/vm/win", engine: .qemu, memory: "8G", firmwareCode: "/fw/code.fd")
        let lin = VMConfig(name: "lin", directory: "/vm/lin", engine: .vz, memory: "4G", firmwareCode: "", tpm: false)
        let configurator = VMConfigurator()
        #expect(try configurator.configure(win, balloon: true).balloon)
        #expect(try !configurator.configure(win, balloon: false).balloon)
        #expect(throws: ProvisionError.self) { try configurator.configure(lin, balloon: true) }
    }

    @Test(.timeLimit(.minutes(1)))
    func vmBalloonExplainsWhatIsMissing() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let fw = home.path + "/code.fd"
        FileManager.default.createFile(atPath: fw, contents: Data(count: 64))
        for (name, engine) in [("win", VMConfig.Engine.qemu), ("lin", .vz)] {
            let dir = home.directory(of: name)
            try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
            try home.create(VMConfig(name: name, directory: dir, engine: engine, memory: "4G", firmwareCode: engine == .qemu ? fw : "", tpm: false))
        }
        let dispatcher = Dispatcher(home: home)
        func balloon(_ vm: String, _ size: String? = nil, auto: Bool? = nil) async throws -> String? {
            await dispatcher.handle(.init(id: 1, method: Method.vmBalloon, params: try JSONValue(encoding: Params.Balloon(vm: vm, size: size, auto: auto)))).error?.message
        }
        func configure(_ vm: String, _ on: Bool) async throws -> String? {
            await dispatcher.handle(.init(id: 1, method: Method.vmConfigure, params: try JSONValue(encoding: Params.Configure(vm: vm, balloon: on)))).error?.message
        }

        #expect(try await balloon("lin")?.contains("not supported for VZ") == true)
        #expect(try await balloon("win")?.contains("glov vm configure win --balloon on") == true)
        #expect(try await configure("win", true) == nil)
        #expect(try home.loadConfig("win").balloon)
        // Stopped: the choice is saved for the next start.
        #expect(try await balloon("win") == nil)
        #expect(try await balloon("win", "3G") == nil)
        #expect(try home.loadConfig("win").balloonTarget == 3 << 30 && !home.loadConfig("win").balloonAuto)
        #expect(try await balloon("win", auto: true) == nil)
        #expect(try home.loadConfig("win").balloonAuto && home.loadConfig("win").balloonTarget == nil, "auto keeps no target")
        #expect(try await balloon("win", "4G") == nil)
        #expect(try home.loadConfig("win").balloonTarget == nil && !home.loadConfig("win").balloonAuto, "the full size keeps all")
        #expect(try await balloon("win", "8G")?.contains("more than VM win's memory (4G)") == true)
        #expect(try await balloon("win", "lots")?.contains("cannot read size lots") == true)
        #expect(try await configure("lin", true)?.contains("--balloon is for QEMU") == true)
        #expect(try await configure("win", false) == nil)
        #expect(try !home.loadConfig("win").balloon)
    }

    @Test(.timeLimit(.minutes(1)))
    func windowsGuestServiceExpandsSystemPartitionOnNextSessionAndClearsMarker() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let dir = f.home.directory(of: "win11")
        let marker = VMConfigurator.pendingPartitionResizePath(dir)
        FileManager.default.createFile(atPath: marker, contents: Data())
        let ranResize = Box("no")
        f.fake.script = { argv in
            if argv.first == "powershell.exe", argv.last == VMConfigurator.windowsResizePartitionScript {
                ranResize.value = "yes"
            }
            return [.exit(0)]
        }
        f.fake.dropConnection()
        await eventually { !FileManager.default.fileExists(atPath: marker) }
        #expect(ranResize.value == "yes")
    }
}
