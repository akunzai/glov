import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI
import GlovViewer

private let cliName = "glov"

struct Glov: AsyncParsableCommand {
    @OptionGroup var connection: RemoteOptions
    static let configuration = CommandConfiguration(
        commandName: cliName,
        abstract: "Control glov VMs through glovd.",
        version: GlovVersion.string,
        subcommands: [VMCommand.self, AgentCommand.self, Exec.self, Copy.self, SSH.self, View.self, ShareCommand.self, SnapshotCommand.self, EgressCommand.self, SecretCommand.self, MCPCommand.self, DaemonCommand.self, VersionCommand.self, Guide.self, SSHRelayCommand.self])
}

/// The daemon of the root named by $GLOV_HOME (default ~/Library/Application Support/glov).
func daemonClient() -> DaemonClient {
    if let remoteDaemon { return DaemonClient(remote: remoteDaemon) }
    return DaemonClient(socketPath: GlovHome.resolve().socketPath)
}

/// Runs `body`, printing errors as one readable line instead of ArgumentParser's default.
func report(_ body: () async throws -> Void) async throws {
    do { try await body() } catch let e as ExitCode { throw e } catch {
        FileHandle.standardError.write(Data("\(cliName): \(error is CocoaError ? error.localizedDescription : "\(error)")\n".utf8))
        throw ExitCode.failure
    }
}

