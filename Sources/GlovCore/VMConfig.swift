import Foundation

/// A guest and the engine that runs it. Everything lives under `directory`.
///
/// QEMU (HVF, Windows) runs as a child process with display `none`. VZ (Virtualization.framework, Linux) runs inside
/// `glovd`; it uses `disk` (raw), `firmwareVars` (EFI variable store), `extraDrives` (read-only virtio block, e.g. the
/// cloud-init seed), `macAddress` and the files under `directory` named below, and ignores `firmwareCode`, `tpm`, `sshPort` and
/// `portForwards` (the host reaches a VZ guest at its NAT address directly).
public struct VMConfig: Codable, Sendable, Equatable {
    public enum Engine: String, Codable, Sendable, CaseIterable {
        case qemu
        case vz
    }

    /// A read-only drive attached over USB (install media, driver ISO, unattend image).
    public struct Drive: Codable, Sendable, Equatable {
        public var path: String
        public var cdrom: Bool
        public var removable: Bool
        public var bootIndex: Int?

        public init(path: String, cdrom: Bool = false, removable: Bool = false, bootIndex: Int? = nil) {
            self.path = path
            self.cdrom = cdrom
            self.removable = removable
            self.bootIndex = bootIndex
        }
    }

    /// QEMU: a TCP port on the host's 127.0.0.1 forwarded to a guest port (user-mode networking has no other way in).
    public struct PortForward: Codable, Sendable, Equatable {
        public var host: Int
        public var guest: Int

        public init(host: Int, guest: Int) {
            self.host = host
            self.guest = guest
        }
    }

    /// The project a `glov <harness>` VM belongs to (`ProjectAgent`), and how the project reaches the guest.
    public struct Project: Codable, Sendable, Equatable {
        /// How the project reaches the guest; fixed for the VM's life.
        public enum Sync: String, Codable, Sendable, CaseIterable {
            /// The host pushes its branch to a repository on the guest's own disk and fetches the guest's commits back.
            case git
            /// The working tree, `.git` included, is shared read-write.
            case live
        }
        /// The project's root on this Mac, which is also the repository's (git) or the share's (live) path in the guest.
        public var path: String
        public var sync: Sync

        public init(path: String, sync: Sync) {
            self.path = path
            self.sync = sync
        }
    }

    /// An isolated VM's egress allowlist (`EgressPolicy`), re-read for every connection.
    public struct Egress: Codable, Sendable, Equatable {
        public var allow: [String]
        /// True lets MCP clients change `allow` (`egress_allow`, `egress_remove`); only `glov egress mcp` sets it.
        public var mcpEdits: Bool?
        /// The secrets (`glov secret`) this VM may use, by name. Names only: a value never enters vm.json.
        public var secrets: [String]?
        /// The harnesses (`HarnessProfile.name`) signed in to the owner's subscription (`glov secret login`) instead of
        /// using an API key. Their token endpoints are intercepted, and the guest is set up for them.
        public var subscriptions: [String]?
        /// The repositories (`RepoPattern` texts) this VM may reach on the hosts of its git secrets (`forge`); none
        /// when nil.
        public var repos: [String]?
        /// The `allow` entries (exact names or addresses, canonical) that may resolve to private-network addresses
        /// (`glov egress allow --private`); only the CLI adds them. Nil when none.
        public var privateAllow: [String]?
        /// Extra CA certificates (`glov vm trust`) the guest's trust store holds and the egress proxy verifies this
        /// VM's intercepted upstream connections with, besides the Mac's. Nil when none.
        public var trust: [Trust]?
        /// True lets the guest reach any public host, still only through the egress proxy (`glov vm configure --egress
        /// open`): substitution, repository checks and the log are unchanged, `allow` waits for a switch back. Only the
        /// CLI sets it. Nil is restricted.
        public var open: Bool?
        /// The secrets the owner let this VM hold as real values in tmpfs while open (`--allow-plain-secrets`); while
        /// open, glovd withholds any other `tmpfs` secret from the guest. Cleared with `open`.
        public var plainSecrets: [String]?

