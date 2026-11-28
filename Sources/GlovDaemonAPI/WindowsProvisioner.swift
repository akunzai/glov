import Foundation
import GlovCore

/// Prepares a QEMU VM that installs Windows 11 Arm64 unattended on its first start: a thin qcow2 disk, a blank UEFI
/// variable store, TPM state (created by swtpm on start), a random password for the `glov` account in `secrets/`, and
/// a FAT16 image labelled UNATTEND with `autounattend.xml`, `setup.ps1` and the Guest Service. The install ISO, virtio-win and
/// that image are attached as install drives; the daemon boots the installer and detaches them once the Guest Service is ready.
/// With `ssh`, the guest also gets OpenSSH (key only) for `secrets/id_ed25519`, and `sshPort` forwards to its port 22.
public struct WindowsProvisioner {
    public var name: String
    /// A bootable Windows 11 Arm64 ISO (`glov host media`).
    public var iso: String
    /// The virtio-win ISO (`glov host media`).
    public var virtio: String
    /// A Windows arm64 `glov-guest.exe`.
    public var guestServiceBinary: String
    public var firmwareCode: String
    /// The defaults of `glov vm create` for a Windows install.
    public static let defaultLanguage = "en-US", defaultEdition = "Professional"
    public static let defaultCPUs = 6, defaultMemory = "12G", defaultDiskSize = "200G"

    /// e.g. zh-TW: Setup's UI, system and user locale.
    public var language = defaultLanguage
    /// A Windows time zone ID, e.g. Taipei Standard Time; nil leaves Setup's default (Pacific).
    public var timeZone: String?
    /// A key of `WindowsAssets.editions`.
    public var edition = defaultEdition
    public var cpus = defaultCPUs
    public var memory = defaultMemory
    public var diskSize: UInt64 = VMConfig.bytes(defaultDiskSize)!
    /// Host port for guest port 22; nil installs no OpenSSH.
    public var sshPort: Int?
    public var portForwards: [VMConfig.PortForward] = []
    /// More drives, after the install drives.
    public var extraDrives: [VMConfig.Drive] = []
    /// Runs a tool and returns its exit status; output never reaches the terminal, so nothing secret can.
    var run: (String, [String]) throws -> (status: Int32, output: String) = LinuxProvisioner.runTool
    /// Whether glovd would start this VM on glov-qemu: only then does it get the balloon device by default.
    var balloonSupported: () -> Bool = {
        (try? VirtualMachine.findQEMU()).map { VirtualMachine.supportsBalloon(qemu: $0) } ?? false
    }

    public init(name: String, iso: String, virtio: String, guestServiceBinary: String, firmwareCode: String) {
        self.name = name
        self.iso = iso
        self.virtio = virtio
        self.guestServiceBinary = guestServiceBinary
        self.firmwareCode = firmwareCode
    }

    /// Paths inside a VM directory.
    public static func diskPath(_ dir: String) -> String { dir + "/disk.qcow2" }
    public static func passwordPath(_ dir: String) -> String { dir + "/secrets/admin-password" }
    public static func privateKeyPath(_ dir: String) -> String { dir + "/secrets/id_ed25519" }
    /// It holds the password in plain text, so it lives in `secrets/`; the daemon deletes it once the install is done.
    public static func unattendImagePath(_ dir: String) -> String { dir + "/secrets/unattend.cdr" }

    /// A host command required to build and run Windows VMs on macOS and the Homebrew formula that provides it.
    public struct HostToolRequirement: Equatable, Sendable {
        public var tool: String
        public var package: String

        public init(tool: String, package: String) {
            self.tool = tool
            self.package = package
        }
    }

    /// Host tools required by the Windows VM create and start path, in install order.
    public static let hostTools: [HostToolRequirement] = [
        .init(tool: "qemu-system-aarch64", package: "qemu"),
        .init(tool: "qemu-img", package: "qemu"),
        .init(tool: "swtpm", package: "swtpm"),
        .init(tool: "wimlib-imagex", package: "wimlib"),
    ]

    /// Returns the Windows host tools missing from `glov-qemu`, `PATH`, `/opt/homebrew/bin` and `/usr/local/bin`.
    public static func missingHostTools(
        findQEMU: () throws -> String = { try VirtualMachine.findQEMU() },
        findQEMUImg: () throws -> String = { try VirtualMachine.findQEMUImg() },
        find: (String) throws -> String = { try VirtualMachine.find($0) }
    ) -> [HostToolRequirement] {
        hostTools.filter { req in
            switch req.tool {
            case "qemu-system-aarch64": (try? findQEMU()) == nil
            case "qemu-img": (try? findQEMUImg()) == nil
            default: (try? find(req.tool)) == nil
            }
        }
    }

    /// Deduplicated Homebrew formulas for `missing`, preserving order.
    public static func brewPackages(for missing: [HostToolRequirement]) -> [String] {
        var seen: Set<String> = []
        return missing.map(\.package).filter { seen.insert($0).inserted }
    }