struct VMCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "vm", abstract: "List, create, configure, start, stop, reboot and delete VMs; also their screenshots, resource use, Guest Service, harnesses and trusted certificates.",
        subcommands: [List.self, Status.self, Configure.self, Start.self, Stop.self, Reboot.self, Wait.self, Create.self, Delete.self, Fetch.self, Land.self, Screenshot.self, StatsCommand.self, Balloon.self, GuestCommand.self, HarnessCommand.self, TrustCommand.self])

    struct List: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "List VMs and their state.", aliases: ["ls"])
        func run() async throws {
            try await report {
                let list = try await daemonClient().call(Method.vmList).decode(Results.List.self)
                if list.vms.isEmpty {
                    print(remoteDaemon.map { "no VMs on \($0.host)" } ?? "no VMs in \(GlovHome.resolve().path)")
                }
                for vm in list.vms {
                    print(vm.name.padding(toLength: 24, withPad: " ", startingAt: 0) + (vm.engine ?? "").padding(toLength: 6, withPad: " ", startingAt: 0) + vm.state)
                }
            }
        }
    }

    struct Status: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Show one VM's state.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report {
                let info = try await daemonClient().call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self)
                print(describe(info))
                if let config = try? GlovHome.resolve().loadConfig(info.name), config.isolated {
                    let egress = config.egress ?? .init()
                    print("  egress: \(egress.open == true ? "open (any public host, through the proxy)" : "restricted (\(egress.allow.count) allowed)")")
                }
                if let dns = try? GlovHome.resolve().loadConfig(info.name).dns, !dns.isEmpty {
                    print("  dns: \(dns.joined(separator: ", "))")
                }
                if let share = try? GlovHome.resolve().loadConfig(info.name).shares.first(where: { $0.name == HarnessSessions.shareName }) {
                    let used = HarnessSessions.usage(share.hostPath)
                    print("  conversation records: \(share.hostPath) (\(used.map { "\($0.files) files, \(HarnessCommand.Install.size($0.bytes))" } ?? "none yet"))")
                }
            }
        }
    }

    struct Configure: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Reconfigure a stopped VM's CPU count, memory, disk size (only grows) or balloon device, or an isolated VM's egress mode.",
            aliases: ["config"])
        enum State: String, ExpressibleByArgument, CaseIterable { case on, off }
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Option(help: "vCPUs (at least 1).") var cpus: Int?
        @Option(help: "Memory, QEMU -m syntax, e.g. 8G.") var memory: String?
        @Option(help: "Disk size, e.g. 120G (grows the disk, never shrinks).") var diskSize: String?
        @Option(help: "QEMU (Windows): on gives the VM a virtio-balloon device for glov vm balloon (needs glov-qemu); off removes it. A live snapshot taken before the change is refused on revert until the setting is put back.")
        var balloon: State?
        @Option(help: "Isolated VM, running or not: open lets the guest reach any public host, still only through the egress proxy (secret substitution, repository checks and the egress log unchanged; private networks still need glov egress allow --private); restricted goes back to the allowlist. Applies to the next connection. Open gives up the allowlist's limit on where project data can go.")
        var egress: EgressModeOption?
        @Flag(help: "With --egress open: open a VM even though a harness of it holds a secret as a real value in tmpfs, which any host could then receive. Without a terminal this is the only way; with one, glov asks.")
        var allowPlainSecrets = false
        @Option(help: "QEMU (Windows), running or stopped: guest DNS server IPv4/IPv6 address (repeatable or comma-separated, e.g. 1.1.1.1,8.8.8.8), or auto to reset to DHCP (10.0.2.3). Pass on its own.")
        var dns: [String] = []

        func validate() throws {
            if cpus == nil && memory == nil && diskSize == nil && balloon == nil && egress == nil && dns.isEmpty {
                throw ValidationError("pass at least one of --cpus, --memory, --disk-size, --balloon, --egress or --dns")
            }
            if egress != nil, cpus != nil || memory != nil || diskSize != nil || balloon != nil || !dns.isEmpty {
                throw ValidationError("pass --egress on its own: it applies to a running VM, the other options need it stopped")
            }
            if !dns.isEmpty, cpus != nil || memory != nil || diskSize != nil || balloon != nil {
                throw ValidationError("pass --dns on its own: it applies to a running or stopped Windows VM")
            }
            if !dns.isEmpty {
                do { _ = try VMConfig.parseDNS(dns, allowAuto: true) } catch { throw ValidationError("\(error)") }
            }
            if allowPlainSecrets, egress != .open { throw ValidationError("--allow-plain-secrets goes with --egress open") }
            if let cpus, cpus < 1 { throw ValidationError("--cpus must be at least 1") }
            if let memory, VMConfig.bytes(memory) == nil { throw ValidationError("cannot read --memory \(memory)") }
            if let diskSize, VMConfig.bytes(diskSize) == nil { throw ValidationError("cannot read --disk-size \(diskSize)") }
        }

        func run() async throws {
            try await report {
                let client = daemonClient()
                if let egress {
                    let r = try await confirmingPlainSecrets(allow: allowPlainSecrets) { allow in
                        try await client.call(
                            Method.egressMode, Params.EgressMode(vm: vm, open: egress == .open, allowPlainSecrets: allow ? true : nil), as: Results.Egress.self)
                    }
                    print(describeEgressMode(r))
                    return
                }
                if !dns.isEmpty {
                    let servers = try VMConfig.parseDNS(dns, allowAuto: true)
                    let r = try await client.call(Method.vmDNS, Params.DNS(vm: vm, servers: servers), as: Results.DNS.self)
                    let suffix = r.applied ? "" : " (applies on next Guest Service session)"
                    print(r.servers.isEmpty ? "\(r.vm): DNS reset to DHCP (10.0.2.3)\(suffix)" : "\(r.vm): DNS set to \(r.servers.joined(separator: ", "))\(suffix)")
                    return
                }
                let info = try await client.call(
                    Method.vmConfigure, Params.Configure(vm: vm, cpus: cpus, memory: memory, diskSize: diskSize, balloon: balloon.map { $0 == .on }),
                    as: Results.VMInfo.self)
                print(describe(info))
            }
        }
    }

    struct Balloon: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Show or set how much memory a Windows (QEMU) VM keeps; the rest goes back to macOS.",
            discussion: """
                auto lets glovd choose from what the guest uses: that plus 2G or half of it as headroom, 1G or a quarter \
                while the Mac is under memory pressure, at least 1G or a quarter of the VM's memory; a guest that runs \
                short gets all its memory back at once, and an auto VM starts with all of it. A size fixes the target \
                instead and turns auto off; give the VM's full memory to return it all. The choice is kept in vm.json and \
                applied whenever the VM starts, so it can be made while it is stopped. The guest gives memory up \
                gradually, and Windows stops early when it runs short, so check again with neither. Needs glov-qemu (brew \
                install akunzai/tap/glov-qemu): other QEMU builds keep the memory on macOS. A VM made before this existed \
                needs glov vm configure <vm> --balloon on while stopped, and auto needs the guest's balloon service, which \
                reports its memory use.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "auto, or the memory the guest keeps in QEMU -m syntax, e.g. 2G. Omit to show the current amount.") var size: String?

        func validate() throws {
            if let size, size != "auto", VMConfig.bytes(size) == nil { throw ValidationError("cannot read size \(size)") }
        }

        func run() async throws {
            try await report {
                let params = size == "auto" ? Params.Balloon(vm: vm, auto: true) : Params.Balloon(vm: vm, size: size)
                let r = try await daemonClient().call(Method.vmBalloon, params, as: Results.Balloon.self)
                print(Self.describe(r))
            }
        }

        static func describe(_ r: Results.Balloon) -> String {
            func g(_ b: UInt64) -> String { String(format: "%.2fG", Double(b) / Double(1 << 30)) }
            let target = r.target ?? r.configured
            let mode = r.mode == "auto" ? "auto" : "fixed"
            guard let memory = r.memory else {
                if r.mode == "auto" { return "\(r.vm): stopped; auto, starts with all \(g(r.configured)) and shrinks once the guest reports its use" }
                return "\(r.vm): stopped; fixed, target \(g(target)) of \(g(r.configured)) when it starts"
            }
            var line = "\(r.vm): guest has \(g(memory)) of \(g(r.configured)), \(mode) target \(g(target))"
            // The driver hands memory over in pages, so a guest that got there can stay a little above.
            if memory > target + (64 << 20) { line += " (the guest gets there gradually)" }
            if let reason = r.reason { line += "\n  \(reason)" }
            return line
        }
    }

    struct Start: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Start a VM (or reattach if its qemu is already running). VZ VMs run inside glovd.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report {
                let info = try await daemonClient().call(Method.vmStart, Params.VM(vm: vm), as: Results.VMInfo.self)
                if let warning = info.warning { FileHandle.standardError.write(Data((warning + "\n").utf8)) }
                print(describe(info))
            }
        }
    }

    struct Stop: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Power a VM down, waiting up to 60 s before forcing it.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Flag(help: "Skip the graceful shutdown.") var force = false
        func run() async throws {
            // A project VM's branches first, for glov vm delete; a hung guest, which --force is for, would only make it wait.
            if !force { await ProjectVM.fetchBeforeStop(vm) }
            try await report { print(describe(try await daemonClient().call(Method.vmStop, Params.Stop(vm: vm, force: force), as: Results.VMInfo.self))) }
        }
    }

    struct Reboot: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Restart a VM and wait until the new boot's Guest Service is ready.",
            discussion: """
                Returns, like vm status, only once a Guest Service answers with a bootId other than the one before the reboot, \
                so the old boot is never taken for the new one. A Windows VM restarts through its Guest Service (a real restart, \
                never a Fast Startup shutdown); one whose Guest Service is not ready is refused: restart it with `glov vm stop` \
                (--force if it does not stop) and `glov vm start`. A Linux VM is stopped as `glov vm stop` does and started again. \
                Refused while exec jobs, a share sync or a share mount run (they are named) unless --force. On timeout it reports the VM and Guest Service states \
                and never forces anything: look at `glov vm screenshot <vm>` before `glov vm stop --force <vm>`.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Option(help: "Seconds to wait for the new boot; the default covers a restart that installs Windows updates.") var timeout: Double = Params.Reboot.defaultTimeout
        @Flag(help: "Interrupt running exec jobs, share syncs and share mounts instead of refusing.") var force = false

        func validate() throws {
            guard timeout.isFinite, timeout > 0 else { throw ValidationError("--timeout must be a positive number of seconds") }
        }

        func run() async throws {
            try await report {
                let info = try await daemonClient().call(Method.vmReboot, Params.Reboot(vm: vm, timeout: timeout, force: force), as: Results.VMInfo.self)
                print(describe(info) + (info.agent?.bootId.map { "\n  boot \($0)" } ?? ""))
            }
        }
    }

    struct Create: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Create <home>/<name>. Refuses to overwrite an existing VM.",
            discussion: """
                QEMU (default), Windows: without --disk, prepares a VM that installs Windows 11 Arm64 unattended on its first \
                `vm start`: a thin qcow2 disk (--disk-size, default 200G), UEFI variables, TPM state and an unattend image with \
                --guest-service (a Windows arm64 glov-guest.exe), for an Administrators account glov with AutoLogon and a random \
                password in <vm>/secrets/. The install ISO for --language and --edition and virtio-win come from the cache \
                `glov host media` fills (downloaded first when missing). The daemon boots the installer, `vm status` shows \
                installing until the Guest Service is ready and Setup is done (10 to 15 minutes), then the install drives are \
                detached; after 60 minutes the install fails. An install is not resumed: delete the VM and create it again. \
                --ssh also installs OpenSSH (key only, key in <vm>/secrets/id_ed25519) and forwards --ssh-port (default: a free \
                port) to the guest's port 22. With --disk, writes vm.json for an existing qcow2 disk and --firmware-code.
                VZ (--engine vz, Linux): converts --image (a raw or qcow2 arm64 cloud image; qcow2 without a backing file or zstd compression) to a raw disk.img grown to --disk-size, \
                generates the SSH key secrets/id_ed25519 for user glov and a cloud-init seed (seed.iso) that installs \
                --guest-service on first boot. Without --image it downloads and verifies --distro (ubuntu by default, or debian) \
                and caches it under $GLOV_CACHE/<distro> (default ~/Library/Caches/glov/<distro>). \
                --isolated attaches no network device: the guest reaches only the hosts listed in vm.json's egress.allow, through \
                an HTTP proxy at 127.0.0.1:3128 in the guest (docs/networking.md); the list starts empty. --harness claude, codex or \
                antigravity sets that coding agent up and allows the hosts it needs to run; vm create lists the secrets it uses (its API key \
                among them), each to define with glov secret set and grant with glov secret grant.
                """)
        @Argument(help: "New VM name.") var name: String
        @Option(help: "qemu (Windows, default) or vz (Linux on Virtualization.framework).") var engine: VMConfig.Engine = .qemu
        @Option(help: "QEMU: UEFI firmware code (default for a Windows install: edk2-aarch64-code.fd from Homebrew's QEMU).") var firmwareCode: String?
        @Option(help: "QEMU, with --disk: UEFI variable store (default: <vm>/vars.fd, created blank).") var firmwareVars: String?
        @Option(help: "vCPUs (default: \(WindowsProvisioner.defaultCPUs) for a Windows install, else 2).") var cpus: Int?
        @Option(help: "Memory, QEMU -m syntax (default: \(WindowsProvisioner.defaultMemory) for a Windows install, else 4G).") var memory: String?
        @Option(help: "QEMU: register an existing qcow2 system disk (attached as NVMe) instead of installing Windows.") var disk: String?
        @Flag(help: "QEMU, with --disk: do not attach a software TPM.") var noTpm = false
        @Flag(help: "QEMU, Windows install: install OpenSSH (key only) and forward a host port to guest port 22.") var ssh = false
        @Option(help: "QEMU: host port forwarded to guest port 22 (Windows install: with --ssh, default a free port).") var sshPort: Int?
        @Option(help: "QEMU: host port on 127.0.0.1 forwarded to a guest TCP port: HOST:GUEST. Repeatable.") var forward: [String] = []
        @Option(help: "QEMU: extra USB drive: path[,cdrom][,removable][,bootindex=N]. Repeatable.") var drive: [String] = []
        @Option(help: "QEMU (Windows): guest DNS server IPv4/IPv6 address (repeatable or comma-separated, e.g. 1.1.1.1,8.8.8.8); bypasses QEMU slirp's 10.0.2.3 when the Mac is on a VPN.")
        var dns: [String] = []
        @Option(help: "QEMU, Windows install: language of the install media, e.g. en-US or zh-TW (default \(WindowsProvisioner.defaultLanguage)).") var language: String?
        @Option(help: "QEMU, Windows install: Windows time zone ID, e.g. \"Taipei Standard Time\" (default: the host's, from Unicode CLDR's mapping; Setup's own default when the host's has none).") var timeZone: String?
        @Option(help: "QEMU, Windows install: \(WindowsAssets.editions.keys.sorted().joined(separator: " or ")) (default \(WindowsProvisioner.defaultEdition)).") var edition: String?
        @Option(help: "VZ: Linux distribution to download and cache: ubuntu (default) or debian.") var distro: LinuxDistro?
        @Flag(help: "VZ: no network device; egress only through the allowlist proxy (egress.allow in vm.json, empty at first).") var isolated = false
        @Option(help: "VZ, with --isolated: set up a coding-agent harness: \(HarnessProfile.names). Repeatable.") var harness: [String] = []
        @Option(help: "With --isolated: restricted (the default: only the allowlist) or open (any public host, still only through the egress proxy); change it later with vm configure --egress.")
        var egress: EgressModeOption?
        @Option(help: "VZ: cloud image to install from (qcow2 or raw, arm64; default: downloaded for --distro).") var image: String?
        @Option(help: "Disk size, e.g. 200G (default: \(WindowsProvisioner.defaultDiskSize) for a Windows install, 32G for VZ; VZ grows the image to it, never shrinks).") var diskSize: String?
        @Option(name: .customLong("guest-service"), help: "Custom Windows/Linux arm64 Guest Service binary (default: the bundled build).") var agent: String?
        @Option(help: "A share, as for glov share add: NAME,HOST_PATH,GUEST_PATH[,ro][,exclude=PATTERN]... (VZ: GUEST_PATH optional, default /mnt/NAME, no exclude). Repeatable.") var share: [String] = []
        @Flag(help: "Only create the VM: do not start it and wait for its Guest Service (a Windows install then runs on the first vm start).")
        var noStart = false
        @Flag(name: .customLong("no-sessions"), help: "With --harness: do not keep the harnesses' conversation records on this Mac (<glov home>/.sessions/<vm>).") var noSessions = false

        /// A new Windows install rather than a registration (`--disk`) or a VZ VM.
        var installsWindows: Bool { engine == .qemu && disk == nil }

        func validate() throws {
            if !harness.isEmpty && !isolated { throw ValidationError("--harness needs --isolated: its API key placeholder is replaced only by the egress proxy of an isolated VM") }
            if egress != nil && !isolated { throw ValidationError("--egress needs --isolated: only an isolated VM goes out through the egress proxy") }
            do { _ = try harnesses() } catch { throw ValidationError("\(error)") }
            if let diskSize, VMConfig.bytes(diskSize) == nil { throw ValidationError("cannot read --disk-size \(diskSize)") }
            switch engine {
            case .qemu where disk != nil:
                if firmwareCode == nil { throw ValidationError("--firmware-code is required with --disk") }
                if image != nil || distro != nil || isolated { throw ValidationError("--image, --distro and --isolated are for --engine vz") }
                if agent != nil || ssh || language != nil || edition != nil || timeZone != nil || diskSize != nil {
                    throw ValidationError("--guest-service, --ssh, --language, --edition, --time-zone and --disk-size are for a new Windows install, without --disk")
                }
                if !dns.isEmpty {
                    do { _ = try VMConfig.parseDNS(dns, allowAuto: false) } catch { throw ValidationError("\(error)") }
                }
            case .qemu:
                if image != nil || distro != nil || isolated { throw ValidationError("--image, --distro and --isolated are for --engine vz") }
                if noTpm { throw ValidationError("--no-tpm: Windows 11 Setup needs a TPM") }
                if firmwareVars != nil { throw ValidationError("--firmware-vars is for --disk: a Windows install starts from a blank store") }
                if sshPort != nil && !ssh { throw ValidationError("--ssh-port needs --ssh") }
                if let edition {
                    do { _ = try WindowsAssets.edition(edition) } catch { throw ValidationError("\(error)") }
                }
                if let timeZone {
                    do { _ = try WindowsAssets.timeZone(requested: timeZone, hostIdentifier: "") } catch { throw ValidationError("\(error)") }
                }
                if !dns.isEmpty {
                    do { _ = try VMConfig.parseDNS(dns, allowAuto: false) } catch { throw ValidationError("\(error)") }
                }
            case .vz:
                if distro != nil && image != nil { throw ValidationError("pass either --distro or --image, not both") }
                if firmwareCode != nil || firmwareVars != nil || disk != nil || sshPort != nil || !forward.isEmpty || noTpm || !drive.isEmpty
                    || ssh || language != nil || edition != nil || timeZone != nil || !dns.isEmpty
                {
                    throw ValidationError("--firmware-code, --firmware-vars, --disk, --no-tpm, --ssh, --ssh-port, --forward, --drive, --language, --edition, --time-zone and --dns are for --engine qemu")
                }
            }
            if let sshPort, !(1...65535).contains(sshPort) { throw ValidationError("--ssh-port must be 1-65535") }
            do { _ = try shares() } catch { throw ValidationError("\(error)") }
        }

        private func harnesses() throws -> [HarnessProfile] {
            var seen: Set<String> = []
            return try harness.filter { seen.insert($0).inserted }.map { try HarnessProfile.named($0) }
        }

        /// The --share specs, checked (before anything is downloaded or created).
        private func shares() throws -> [VMConfig.Share] {
            let home = GlovHome.resolve()
            var probe = VMConfig(name: name, directory: home.directory(of: name), firmwareCode: "")
            for spec in share {
                let s = try home.checkShare(VMConfig.Share(spec: spec, directory: FileManager.default.currentDirectoryPath), engine: engine)
                probe = try probe.adding(s)
            }
            return probe.shares
        }

        private func addShares(in home: GlovHome) throws {
            for s in try shares() { try home.addShare(vm: name, s) }
        }

        func run() async throws {
            try await report {
                let home = GlovHome.resolve()
                guard try await make(home), !noStart else { return }
                // A new VM is of no use until its Guest Service answers, so creating one starts it and waits by default.
                // The first boot of a Windows install runs Setup (10 to 15 minutes, failed after 60).
                _ = try await daemonClient().call(Method.vmStart, Params.VM(vm: name), as: Results.VMInfo.self)
                try await Wait.parse([name, "guest-ready", "--timeout", installsWindows ? "3600" : "900"]).run()
            }
        }

        /// Creates the VM; true when it is one to start and wait for (a Linux VM or a Windows install, not a registered disk).
        private func make(_ home: GlovHome) async throws -> Bool {
            if engine == .vz || installsWindows {
                // Before downloading anything, so a bad or taken name fails fast.
                guard GlovHome.isValid(name: name) else { throw HomeError.invalidName(name) }
                guard !FileManager.default.fileExists(atPath: home.directory(of: name)) else { throw HomeError.exists(name) }
            }
            if engine == .vz {
                // Provisioning downloads and waits for tools, so it runs off the cooperative pool.
                let config = try await Blocking.run { [self] in
                    let guestService = try agent ?? GuestServiceDistribution.target(os: "linux").path
                    var provisioner = LinuxProvisioner(
                        name: name, image: try image ?? (distro ?? .ubuntu).image(), cpus: cpus ?? 2, memory: memory ?? "4G",
                        diskSize: VMConfig.bytes(diskSize ?? "32G") ?? 0, guestServiceBinary: guestService)
                    provisioner.isolated = isolated
                    provisioner.harnesses = try harnesses()
                    return try provisioner.create(in: home)
                }
                try addShares(in: home)
                // A new VM has no grants yet, so nothing it holds needs confirming.
                if egress == .open {
                    _ = try home.update(name) { c in
                        var e = c.egress ?? .init()
                        e.open = true
                        c.egress = e
                    }
                }
                print("created \(config.directory)/vm.json (engine vz\(isolated ? ", isolated" : "")\(egress == .open ? ", egress open" : ""))")
                if isolated, !harness.isEmpty, !noSessions {
                    let records = HarnessSessions.share(home: home.path, vm: name)
                    for dir in [(records.hostPath as NSString).deletingLastPathComponent, records.hostPath] {
                        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
                    }
                    try home.addShare(vm: name, records)
                    print("conversation records: \(records.hostPath) (for review on this Mac; never resume them there)")
                }
                if isolated, egress == .open {
                    print("isolated: no network device; egress open: any public host through the egress proxy (vm configure --egress restricted for the allowlist)")
                } else if isolated {
                    let allow = config.egress?.allow ?? []
                    print("isolated: no network device; egress.allow in vm.json lists the allowed hosts (\(allow.isEmpty ? "empty: nothing is allowed" : allow.joined(separator: ", ")))")
                }
                let profiles = try harnesses()
                if profiles.contains(where: { !$0.secrets.isEmpty }) {
                    let defined = (try? await daemonClient().call(Method.secretList, Params.Secret(), as: Results.Secrets.self)).map { Set($0.secrets.map(\.name)) }
                    for p in profiles { for line in p.secretNeeds(vm: name, defined: defined) { print(line) } }
                }
                print("ssh key: \(LinuxProvisioner.privateKeyPath(config.directory)) (user \(CloudInitSeed.user)); boot log: \(config.consoleLogPath)")
                return true
            }
            let forwards = try forward.map { try VMConfig.PortForward(spec: $0) }
            let drives = try drive.map { try VMConfig.Drive(spec: $0) }
            let dnsServers = try dns.isEmpty ? [] : VMConfig.parseDNS(dns, allowAuto: false)
            if installsWindows {
                try await Blocking.run { [self] in try createWindows(in: home, forwards: forwards, drives: drives, dnsServers: dnsServers) }
                try addShares(in: home)
                return true
            }
            let config = VMConfig(
                name: name, directory: home.directory(of: name), cpus: cpus ?? 2, memory: memory ?? "4G", disk: disk,
                firmwareCode: firmwareCode ?? "", firmwareVars: firmwareVars, tpm: !noTpm, sshPort: sshPort,
                portForwards: forwards, extraDrives: drives, macAddress: VMConfig.randomMACAddress(),
                uuid: UUID().uuidString.lowercased(), diskSerial: VMConfig.randomDiskSerial(),
                dns: dnsServers.isEmpty ? nil : dnsServers)
            try home.create(config)
            try addShares(in: home)
            print("created \(config.directory)/vm.json")
            return false
        }

        private func createWindows(in home: GlovHome, forwards: [VMConfig.PortForward], drives: [VMConfig.Drive], dnsServers: [String]) throws {
            let firmware = try WindowsProvisioner.checkHostTools(firmwareCode: firmwareCode)
            let guestService = try agent ?? GuestServiceDistribution.target(os: "windows").path
            try WindowsProvisioner.checkGuestServiceBinary(guestService)
            let language = WindowsAssets.languageTag(language ?? WindowsProvisioner.defaultLanguage)
            let media = WindowsMedia()
            let esd = try media.catalog().select(language: language, edition: edition ?? WindowsProvisioner.defaultEdition)
            let iso = try media.makeISO(esd)
            let virtio = try media.virtioISO()
            var p = WindowsProvisioner(
                name: name, iso: iso, virtio: virtio, guestServiceBinary: guestService, firmwareCode: firmware)
            p.language = language
            p.edition = esd.edition
            p.timeZone = try WindowsAssets.timeZone(requested: timeZone, hostIdentifier: TimeZone.current.identifier)
            if let cpus { p.cpus = cpus }
            if let memory { p.memory = memory }
            if let diskSize, let bytes = VMConfig.bytes(diskSize) { p.diskSize = bytes }
            p.sshPort = ssh ? try sshPort ?? WindowsProvisioner.freePort() : nil
            p.portForwards = forwards
            p.extraDrives = drives
            p.dns = dnsServers
            let config = try p.create(in: home)
            print("created \(config.directory)/vm.json: Windows 11 \(esd.edition) (\(language)) installs on the first start")
            if !dnsServers.isEmpty { print("dns: \(dnsServers.joined(separator: ", "))") }
            if let zone = p.timeZone {
                print("time zone: \(zone)")
            } else {
                print("time zone: the host's (\(TimeZone.current.identifier)) has no Windows mapping, so Setup's default (Pacific) applies; pass --time-zone to set one")
            }
            if noStart {
                print("next: glov vm start \(name), then glov vm wait \(name) guest-ready --timeout 3600, which returns once the install is done (vm status says installing until then)")
            }
            if let port = config.sshPort {
                print("ssh: ssh -i \(WindowsProvisioner.privateKeyPath(config.directory)) -p \(port) glov@127.0.0.1 (once the install is done)")
            }
        }
    }

    struct Delete: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Delete a stopped VM and everything in its directory: disk, UEFI variables, TPM state, secrets and snapshots.",
            discussion: """
                Asks for confirmation unless --yes is given. The name must be exact. Shared media in the cache are kept. For a \
                project's VM in git mode (glov claude), glov first lists the guest's fetched commits that no local branch has \
                and asks again (--yes deletes them without a terminal); a stopped VM may hold commits since the last fetch, so \
                in a terminal glov offers to start it, fetch and stop it first. Then it removes the glov-<vm> remote, its land \
                state and its SSH host.
                """,
            aliases: ["rm"])
        @Argument(help: "VM name (exact).") var vm: String
        @Flag(name: .shortAndLong, help: "Do not ask for confirmation.") var yes = false

        func run() async throws {
            try await report {
                let home = GlovHome.resolve()
                let records = (try? home.loadConfig(vm))?.shares.first { $0.name == HarnessSessions.shareName }?.hostPath
                let project = try await ProjectVM.checkBeforeDelete(vm, yes: yes)
                let outcome = try await VMDeletion.delete(
                    vm, in: home, isRunning: { try await VMDeletion.isRunning($0, in: home) },
                    confirm: { question in
                        if yes { return true }
                        FileHandle.standardError.write(Data(question.utf8))
                        return VMDeletion.isYes(readLine())
                    })
                if outcome == .declined {
                    FileHandle.standardError.write(Data("not deleted\n".utf8))
                    throw ExitCode.failure
                }
                print("deleted \(home.directory(of: vm))")
                if let project {
                    await project.forget()
                    print("removed the \(project.remote) remote from \(project.dir)")
                }
                if let line = HerdrTool.forget(vm) { print(line) }
                if let records, FileManager.default.fileExists(atPath: records) { print("kept its conversation records at \(records); remove them yourself when no longer needed") }
            }
        }
    }

    struct Wait: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Wait until a VM is running, or its Guest Service answers (guest-ready).")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "running or guest-ready.") var state = "running"
        @Option(help: "Seconds to wait.") var timeout: Double = 60
        func run() async throws {
            try await report {
                let client = daemonClient()
                // Say what it waits for and each change on the way (stderr), so a wait of minutes does not look hung; the
                // daemon still decides when it is done and what a failure says (stdout and the exit code are as before).
                note("waiting for \(vm) to be \(state) (up to \(duration(Int(timeout))))")
                let started = ContinuousClock.now
                var last = ""
                var lastNote = started
                while true {
                    let left = timeout - Double((ContinuousClock.now - started).components.seconds)
                    do {
                        let done = try await client.call(Method.vmWait, Params.Wait(vm: vm, state: state, timeout: max(0.1, min(left, 5))), as: Results.VMInfo.self)
                        print(describe(done))
                        return
                    } catch let e as RPCError where e.message.hasPrefix("timed out") && left > 5 {
                        let info = try await client.call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self)
                        let now = WaitProgress.line(info)
                        let elapsed = Int((ContinuousClock.now - started).components.seconds)
                        if now != last {
                            note("\(now) (\(duration(elapsed)))")
                            last = now
                            lastNote = ContinuousClock.now
                        } else if ContinuousClock.now - lastNote >= .seconds(60) {
                            note("still \(now) (\(duration(elapsed)))")
                            lastNote = ContinuousClock.now
                        }
                    }
                }
            }
        }

        private func note(_ line: String) { FileHandle.standardError.write(Data((line + "\n").utf8)) }
    }
}