        /// A CA certificate given to the VM as trust data: public, so it lives in vm.json.
        public struct Trust: Codable, Sendable, Equatable {
            /// `[A-Za-z0-9._-]`, 1 to 64 characters; the guest file is `<name>.crt`.
            public var name: String
            /// One certificate, PEM, as glov re-encoded it.
            public var certificate: String
            public init(name: String, certificate: String) { self.name = name; self.certificate = certificate }
        }

        public init(
            allow: [String] = [], mcpEdits: Bool? = nil, secrets: [String]? = nil, subscriptions: [String]? = nil, repos: [String]? = nil,
            privateAllow: [String]? = nil, trust: [Trust]? = nil, open: Bool? = nil, plainSecrets: [String]? = nil
        ) {
            self.allow = allow
            self.mcpEdits = mcpEdits
            self.secrets = secrets
            self.subscriptions = subscriptions
            self.repos = repos
            self.privateAllow = privateAllow
            self.trust = trust
            self.open = open
            self.plainSecrets = plainSecrets
        }
    }

    /// A Windows install that `glov vm create` prepared and the daemon runs on the VM's first start: it types the
    /// installer's boot line, waits for the Guest Service, then detaches `drives` and clears this.
    public struct Install: Codable, Sendable, Equatable {
        /// The entries of `extraDrives` only Setup needs (install ISO, virtio-win, unattend image), by path.
        public var drives: [String]
        /// When the first start booted the installer. Set while the VM is stopped, the install was interrupted.
        public var started: Date?
        /// Why the install failed. A failed or interrupted install is not resumed: the VM is deleted and created again.
        public var failure: String?

        public init(drives: [String], started: Date? = nil, failure: String? = nil) {
            self.drives = drives
            self.started = started
            self.failure = failure
        }

        enum CodingKeys: String, CodingKey { case drives, started, failure }

        /// `started` is an ISO 8601 string, readable in vm.json whatever date strategy the coder uses.
        public init(from decoder: any Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            drives = try c.decode([String].self, forKey: .drives)
            failure = try c.decodeIfPresent(String.self, forKey: .failure)
            started = try c.decodeIfPresent(String.self, forKey: .started).map { text in
                guard let date = try? Date(text, strategy: .iso8601) else {
                    throw DecodingError.dataCorruptedError(forKey: .started, in: c, debugDescription: "not an ISO 8601 date: \(text)")
                }
                return date
            }
        }

        public func encode(to encoder: any Encoder) throws {
            var c = encoder.container(keyedBy: CodingKeys.self)
            try c.encode(drives, forKey: .drives)
            try c.encodeIfPresent(started?.formatted(.iso8601), forKey: .started)
            try c.encodeIfPresent(failure, forKey: .failure)
        }
    }