    /// Error text naming every missing Windows host tool and one `brew install` command, or `nil` when none are missing.
    public static func missingHostToolsMessage(_ missing: [HostToolRequirement]) -> String? {
        guard !missing.isEmpty else { return nil }
        let tools = missing.map(\.tool).joined(separator: ", ")
        let packages = brewPackages(for: missing).joined(separator: " ")
        return "missing host tools for Windows VMs: \(tools) (brew install \(packages))"
    }

    /// Non-fatal note for `glov host install` when Windows host tools are missing, or `nil` when all are present.
    public static func missingHostToolsNote(_ missing: [HostToolRequirement]) -> String? {
        guard !missing.isEmpty else { return nil }
        let tools = missing.map(\.tool).joined(separator: ", ")
        let packages = brewPackages(for: missing).joined(separator: " ")
        return "note: Windows VMs need \(tools) (brew install \(packages)); Linux VMs need none of them"
    }

    /// Verifies `path` begins with a Windows PE ("MZ") header.
    public static func checkGuestServiceBinary(_ path: String) throws {
        let header = (try? FileHandle(forReadingFrom: URL(fileURLWithPath: path)).read(upToCount: 2)) ?? nil
        guard header == Data("MZ".utf8) else {
            throw ProvisionError("not a Windows executable: \(path) (build it: cd guest && GOOS=windows GOARCH=arm64 go build -o <out>.exe ./cmd/glov-guest)")
        }
    }

    /// Verifies every Windows host tool and resolves the UEFI firmware before `vm create` downloads anything.
    public static func checkHostTools(
        firmwareCode: String?,
        findQEMU: () throws -> String = { try VirtualMachine.findQEMU() },
        findQEMUImg: () throws -> String = { try VirtualMachine.findQEMUImg() },
        find: (String) throws -> String = { try VirtualMachine.find($0) },
        defaultFirmware: () throws -> String = { try WindowsProvisioner.defaultFirmware() }
    ) throws -> String {
        let missing = missingHostTools(findQEMU: findQEMU, findQEMUImg: findQEMUImg, find: find)
        if let message = missingHostToolsMessage(missing) {
            throw ProvisionError(message)
        }
        if let firmwareCode {
            guard FileManager.default.fileExists(atPath: firmwareCode) else {
                throw ProvisionError("UEFI firmware not found: \(firmwareCode)")
            }
            return firmwareCode
        }
        return try defaultFirmware()
    }

    /// The EDK2 firmware that ships with QEMU, next to `qemu-system-aarch64` (through its unversioned prefix, so a QEMU
    /// upgrade does not break the VM). Homebrew's qemu comes first, so the VM keeps booting if glov-qemu is uninstalled;
    /// glov-qemu's copy serves when it is the only QEMU.
    public static func defaultFirmware(glovQEMUPrefixes: [String] = VirtualMachine.glovQEMUPrefixes) throws -> String {
        let fm = FileManager.default
        var candidates: [String] = []
        if let qemu = try? VirtualMachine.find("qemu-system-aarch64") {
            candidates.append(URL(fileURLWithPath: qemu).deletingLastPathComponent().deletingLastPathComponent().path)
        }
        candidates += glovQEMUPrefixes.filter { fm.isExecutableFile(atPath: $0 + "/bin/qemu-system-aarch64") }
        guard !candidates.isEmpty else { throw GlovError.executableNotFound("qemu-system-aarch64") }
        let codes = candidates.map { $0 + "/share/qemu/edk2-aarch64-code.fd" }
        guard let code = codes.first(where: fm.fileExists(atPath:)) else {
            throw ProvisionError("UEFI firmware not found at \(codes[0]); pass --firmware-code")
        }
        return code
    }

