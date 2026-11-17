import Foundation
import GlovCore

public struct ProvisionError: Error, CustomStringConvertible {
    public var description: String
    public init(_ description: String) { self.description = description }
}

/// Creates a VZ (Linux) VM from a cloud image: a raw `disk.img` grown to the requested size, an SSH key pair in
/// `secrets/`, and a cloud-init NoCloud seed (`seed.iso`) that creates the `glov` user and installs the Guest Service.
public struct LinuxProvisioner {
    public var name: String
    /// A qcow2 or raw cloud image (e.g. an Ubuntu arm64 server cloud image).
    public var image: String
    public var cpus: Int
    public var memory: String
    /// The disk is grown to this many bytes (never shrunk); cloud-init grows the root file system on first boot.
    public var diskSize: UInt64
    /// A Linux arm64 `glov-guest` to install on first boot.
    public var guestServiceBinary: String?
    /// No network device: the guest gets out only through the egress proxy, which allows nothing until `egress.allow` lists it.
    public var isolated = false
    /// Isolated VMs: harnesses set up in the guest; their runtime hosts join the allowlist. Their secrets are granted
    /// separately (`glov secret grant`).
    public var harnesses: [HarnessProfile] = []
    /// Runs a tool and returns its exit status; output never reaches the terminal, so nothing secret can.
    var run: (String, [String]) throws -> (status: Int32, output: String) = LinuxProvisioner.runTool
    /// Finds `glov-image`, which converts a qcow2 image.
    var imageTool: () throws -> String = { try DiskImageTool.locate() }
    /// Progress of the steps that take a while; the default writes to stderr.
    var log: (String) -> Void = { FileHandle.standardError.write(Data("\($0)\n".utf8)) }

    public init(name: String, image: String, cpus: Int = 2, memory: String = "4G", diskSize: UInt64 = 32 << 30, guestServiceBinary: String? = nil) {
        self.name = name
        self.image = image
        self.cpus = cpus
        self.memory = memory
        self.diskSize = diskSize
        self.guestServiceBinary = guestServiceBinary
    }

    /// Paths inside a VM directory.
    public static func diskPath(_ dir: String) -> String { dir + "/disk.img" }
    public static func seedPath(_ dir: String) -> String { dir + "/seed.iso" }
    public static func privateKeyPath(_ dir: String) -> String { dir + "/secrets/id_ed25519" }

    /// Builds everything and writes vm.json. On failure, removes the VM directory if this call created it.
    @discardableResult
    public func create(in home: GlovHome) throws -> VMConfig {
        guard GlovHome.isValid(name: name) else { throw HomeError.invalidName(name) }
        let fm = FileManager.default
        // A quick refusal before the inputs are checked; makeVMDirectory decides.
        guard !fm.fileExists(atPath: home.directory(of: name)) else { throw HomeError.exists(name) }
        guard fm.fileExists(atPath: image) else { throw ProvisionError("image not found: \(image)") }
        if let guestServiceBinary, !fm.isExecutableFile(atPath: guestServiceBinary) {
            throw ProvisionError("Guest Service binary not found or not executable: \(guestServiceBinary) (build it: cd guest && GOOS=linux GOARCH=arm64 go build -o <out> ./cmd/glov-guest)")
        }
        let dir = try home.makeVMDirectory(name)
        do {
            try makeDisk(at: Self.diskPath(dir))
            let publicKey = try makeKey(in: dir)
            try makeSeed(at: Self.seedPath(dir), publicKey: publicKey, scratch: dir + "/seed", egressCA: isolated ? try EgressCA.loadOrCreate(home: home).certificatePEM : nil)
            let config = VMConfig(
                name: name, directory: dir, engine: .vz, cpus: cpus, memory: memory, disk: Self.diskPath(dir),
                firmwareCode: "", firmwareVars: dir + "/efi-vars.fd", tpm: false, sshPort: nil,
                extraDrives: [.init(path: Self.seedPath(dir), cdrom: true)], macAddress: isolated ? nil : Self.randomMACAddress(),
                isolated: isolated, egress: isolated ? egress : nil, harnesses: harnesses.isEmpty ? nil : harnesses.map(\.name))
            try home.create(config)
            return try home.loadConfig(name)
        } catch {
            try? fm.removeItem(atPath: dir)
            throw error
        }
    }