    public var name: String
    public var directory: String
    /// `qemu` for vm.json files written before engines existed.
    public var engine: Engine
    public var cpus: Int
    /// QEMU `-m` syntax, e.g. "16G" (both engines).
    public var memory: String
    /// System disk: qcow2 attached as NVMe (QEMU) or a raw image on virtio block (VZ). Nil boots without a disk.
    public var disk: String?
    /// UEFI code for QEMU pflash; empty for VZ, whose firmware is built in.
    public var firmwareCode: String
    public var firmwareVars: String
    public var tpm: Bool
    /// Host port forwarded to guest port 22 on 127.0.0.1.
    public var sshPort: Int?
    /// QEMU: attach the user-mode NIC even with no port forwarded. Without it, a NIC is attached only for `sshPort` or `portForwards`.
    public var network: Bool
    public var portForwards: [PortForward]
    public var extraDrives: [Drive]
    /// Attach the virtio-serial port the Guest Service talks over (`guestSocketPath`).
    public var guestService: Bool
    /// When false the daemon refuses exec, file, copy and input tools for this VM (screenshots stay allowed).
    public var agentControl: Bool
    /// Host directories `copy_to_vm` / `copy_from_vm` may read or write.
    public var hostPathAllowlist: [String]
    /// VZ: the NIC's MAC address, fixed so the guest keeps its DHCP lease across boots. Nil picks a random one per start.
    public var macAddress: String?
    /// A Windows install still to finish (QEMU only).
    public var install: Install?
    /// Host folders shared with the guest (`Share`); none unless added.
    public var shares: [Share]
    /// VZ: no network device; the guest gets out only through the egress proxy (docs/guest-protocol.md).
    public var isolated: Bool
    /// VZ, isolated: where the egress proxy lets the guest connect. Nil allows nothing.
    public var egress: Egress?
    /// VZ, isolated: the harness profiles (`HarnessProfile.name`) it was created with, which `glov vm harness install` installs.
    public var harnesses: [String]?
    /// QEMU: give the guest a virtio-balloon device, so `glov vm balloon` can take memory back. Only glov-qemu frees what the
    /// guest hands over on macOS, so the device is added only on that QEMU. `glov vm create` turns it on for a Windows VM
    /// only when glov-qemu is installed then; off for VMs made before it existed.
    public var balloon: Bool
    /// QEMU with a balloon: glovd picks the target from the guest's use (`glov vm balloon <vm> auto`, `BalloonPolicy`).
    /// `glov vm create` turns it on with the device; a fixed size (`glov vm balloon <vm> <size>`) turns it off.
    public var balloonAuto: Bool
    /// The fixed size the guest keeps, in bytes, applied whenever the VM starts; nil keeps all of `memory`. Auto keeps
    /// none: an auto VM starts with all of `memory`.
    public var balloonTarget: UInt64?
    /// VZ, isolated: the project of a `glov <harness>` VM. Nil for other VMs, and for project VMs made before git mode,
    /// which share the project live (`projectSync(path:)`).
    public var project: Project?

    /// How `path`'s project reaches this VM: the recorded mode, or live for a VM made before modes that shares `path`
    /// as `ProjectAgent.shareName`; nil when the VM is not that project's.
    public func projectSync(path: String) -> Project.Sync? {
        if let project { return project.path == path ? project.sync : nil }
        return shares.contains { $0.name == ProjectAgent.shareName && $0.hostPath == path } ? .live : nil
    }

    /// Whether this is a project's VM (`glov <harness>`), which is never reverted to a snapshot.
    public var isProjectVM: Bool { project != nil || shares.contains { $0.name == ProjectAgent.shareName } }

    /// The VM's shared folder and `scratch` next to the VM directories (the glov home).
    public static func defaultAllowlist(directory: String) -> [String] {
        let dir = URL(fileURLWithPath: directory)
        return [dir.appendingPathComponent("shared").path, dir.deletingLastPathComponent().appendingPathComponent("scratch").path]
    }

    public init(
        name: String, directory: String, engine: Engine = .qemu, cpus: Int = 2, memory: String = "4G", disk: String? = nil,
        firmwareCode: String, firmwareVars: String? = nil, tpm: Bool = true, sshPort: Int? = nil, network: Bool = false,
        portForwards: [PortForward] = [], extraDrives: [Drive] = [], guestService: Bool = true, agentControl: Bool = true, hostPathAllowlist: [String]? = nil,
        macAddress: String? = nil, install: Install? = nil, shares: [Share] = [], isolated: Bool = false, egress: Egress? = nil,
        harnesses: [String]? = nil, balloon: Bool = false, balloonAuto: Bool = false, balloonTarget: UInt64? = nil,
        project: Project? = nil
    ) {
        self.name = name
        self.directory = directory
        self.engine = engine
        self.cpus = cpus
        self.memory = memory
        self.disk = disk
        self.firmwareCode = firmwareCode
        self.firmwareVars = firmwareVars ?? URL(fileURLWithPath: directory).appendingPathComponent("vars.fd").path
        self.tpm = tpm
        self.sshPort = sshPort
        self.network = network
        self.portForwards = portForwards
        self.extraDrives = extraDrives
        self.guestService = guestService
        self.agentControl = agentControl
        self.hostPathAllowlist = hostPathAllowlist ?? Self.defaultAllowlist(directory: directory)
        self.macAddress = macAddress
        self.install = install
        self.shares = shares
        self.isolated = isolated
        self.egress = egress
        self.harnesses = harnesses
        self.balloon = balloon
        self.balloonAuto = balloonAuto
        self.balloonTarget = balloonTarget
        self.project = project
    }