    /// A TCP port on 127.0.0.1 that nothing listens on now.
    public static func freePort() throws -> Int {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(fd) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_addr.s_addr = UInt32(0x7f00_0001).bigEndian
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        let ok = withUnsafeMutablePointer(to: &addr) { p in
            p.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, len) == 0 && getsockname(fd, $0, &len) == 0 }
        }
        guard ok else { throw ProvisionError("cannot find a free TCP port: \(String(cString: strerror(errno)))") }
        return Int(UInt16(bigEndian: addr.sin_port))
    }

    /// 24 random letters and digits: no character needs escaping in XML or PowerShell.
    static func randomPassword() -> String {
        let alphabet = Array("ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789")
        var rng = SystemRandomNumberGenerator()
        return String((0..<24).map { _ in alphabet.randomElement(using: &rng)! })
    }

    /// Builds everything and writes vm.json. On failure, removes the VM directory if this call created it.
    @discardableResult
    public func create(in home: GlovHome) throws -> VMConfig {
        guard GlovHome.isValid(name: name) else { throw HomeError.invalidName(name) }
        let fm = FileManager.default
        // A quick refusal before the inputs are checked; makeVMDirectory decides.
        guard !fm.fileExists(atPath: home.directory(of: name)) else { throw HomeError.exists(name) }
        _ = try WindowsAssets.edition(edition)
        for (what, path) in [("install ISO", iso), ("virtio-win ISO", virtio), ("UEFI firmware", firmwareCode)] where !fm.fileExists(atPath: path) {
            throw ProvisionError("\(what) not found: \(path)")
        }
        try Self.checkGuestServiceBinary(guestServiceBinary)
        let dir = try home.makeVMDirectory(name)
        do {
            let qemuImg: String
            do { qemuImg = try VirtualMachine.findQEMUImg() } catch { throw ProvisionError("\(error) (brew install qemu)") }
            let disk = Self.diskPath(dir)
            let r = try run(qemuImg, ["create", "-q", "-f", "qcow2", "-o", "cluster_size=1M,lazy_refcounts=on", disk, String(diskSize)])
            guard r.status == 0 else { throw ProvisionError("qemu-img create failed: \(r.output)") }

            try fm.createDirectory(atPath: dir + "/secrets", withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let password = Self.randomPassword()
            try writePrivate(Data(password.utf8), to: Self.passwordPath(dir))
            let publicKey = try sshPort.map { _ in try makeKey(in: dir) }

            let image = Self.unattendImagePath(dir)
            try makeUnattendImage(at: image, password: password, publicKey: publicKey, scratch: dir + "/secrets/unattend")
            let installDrives: [VMConfig.Drive] = [
                // The ISO must be a CD: as a USB disk, Setup cannot find its own media. EDK2 ignores bootindex for
                // usb-storage, so the daemon boots it from the UEFI shell.
                .init(path: iso, cdrom: true, bootIndex: 1),
                .init(path: virtio, cdrom: true),
                .init(path: image, removable: true),
            ]
            let balloon = balloonSupported()
            let config = VMConfig(
                name: name, directory: dir, cpus: cpus, memory: memory, disk: disk, firmwareCode: firmwareCode, tpm: true,
                sshPort: sshPort, network: true, portForwards: portForwards, extraDrives: installDrives + extraDrives,
                install: .init(drives: installDrives.map(\.path)), balloon: balloon, balloonAuto: balloon)
            try home.create(config)
            return try home.loadConfig(name)
        } catch {
            try? fm.removeItem(atPath: dir)
            throw error
        }
    }

    /// Created with mode 0600 before anything is written, so the secret is never readable by others.
    func writePrivate(_ data: Data, to path: String) throws {
        guard FileManager.default.createFile(atPath: path, contents: nil, attributes: [.posixPermissions: 0o600]) else {
            throw ProvisionError("cannot create \(path)")
        }
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
        defer { try? h.close() }
        try h.write(contentsOf: data)
    }

    /// `secrets/id_ed25519` (0600) and its public half; returns the public key line.
    func makeKey(in dir: String) throws -> String {
        let key = Self.privateKeyPath(dir)
        let r = try run("/usr/bin/ssh-keygen", ["-q", "-t", "ed25519", "-N", "", "-C", "glov@\(name)", "-f", key])
        guard r.status == 0 else { throw ProvisionError("ssh-keygen failed with status \(r.status)") }
        return try String(contentsOfFile: key + ".pub", encoding: .utf8)
    }

    /// The files of the unattend image, in `dir`.
    func writeUnattendFiles(to dir: String, password: String, publicKey: String?) throws {
        let fm = FileManager.default
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try writePrivate(Data(try WindowsAssets.autounattend(password: password, language: language, edition: edition, timeZone: timeZone).utf8), to: dir + "/autounattend.xml")
        try Data(WindowsAssets.setupScript(publicKey: publicKey).utf8).write(to: URL(fileURLWithPath: dir + "/setup.ps1"))
        if publicKey != nil { try Data(WindowsAssets.openSSHScript.utf8).write(to: URL(fileURLWithPath: dir + "/setup-openssh-server.ps1")) }
        try fm.copyItem(atPath: guestServiceBinary, toPath: dir + "/glov-guest.exe")
    }

    /// A FAT16 image (raw, `hdiutil -format UDTO`) of the unattend files; the staging directory is removed either way.
    func makeUnattendImage(at path: String, password: String, publicKey: String?, scratch: String) throws {
        let fm = FileManager.default
        try? fm.removeItem(atPath: scratch)
        defer { try? fm.removeItem(atPath: scratch) }
        try writeUnattendFiles(to: scratch, password: password, publicKey: publicKey)
        let guestServiceSize = (try? fm.attributesOfItem(atPath: guestServiceBinary)[.size] as? UInt64) ?? 0
        let megabytes = max(32, Int(guestServiceSize >> 20) + 16)
        let r = try run("/usr/bin/hdiutil", [
            "create", "-quiet", "-megabytes", String(megabytes), "-fs", "MS-DOS FAT16", "-volname", "UNATTEND",
            "-layout", "MBRSPUD", "-format", "UDTO", "-srcfolder", scratch, "-o", path,
        ])
        guard r.status == 0 else { throw ProvisionError("hdiutil create failed: \(r.output)") }
        // hdiutil names a UDTO image <path>.cdr unless the path already ends in .cdr.
        guard fm.fileExists(atPath: path) else { throw ProvisionError("hdiutil did not write \(path)") }
        try fm.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path)
    }
}