    var egress: VMConfig.Egress {
        var allow: [String] = []
        for host in harnesses.flatMap(\.runtime) where !allow.contains(host) { allow.append(host) }
        return .init(allow: allow)
    }

    /// qcow2 is converted to a sparse raw disk by `glov-image`; a raw image is cloned (APFS copy-on-write). Then grown sparsely.
    func makeDisk(at path: String) throws {
        let header = try FileHandle(forReadingFrom: URL(fileURLWithPath: image)).read(upToCount: 4) ?? Data()
        if header == Data([0x51, 0x46, 0x49, 0xfb]) {  // "QFI\xfb"
            let tool = try imageTool()
            let r = try Heartbeat.whileRunning("converting \(URL(fileURLWithPath: image).lastPathComponent) to a raw disk", log: log) {
                try run(tool, ["convert", image, path])
            }
            guard r.status == 0 else { throw ProvisionError("cannot convert \(image) to a raw disk: \(r.output.trimmingCharacters(in: .whitespacesAndNewlines))") }
        } else {
            try FileManager.default.copyItem(atPath: image, toPath: path)
        }
        let size = (try FileManager.default.attributesOfItem(atPath: path)[.size] as? UInt64) ?? 0
        if size < diskSize {
            let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
            try h.truncate(atOffset: diskSize)
            try h.close()
        }
    }

    /// `secrets/id_ed25519` (0600, in a 0700 directory) and its public half; returns the public key line.
    func makeKey(in dir: String) throws -> String {
        let secrets = dir + "/secrets"
        try FileManager.default.createDirectory(atPath: secrets, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let key = Self.privateKeyPath(dir)
        let r = try run("/usr/bin/ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-C", "glov@\(name)", "-f", key])
        guard r.status == 0 else { throw ProvisionError("ssh-keygen failed with status \(r.status)") }
        return try String(contentsOfFile: key + ".pub", encoding: .utf8)
    }

    func makeSeed(at path: String, publicKey: String, scratch: String, egressCA: String? = nil) throws {
        var seed = CloudInitSeed(hostname: name, instanceID: "glov-\(name)-\(UUID().uuidString.lowercased())", sshPublicKey: publicKey, installGuestService: guestServiceBinary != nil)
        seed.isolated = isolated
        seed.harnesses = harnesses
        seed.egressCA = egressCA
        try? FileManager.default.removeItem(atPath: scratch)
        try seed.write(to: scratch, guestServiceBinary: guestServiceBinary)
        defer { try? FileManager.default.removeItem(atPath: scratch) }
        let r = try run("/usr/bin/hdiutil", ["makehybrid", "-quiet", "-iso", "-joliet", "-default-volume-name", CloudInitSeed.volumeLabel, "-o", path, scratch])
        guard r.status == 0 else { throw ProvisionError("hdiutil makehybrid failed: \(r.output)") }
    }

    /// A random unicast, locally administered address.
    public static func randomMACAddress() -> String {
        var bytes = (0..<6).map { _ in UInt8.random(in: 0...255) }
        bytes[0] = (bytes[0] & 0xfc) | 0x02
        return bytes.map { String(format: "%02x", $0) }.joined(separator: ":")
    }

    /// Blocks until the tool exits: async code calls the provisioners through `Blocking.run`.
    @available(*, noasync, message: "blocks a cooperative thread; call it through Blocking.run")
    static func runTool(_ exe: String, _ args: [String]) throws -> (status: Int32, output: String) {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        p.standardInput = FileHandle.nullDevice
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        // Not waitUntilExit, which spins a run loop and can miss the exit when other threads spawn processes too.
        let exited = DispatchSemaphore(value: 0)
        p.terminationHandler = { _ in exited.signal() }
        try p.run()
        let data = pipe.fileHandleForReading.readDataToEndOfFile()
        exited.wait()
        return (p.terminationStatus, String(decoding: data, as: UTF8.self))
    }
}