    /// Keys added after the first release are optional, so older `vm.json` files still load.
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            name: try c.decode(String.self, forKey: .name), directory: try c.decode(String.self, forKey: .directory),
            engine: try c.decodeIfPresent(Engine.self, forKey: .engine) ?? .qemu,
            cpus: try c.decode(Int.self, forKey: .cpus), memory: try c.decode(String.self, forKey: .memory),
            disk: try c.decodeIfPresent(String.self, forKey: .disk), firmwareCode: try c.decode(String.self, forKey: .firmwareCode),
            firmwareVars: try c.decode(String.self, forKey: .firmwareVars), tpm: try c.decode(Bool.self, forKey: .tpm),
            sshPort: try c.decodeIfPresent(Int.self, forKey: .sshPort), network: try c.decodeIfPresent(Bool.self, forKey: .network) ?? false,
            portForwards: try c.decodeIfPresent([PortForward].self, forKey: .portForwards) ?? [], extraDrives: try c.decode([Drive].self, forKey: .extraDrives),
            guestService: try c.decodeIfPresent(Bool.self, forKey: .guestService) ?? true,
            agentControl: try c.decodeIfPresent(Bool.self, forKey: .agentControl) ?? true,
            hostPathAllowlist: try c.decodeIfPresent([String].self, forKey: .hostPathAllowlist),
            macAddress: try c.decodeIfPresent(String.self, forKey: .macAddress),
            install: try c.decodeIfPresent(Install.self, forKey: .install),
            shares: try c.decodeIfPresent([Share].self, forKey: .shares) ?? [],
            isolated: try c.decodeIfPresent(Bool.self, forKey: .isolated) ?? false,
            egress: try c.decodeIfPresent(Egress.self, forKey: .egress),
            harnesses: try c.decodeIfPresent([String].self, forKey: .harnesses),
            balloon: try c.decodeIfPresent(Bool.self, forKey: .balloon) ?? false,
            balloonAuto: try c.decodeIfPresent(Bool.self, forKey: .balloonAuto) ?? false,
            balloonTarget: try c.decodeIfPresent(UInt64.self, forKey: .balloonTarget),
            project: try c.decodeIfPresent(Project.self, forKey: .project))
    }

    private func path(_ component: String) -> String {
        URL(fileURLWithPath: directory).appendingPathComponent(component).path
    }

    public var qmpSocketPath: String { path("qmp.sock") }
    public var pidFilePath: String { path("qemu.pid") }
    public var logPath: String { path("qemu.log") }
    public var tpmDirectory: String { path("tpm") }
    public var tpmSocketPath: String { path("tpm/swtpm.sock") }
    public var guestSocketPath: String { path("guest.sock") }
    public var jobsDirectory: String { path("jobs") }
    /// QEMU: per-snapshot copies of the TPM state, kept for snapshots taken while the VM is stopped. VZ: the snapshots (`VZSnapshots`).
    public var snapshotsDirectory: String { path("snapshots") }
    /// VZ: the generic platform's machine identifier (`dataRepresentation`), created on first start.
    public var machineIdentifierPath: String { path("machine-id") }
    /// VZ: what the guest writes to its virtio console (`/dev/hvc0`), rewritten on each start.
    public var consoleLogPath: String { path("console.log") }
    /// VZ: the console log of the boot before, kept on each start so a hang survives the restart that ends it.
    public var previousConsoleLogPath: String { path("console.previous.log") }
    /// Isolated VMs: the egress proxy's records (`EgressLog`).
    public var egressLogPath: String { path("egress.log") }
    /// Private keys and the like; never printed.
    public var secretsDirectory: String { path("secrets") }

    /// `memory` in bytes.
    public var memoryBytes: UInt64? { Self.bytes(memory) }

    /// A number with an optional K, M, G or T suffix (binary units), MiB without one, as QEMU `-m` reads it.
    public static func bytes(_ size: String) -> UInt64? {
        let s = size.trimmingCharacters(in: .whitespaces).uppercased()
        let units: [Character: UInt64] = ["K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40]
        let (digits, unit) = s.last.flatMap { units[$0] }.map { (String(s.dropLast()), $0) } ?? (s, 1 << 20)
        guard let n = UInt64(digits), n > 0 else { return nil }
        let (bytes, overflow) = n.multipliedReportingOverflow(by: unit)
        return overflow ? nil : bytes
    }

    /// Indices in `extraDrives` of the drives only the install needs.
    public var installDriveIndices: [Int] {
        guard let install else { return [] }
        return extraDrives.indices.filter { install.drives.contains(extraDrives[$0].path) }
    }

    /// This VM once its install has finished: without the install drives, so the next start boots from the disk only.
    public func finishingInstall() -> VMConfig {
        var c = self
        c.extraDrives = extraDrives.filter { d in !(install?.drives.contains(d.path) ?? false) }
        c.install = nil
        return c
    }

    /// The QEMU device id of `extraDrives[index]`, for `device_del`.
    public static func driveDeviceID(_ index: Int) -> String { "extra\(index)-usb" }

    /// Block node name of the system disk; `snapshot-save` and friends address it instead of the drive id.
    public static let systemNode = "sys0"
    /// Device id of the virtio-balloon device (`balloon`).
    public static let balloonDeviceID = "balloon0"

    /// macOS `sun_path` holds 104 bytes including the NUL.
    static let maxSocketPath = 103

    func validateSocketPaths() throws {
        for p in [qmpSocketPath] + (tpm ? [tpmSocketPath] : []) + (guestService ? [guestSocketPath] : []) where p.utf8.count > Self.maxSocketPath {
            throw GlovError.socketPathTooLong(p)
        }
    }

    /// Arguments for `qemu-system-aarch64`. `paused` adds `-S`: the guest does not run until QMP `cont`.
    /// `balloonSupported` says the QEMU is glov-qemu; only then does `balloon` add the device.
    public func qemuArguments(paused: Bool = false, balloonSupported: Bool = false) throws -> [String] {
        try validateSocketPaths()
        // QEMU option values treat "," as a separator; a literal comma is written ",,".
        func e(_ s: String) -> String { s.replacingOccurrences(of: ",", with: ",,") }

        var a = [
            "-name", "glov-\(name)",
            "-machine", "virt,gic-version=3,highmem=on",
            "-accel", "hvf,kernel-irqchip=on",
            "-cpu", "host", "-smp", String(cpus), "-m", memory,
            "-nodefaults", "-serial", "none",
            "-drive", "if=pflash,format=raw,readonly=on,file=\(e(firmwareCode))",
            "-drive", "if=pflash,format=raw,file=\(e(firmwareVars))",
        ]
        if tpm {
            a += [
                "-chardev", "socket,id=chrtpm,path=\(e(tpmSocketPath))",
                "-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
                // ppi=on maps an unaligned RAM region, which HVF rejects (HV_BAD_ARGUMENT).
                "-device", "tpm-tis-device,tpmdev=tpm0,ppi=off",
            ]
        }
        if let disk {
            a += [
                "-drive", "if=none,id=disk0,node-name=\(Self.systemNode),file=\(e(disk)),format=qcow2,cache=writeback,discard=unmap",
                "-device", "nvme,drive=disk0,serial=glov0,bootindex=0",
            ]
        }
        let forwards = (sshPort.map { [PortForward(host: $0, guest: 22)] } ?? []) + portForwards
        if !forwards.isEmpty || network {
            a += [
                "-netdev", "user,id=net0" + forwards.map { ",hostfwd=tcp:127.0.0.1:\($0.host)-:\($0.guest)" }.joined(),
                "-device", "virtio-net-pci,netdev=net0",
            ]
        }
        if guestService {
            // The Guest Service opens this port by name; QEMU listens on the socket and never waits for a client.
            a += [
                "-device", "virtio-serial-pci,id=vser0",
                "-chardev", "socket,id=guest0,path=\(e(guestSocketPath)),server=on,wait=off",
                "-device", "virtserialport,bus=vser0.0,chardev=guest0,name=org.glov.guest.0",
            ]
        }
        if balloon && balloonSupported { a += ["-device", "virtio-balloon-pci,id=\(Self.balloonDeviceID)"] }
        a += ["-device", "qemu-xhci,id=xhci", "-device", "usb-kbd", "-device", "usb-tablet", "-device", "ramfb"]
        for (i, d) in extraDrives.enumerated() {
            a += ["-drive", "if=none,id=extra\(i),file=\(e(d.path)),format=raw,readonly=on" + (d.cdrom ? ",media=cdrom" : "")]
            a += ["-device", "usb-storage,id=\(Self.driveDeviceID(i)),drive=extra\(i)" + (d.removable ? ",removable=on" : "")
                + (d.bootIndex.map { ",bootindex=\($0)" } ?? "")]
        }
        // cocoa would make QEMU a GUI app, and App Nap throttles it. There is deliberately no option for it.
        a += [
            "-display", "none",
            "-qmp", "unix:\(e(qmpSocketPath)),server,wait=off",
            "-pidfile", pidFilePath,
        ]
        if paused { a.append("-S") }
        return a
    }
}