extension VMConfig.Engine: ExpressibleByArgument {}
extension VMConfig.Project.Sync: ExpressibleByArgument {}
extension LinuxDistro: ExpressibleByArgument {}

/// "6m 05s", "45s".
func duration(_ seconds: Int) -> String {
    seconds < 60 ? "\(seconds)s" : "\(seconds / 60)m" + (seconds % 60 == 0 ? "" : String(format: " %02ds", seconds % 60))
}

func describe(_ vm: Results.VMInfo) -> String {
    vm.name + ": " + vm.state + (vm.engine.map { " [\($0)]" } ?? "") + (vm.pid.map { " (pid \($0))" } ?? "") + (vm.agent.map { ", Guest Service " + $0.state + ($0.problem.map { " (" + $0 + ")" } ?? "") } ?? "")
        + (vm.agent?.update.map { "\n  " + $0.description } ?? "")
        + (vm.installFailure.map { ": " + $0 } ?? "")
        + (vm.install.map { "\n  install: \($0.phase), \(duration($0.elapsedSeconds)) so far (it takes 10 to 15 minutes, at most \(duration($0.limitSeconds)))"
            + ($0.overdue ? "; \(WaitProgress.overdueHint(vm.name))" : "") } ?? "")
        + (vm.agent?.shares ?? []).map { "\n  share \($0.name) at \($0.guestPath): \($0.state)" + ($0.problem.map { " (" + $0 + ")" } ?? "") }.joined()
}

struct Screenshot: AsyncParsableCommand {
    static let configuration = CommandConfiguration(abstract: "Save a PNG screenshot of a running VM.")
    @Argument(help: "VM name or unique prefix.") var vm: String
    @Option(name: .shortAndLong, help: "Output file.") var output: String
    @Option(help: "Scale down to this width (default 1280).") var maxWidth: Int?

    func run() async throws {
        try await report {
            let shot = try await daemonClient().call(
                Method.screenshot, Params.Screenshot(vm: vm, maxWidth: maxWidth), as: Results.Screenshot.self)
            guard let png = Data(base64Encoded: shot.png) else { throw DaemonClientError.badReply("screenshot is not base64") }
            try png.write(to: URL(fileURLWithPath: output))
            print("\(output): \(shot.width)x\(shot.height)")
        }
    }
}