public enum GlovError: Error, Equatable, CustomStringConvertible {
    case socketPathTooLong(String)
    case executableNotFound(String)
    case alreadyRunning(pid: Int32)
    case notRunning
    case startFailed(String)
    case screenshotFailed(String)
    case unsupportedCharacter(Character)
    case unknownKey(String)
    /// A tool the VM's engine does not implement (yet).
    case unsupported(String)
    /// The process lacks `com.apple.security.virtualization`, so Virtualization.framework refuses to run VMs.
    case missingVirtualizationEntitlement

    public var description: String {
        switch self {
        case .socketPathTooLong(let p): "unix socket path exceeds \(VMConfig.maxSocketPath) bytes, use a shorter VM directory: \(p)"
        case .executableNotFound(let n): "\(n) not found on PATH, /opt/homebrew/bin or /usr/local/bin"
        case .alreadyRunning(let pid): "VM already running (pid \(pid))"
        case .notRunning: "VM is not running"
        case .startFailed(let m): "VM failed to start: \(m)"
        case .screenshotFailed(let m): "screenshot failed: \(m)"
        case .unsupportedCharacter(let c): "cannot type character \(String(reflecting: c))"
        case .unknownKey(let k): "unknown key \(String(reflecting: k))"
        case .unsupported(let m): m
        case .missingVirtualizationEntitlement:
            "glovd lacks the com.apple.security.virtualization entitlement, so it cannot run VZ (Linux) VMs: run glov host install, which installs a signed copy, or for a daemon started by hand sign it with tools/linux-vm/sign-daemon.sh (after every swift build) and restart it"
        }
    }
}
