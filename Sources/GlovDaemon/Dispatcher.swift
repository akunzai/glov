import Foundation
import GlovCore
import GlovDaemonAPI

/// A FIFO mutex for async code: snapshot operations, start and stop of one VM never overlap.
actor AsyncMutex {
    private var locked = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func lock() async {
        if !locked { locked = true; return }
        await withCheckedContinuation { waiters.append($0) }
    }

    func unlock() {
        if waiters.isEmpty { locked = false } else { waiters.removeFirst().resume() }
    }
}

/// Owns the VMs (QEMU `VirtualMachine` and in-process `VZMachine` actors) and answers JSON-RPC requests.
/// A client going away never touches a VM.
public actor Dispatcher {
    private let instanceID = UUID().uuidString
    private let home: GlovHome
    private let guestServiceDirectory: String
    private var machines: [String: any Machine] = [:]
    private var consoles: [String: Console] = [:]
    private var agents: [String: VMGuestService] = [:]
    private var locks: [String: AsyncMutex] = [:]
    private var installs: [String: Task<Void, Never>] = [:]
    /// QEMU VMs' balloon targets (`BalloonController`), from start to stop.
    private var balloons: [String: BalloonController] = [:]
    private let installTiming: InstallTiming
    /// The owner's named secrets (`glov secret`) and their values, for the egress proxy.
    let secrets: SecretBroker
    private var egressTLSCache: EgressTLS?
    /// Starts a VZ VM again during `vm.reboot` in place of `startVM` (tests, which cannot boot one).
    private var restartForTesting: (@Sendable (String) async throws -> Void)?

    public init(home: GlovHome, installTiming: InstallTiming = .windows, guestServiceDirectory: String = GuestServiceDistribution.directory) {
        self.init(
            home: home, installTiming: installTiming, guestServiceDirectory: guestServiceDirectory,
            secrets: SecretBroker(home: home, values: KeychainValues()))
    }

    init(
        home: GlovHome, installTiming: InstallTiming = .windows, guestServiceDirectory: String = GuestServiceDistribution.directory,
        secrets: SecretBroker? = nil
    ) {
        self.secrets = secrets ?? SecretBroker(home: home, values: MemoryValues())
        self.guestServiceDirectory = guestServiceDirectory
        self.home = home
        self.installTiming = installTiming
    }

    /// Reattaches to every VM whose qemu survived a daemon restart. Returns the names attached.
    public func attachRunning() async -> [String] {
        var attached: [String] = []
        for name in home.vmNames() {
            do {
                let config = try home.loadConfig(name)
                guard config.engine == .qemu else { continue }  // a VZ guest ran inside the previous daemon and ended with it
                let vm = try VirtualMachine(config: config)
                if try await vm.attach() {
                    machines[name] = vm
                    attached.append(name)
                    await startGuestService(vm.config)
                    await startBalloon(vm, config, resuming: true)
                    // The Mac may have slept while no daemon watched for the wake: set the clock once the Guest Service answers.
                    await agents[name]?.expectTimeSync()
                    if config.install?.started != nil && config.install?.failure == nil { watchInstall(name, typeBoot: false) }
                }
            } catch { DaemonLog.write("cannot reattach \(name): \(error)") }
        }
        return attached
    }

    public func handle(_ request: RPCRequest) async -> RPCResponse {
        let id = request.id ?? .null
        do { return RPCResponse(id: id, result: try await call(request.method, request.params ?? [:])) } catch let e as RPCError {
            return RPCResponse(id: id, error: e)
        } catch {
            let message = error is CocoaError ? error.localizedDescription : "\(error)"
            if case GuestServiceError.remote(GuestServiceError.noUserCode, _) = error { return RPCResponse(id: id, error: RPCError(code: RPCError.noUser, message: message)) }
            return RPCResponse(id: id, error: RPCError(code: RPCError.failed, message: message))
        }
    }

    /// The harness profiles a VM's `harnesses` name.
    private var harnessProfiles = HarnessProfile.all
    func setHarnessProfilesForTesting(_ profiles: [HarnessProfile]) { harnessProfiles = profiles }
    func attachMachineForTesting(_ machine: any Machine) { machines[machine.config.name] = machine }

    func guestServiceForTesting(_ name: String) -> VMGuestService? { agents[name] }

    /// Every guest's clock may have stood still while the Mac slept.
    public func hostWoke() async {
        for agent in agents.values { await agent.hostWoke() }
    }

    func setRestartForTesting(_ restart: @escaping @Sendable (String) async throws -> Void) { restartForTesting = restart }

    private func call(_ method: String, _ params: JSONValue) async throws -> JSONValue {
        switch method {
        case Method.daemonInfo:
            return try JSONValue(encoding: Results.DaemonInfo(protocolVersion: DaemonProtocol.version, instanceID: instanceID))
        case Method.vmList:
            var vms: [Results.VMInfo] = []
            for name in home.vmNames() { vms.append(await info(name)) }
            return try JSONValue(encoding: Results.List(vms: vms))
        case Method.vmStatus:
            return try JSONValue(encoding: await info(try resolve(params.decode(Params.VM.self).vm)))
        case Method.vmConfigure:
            let p = try params.decode(Params.Configure.self)
            let name = try resolve(p.vm)
            return try await exclusive(name) {
                try await self.configureVM(name, cpus: p.cpus, memory: p.memory, diskSize: p.diskSize, balloon: p.balloon, requireChange: true)
                return try JSONValue(encoding: await self.info(name))
            }
        case Method.vmDNS:
            let p = try params.decode(Params.DNS.self)
            let name = try resolve(p.vm)
            return try await exclusive(name) {
                try JSONValue(encoding: try await self.setDNS(name, servers: p.servers))
            }
        case Method.vmBalloon:
            let p = try params.decode(Params.Balloon.self)
            let name = try resolve(p.vm)
            // A change waits for a start or stop under way; a report does not.
            guard p.size != nil || p.auto == true else { return try JSONValue(encoding: try await balloon(name, size: nil, auto: false)) }
            return try await exclusive(name) { try JSONValue(encoding: try await self.balloon(name, size: p.size, auto: p.auto == true)) }
        case Method.guestServiceCheck:
            let p = try params.decode(Params.GuestService.self)
            let name = try resolve(p.vm)
            try requireControl(name)
            let current = await info(name)
            do { _ = try GuestServiceUpdate.requireReadyGuestService(current) } catch {
                return try JSONValue(encoding: Results.GuestServiceCheck(state: "unknown", currentVersion: current.agent?.version, targetVersion: current.agent?.update?.targetVersion, problem: "\(error)"))
            }
            return try JSONValue(encoding: await agent(name).1.checkGuestService(binary: p.binary))
        case Method.guestServiceUpdate:
            let p = try params.decode(Params.GuestService.self)
            let name = try resolve(p.vm)
            try requireControl(name)
            return try await exclusive(name) {
                _ = try GuestServiceUpdate.requireReadyGuestService(await self.info(name))
                let a = try await self.agent(name).1
                return try JSONValue(encoding: await a.updateGuestService(p, home: self.home))
            }
        case Method.vmStart:
            let name = try resolve(params.decode(Params.VM.self).vm)
            return try await exclusive(name) {
                let warning = await self.startWarning(name)
                try await self.startVM(name)
                var started = await self.info(name)
                started.warning = warning
                return try JSONValue(encoding: started)
            }
        case Method.egressList:
            let name = try resolve(params.decode(Params.Egress.self).vm)
            return try JSONValue(encoding: Self.egressResult(name, try isolatedConfig(name)))
        case Method.egressAllow, Method.egressRemove:
            let p = try params.decode(Params.Egress.self)
            let name = try resolve(p.vm)
            guard let hosts = p.hosts, !hosts.isEmpty else { throw RPCError(code: RPCError.invalidParams, message: "hosts: name at least one host") }
            let allow = method == Method.egressAllow
            return try JSONValue(encoding: try await editEgress(name) { egress in
                if p.mcp == true, egress.mcpEdits != true {
                    throw RPCError(code: RPCError.failed, message: "VM \(name) does not let MCP clients change its egress allowlist: ask the user to run glov egress \(allow ? "allow" : "remove") \(name) ..., or to opt in with glov egress mcp \(name) on")
                }
                if p.private == true, p.mcp == true {
                    throw RPCError(code: RPCError.failed, message: "MCP clients cannot allow private-network hosts: ask the user to run glov egress allow \(name) <host> --private")
                }
                egress = try allow ? egress.allowing(hosts, privateNetworks: p.private == true) : egress.removing(hosts)
            })
        case Method.egressMCP:
            let p = try params.decode(Params.EgressMCP.self)
            return try JSONValue(encoding: try await editEgress(try resolve(p.vm)) { $0.mcpEdits = p.enabled ? true : nil })
        case Method.egressMode:
            let p = try params.decode(Params.EgressMode.self)
            let name = try resolve(p.vm)
            let declared = try tmpfsDeclared(home.loadConfig(name))
            let result = try await editEgress(name) { egress in
                if p.open {
                    let plain = (egress.secrets ?? []).filter(declared.contains)
                    if !plain.isEmpty, p.allowPlainSecrets != true {
                        throw Self.plainSecretsRefusal(name, plain, retry: "glov vm configure \(name) --egress open --allow-plain-secrets")
                    }
                    egress.open = true
                    egress.plainSecrets = plain.isEmpty ? nil : plain
                } else {
                    egress.open = nil
                    egress.plainSecrets = nil
                }
            }
            try await resyncSecrets([name])
            return try JSONValue(encoding: result)
        case Method.trustList:
            let name = try resolve(params.decode(Params.Trust.self).vm)
            return try JSONValue(encoding: Self.trustResult(name, try isolatedConfig(name)))
        case Method.trustAdd, Method.trustRemove:
            let p = try params.decode(Params.Trust.self)
            let name = try resolve(p.vm)
            guard let label = p.name else { throw RPCError(code: RPCError.invalidParams, message: "name: name the certificate") }
            let added: VMConfig.Egress.Trust?
            if method == Method.trustAdd {
                guard let pem = p.certificate else { throw RPCError(code: RPCError.invalidParams, message: "certificate: give the PEM text") }
                do { added = try TrustedCertificate.make(name: label, pem: pem) } catch { throw RPCError(code: RPCError.failed, message: error.description) }
            } else { added = nil }
            _ = try await editEgress(name) { egress in
                var list = egress.trust ?? []
                if let added {
                    if let i = list.firstIndex(where: { $0.name == added.name }) { list[i] = added } else { list.append(added) }
                } else {
                    guard list.contains(where: { $0.name == label }) else {
                        throw RPCError(code: RPCError.failed, message: "VM \(name) has no trusted certificate named \(label): see glov vm trust list \(name)")
                    }
                    list.removeAll { $0.name == label }
                }
                egress.trust = list.isEmpty ? nil : list
            }
            if let agent = agents[name], let problem = await agent.syncTrust() {
                throw RPCError(code: RPCError.failed, message: "saved, but \(problem); the next Guest Service session retries")
            }
            return try JSONValue(encoding: Self.trustResult(name, try home.loadConfig(name)))
        case Method.secretSet, Method.secretSource:
            let p = try params.decode(Params.Secret.self)
            guard let name = p.name else { throw RPCError(code: RPCError.invalidParams, message: "name: name the secret") }
            var forge: Forge?
            if let f = p.forge {
                guard let parsed = Forge(rawValue: f) else {
                    throw RPCError(code: RPCError.invalidParams, message: "forge \(f): use \(Forge.allCases.map(\.rawValue).joined(separator: " or "))")
                }
                forge = parsed
            }
            if method == Method.secretSet {
                try await secrets.set(name, hosts: p.hosts ?? [], value: p.value ?? "", forge: forge)
            } else {
                try await secrets.source(name, hosts: p.hosts ?? [], command: p.command ?? [], forge: forge)
            }
            try await resyncSecrets(home.vmNames().filter { (try? home.loadConfig($0))?.egress?.secrets?.contains(name) == true })
            return try JSONValue(encoding: try await secrets.definition(name)?.summary)
        case Method.secretList:
            return try JSONValue(encoding: Results.Secrets(secrets: try await secrets.list().map(\.summary)))
        case Method.secretRemove:
            let p = try params.decode(Params.Secret.self)
            guard let name = p.name else { throw RPCError(code: RPCError.invalidParams, message: "name: name the secret") }
            guard try await secrets.remove(name) else { throw RPCError(code: RPCError.failed, message: "no secret named \(name)") }
            let holders = home.vmNames().filter { (try? home.loadConfig($0))?.egress?.secrets?.contains(name) == true }
            for vm in holders {
                _ = try await editEgress(vm) { egress in
                    egress.secrets = egress.secrets?.filter { $0 != name }
                    if egress.secrets?.isEmpty == true { egress.secrets = nil }
                }
            }
            try await resyncSecrets(holders)
            return .object([:])
        case Method.secretGrant, Method.secretRevoke:
            let p = try params.decode(Params.SecretGrant.self)
            let name = try resolve(p.vm)
            guard let names = p.names, !names.isEmpty else { throw RPCError(code: RPCError.invalidParams, message: "names: name at least one secret") }
            if method == Method.secretGrant {
                let defined = Set(try await secrets.list().map(\.name))
                let unknown = names.filter { !defined.contains($0) }
                guard unknown.isEmpty else {
                    throw RPCError(code: RPCError.failed, message: "no secret named \(unknown.joined(separator: ", ")): define it with glov secret set or glov secret source")
                }
            }
            let grant = method == Method.secretGrant
            let declared = try tmpfsDeclared(home.loadConfig(name))
            _ = try await editEgress(name) { egress in
                var granted = egress.secrets ?? []
                if grant {
                    let plain = names.filter(declared.contains)
                    if egress.open == true, !plain.isEmpty {
                        guard p.allowPlainSecrets == true else {
                            throw Self.plainSecretsRefusal(name, plain, retry: "glov secret grant \(name) \(names.joined(separator: " ")) --allow-plain-secrets")
                        }
                        var allowed = egress.plainSecrets ?? []
                        for n in plain where !allowed.contains(n) { allowed.append(n) }
                        egress.plainSecrets = allowed
                    }
                    for n in names where !granted.contains(n) { granted.append(n) }
                } else {
                    let missing = names.filter { !granted.contains($0) }
                    guard missing.isEmpty else {
                        throw RPCError(code: RPCError.failed, message: "VM \(name) was not granted \(missing.joined(separator: ", "))")
                    }
                    granted.removeAll { names.contains($0) }
                }
                egress.secrets = granted.isEmpty ? nil : granted
            }
            try await resyncSecrets([name])
            return try JSONValue(encoding: try await secretGrants(name))
        case Method.secretLogin:
            let p = try params.decode(Params.SecretLogin.self)
            return try JSONValue(encoding: try await login(try resolve(p.vm), p.harness, value: p.value))
        case Method.secretLogout:
            let p = try params.decode(Params.SecretLogin.self)
            return try JSONValue(encoding: try await logout(try resolve(p.vm), p.harness))
        case Method.secretRepos:
            let p = try params.decode(Params.SecretRepos.self)
            let name = try resolve(p.vm)
            func patterns(_ texts: [String]?) throws -> [String] {
                try (texts ?? []).map { text in
                    guard let pattern = RepoPattern(text) else {
                        throw RPCError(code: RPCError.invalidParams, message: "repository \(text): give <host>/<owner>/<repo> or <host>/<group>/*")
                    }
                    return pattern.text
                }
            }
            let add = try patterns(p.add), remove = try patterns(p.remove)
            if add.isEmpty && remove.isEmpty {
                return try JSONValue(encoding: Results.SecretRepos(vm: name, repos: try isolatedConfig(name).egress?.repos ?? []))
            }
            _ = try await editEgress(name) { egress in
                var repos = egress.repos ?? []
                let missing = remove.filter { !repos.contains($0) }
                guard missing.isEmpty else { throw RPCError(code: RPCError.failed, message: "VM \(name) has no repository \(missing.joined(separator: ", "))") }
                repos.removeAll { remove.contains($0) }
                for r in add where !repos.contains(r) { repos.append(r) }
                egress.repos = repos.isEmpty ? nil : repos
            }
            return try JSONValue(encoding: Results.SecretRepos(vm: name, repos: try home.loadConfig(name).egress?.repos ?? []))
        case Method.secretGrants:
            let p = try params.decode(Params.SecretGrant.self)
            let name = try resolve(p.vm)
            _ = try isolatedConfig(name)
            return try JSONValue(encoding: try await secretGrants(name))
        case Method.egressLog:
            let p = try params.decode(Params.EgressLogRead.self)
            let config = try isolatedConfig(try resolve(p.vm))
            let limit = min(max(p.limit ?? Params.EgressLogRead.defaultLimit, 1), Params.EgressLogRead.maxLimit)
            let page = await Blocking.run { EgressLog.read(path: config.egressLogPath, cursor: p.cursor, deniedOnly: p.denied ?? false, limit: limit) }
            return try JSONValue(encoding: Results.EgressLogPage(records: page.records, cursor: page.cursor))
        case Method.vmStats:
            return try JSONValue(encoding: await stats(try params.decode(Params.Stats.self).vm))
        case Method.vmStop:
            let p = try params.decode(Params.Stop.self)
            let name = try resolve(p.vm)
            return try await exclusive(name) {
                guard let vm = await self.machines[name], await vm.isRunning else { throw GlovError.notRunning }
                await vm.stop(timeout: p.force == true ? .zero : .seconds(60))
                await self.dropConsole(name)
                await self.stopGuestService(name)
                return try JSONValue(encoding: await self.info(name))
            }
        case Method.vmReboot:
            let p = try params.decode(Params.Reboot.self)
            return try JSONValue(encoding: await rebootVM(try resolve(p.vm), timeout: p.timeout ?? Params.Reboot.defaultTimeout, force: p.force == true))
        case Method.snapshotList:
            let name = try resolve(params.decode(Params.VM.self).vm)
            return try await exclusive(name) { try JSONValue(encoding: Results.SnapshotList(snapshots: try await self.listSnapshots(name))) }
        case Method.snapshotCreate:
            let p = try params.decode(Params.Snapshot.self)
            let name = try resolve(p.vm)
            return try await exclusive(name) {
                try await self.createSnapshot(name, p.name)
                return try JSONValue(encoding: Results.SnapshotList(snapshots: try await self.listSnapshots(name)))
            }
        case Method.snapshotRevert:
            let p = try params.decode(Params.Snapshot.self)
            let name = try resolve(p.vm)
            return try await exclusive(name) {
                try await self.revertSnapshot(name, p.name)
                return try JSONValue(encoding: await self.info(name))
            }
        case Method.snapshotDelete:
            let p = try params.decode(Params.Snapshot.self)
            let name = try resolve(p.vm)
            return try await exclusive(name) {
                try await self.deleteSnapshot(name, p.name)
                return try JSONValue(encoding: Results.SnapshotList(snapshots: try await self.listSnapshots(name)))
            }
        case Method.vmWait:
            let p = try params.decode(Params.Wait.self)
            guard p.state == "running" || p.state == "guest-ready" else {
                throw RPCError(code: RPCError.invalidParams, message: "state must be \"running\" or \"guest-ready\"")
            }
            let deadline = ContinuousClock.now + .seconds(p.timeout ?? 60)
            if p.state == "guest-ready" {
                let (name, agent) = try agent(p.vm, control: false)
                // During a Windows install the Guest Service answers before setup.ps1 is done: ready means the install finished.
                func installing() throws -> Bool {
                    guard let install = (try? home.loadConfig(name))?.install else { return false }
                    if let failure = install.failure {
                        throw RPCError(code: RPCError.failed, message: "the Windows install of VM \(name) failed: \(failure)")
                    }
                    return true
                }
                while true {
                    let pending = try installing()
                    if !pending, await agent.isReady { break }
                    if ContinuousClock.now >= deadline, pending {
                        throw RPCError(
                            code: RPCError.failed,
                            message: "timed out waiting for the Windows install of VM \(name) (installing; it takes 10 to 15 minutes, at most 60): call vm_wait again, or screenshot to watch")
                    }
                    if ContinuousClock.now >= deadline {
                        let status = await agent.status()
                        throw RPCError(
                            code: RPCError.failed,
                            message: "timed out waiting for the Guest Service (\(status.state)\(status.problem.map { ": " + $0 } ?? "")); a guest can need minutes to boot (Linux: cloud-init installs the Guest Service on first boot), and the Guest Service must be installed in it")
                    }
                    try await Task.sleep(for: .milliseconds(100))
                }
                var ready = await info(name)
                ready.agent = await agent.status()
                return try JSONValue(encoding: ready)
            }
            let (name, vm) = try await runningMachine(p.vm)
            while true {
                if await vm.guestIsRunning() { break }
                if ContinuousClock.now >= deadline { throw RPCError(code: RPCError.failed, message: "timed out waiting for the VM to be running") }
                try await Task.sleep(for: .milliseconds(250))
            }
            return try JSONValue(encoding: await info(name))
        case Method.screenshot:
            let p = try params.decode(Params.Screenshot.self)
            let shot = try await console(p.vm, input: false).screenshot(
                maxWidth: p.maxWidth, region: p.region.map { PixelRect(x: $0.x, y: $0.y, width: $0.width, height: $0.height) })
            return try JSONValue(encoding: Results.Screenshot(
                png: shot.png.base64EncodedString(), width: shot.size.width, height: shot.size.height,
                frameWidth: shot.frame.width, frameHeight: shot.frame.height))
        case Method.execRun:
            let p = try params.decode(Params.Exec.self)
            return try JSONValue(encoding: await agent(p.vm).1.execRun(p))
        case Method.execStart:
            let p = try params.decode(Params.Exec.self)
            return try JSONValue(encoding: await agent(p.vm).1.execStart(p))
        case Method.execRead:
            let p = try params.decode(Params.ExecRead.self)
            return try JSONValue(encoding: await agent(p.vm).1.execRead(p))
        case Method.execKill:
            let p = try params.decode(Params.ExecKill.self)
            try await agent(p.vm).1.execKill(p)
            return try ok()
        case Method.fileRead:
            let p = try params.decode(Params.FileRead.self)
            return try JSONValue(encoding: await agent(p.vm).1.fileRead(p))
        case Method.fileWrite:
            let p = try params.decode(Params.FileWrite.self)
            return try JSONValue(encoding: await agent(p.vm).1.fileWrite(p))
        case Method.copyToVM:
            let p = try params.decode(Params.CopyToVM.self)
            let (name, agent) = try agent(p.vm)
            return try JSONValue(encoding: await agent.copyToVM(p, current: try home.loadConfig(name)))
        case Method.copyFromVM:
            let p = try params.decode(Params.CopyFromVM.self)
            let (name, agent) = try agent(p.vm)
            return try JSONValue(encoding: await agent.copyFromVM(p, current: try home.loadConfig(name)))
        case Method.shareSync:
            let p = try params.decode(Params.ShareSync.self)
            return try JSONValue(encoding: await shareSync(p))
        case Method.shareStatus:
            let p = try params.decode(Params.ShareStatus.self)
            let (name, agent) = try agent(p.vm)
            if let share = p.share { _ = try home.loadConfig(name).share(try ShareReference.parse(share).0) }
            return try JSONValue(encoding: Results.ShareStatus(syncs: await agent.shareStatus(p.share.map { try ShareReference.parse($0).0 })))
        case Method.click:
            let p = try params.decode(Params.Click.self)
            try await console(p.vm).click(x: p.x, y: p.y, button: p.button ?? "left", count: p.count ?? 1)
            return try ok()
        case Method.move:
            let p = try params.decode(Params.Move.self)
            try await console(p.vm).move(x: p.x, y: p.y)
            return try ok()
        case Method.drag:
            let p = try params.decode(Params.Drag.self)
            try await console(p.vm).drag(fromX: p.fromX, fromY: p.fromY, toX: p.toX, toY: p.toY, button: p.button ?? "left")
            return try ok()
        case Method.type:
            let p = try params.decode(Params.TypeText.self)
            try await console(p.vm).type(p.text)
            return try ok()
        case Method.key:
            let p = try params.decode(Params.Key.self)
            try await console(p.vm).key(p.key)
            return try ok()
        case Method.scroll:
            let p = try params.decode(Params.Scroll.self)
            try await console(p.vm).scroll(x: p.x, y: p.y, dx: p.dx ?? 0, dy: p.dy ?? 0)
            return try ok()
        default:
            throw RPCError(code: RPCError.methodNotFound, message: "unknown method \(method)")
        }
    }

    private func exclusive<T: Sendable>(_ name: String, _ body: @Sendable () async throws -> T) async throws -> T {
        let mutex = locks[name] ?? AsyncMutex()
        locks[name] = mutex
        await mutex.lock()
        do {
            let value = try await body()
            await mutex.unlock()
            return value
        } catch {
            await mutex.unlock()
            throw error
        }
    }

    private func dropConsole(_ name: String) { consoles[name] = nil }

    private func configureVM(
        _ name: String, cpus: Int?, memory: String?, diskSize: String?, balloon: Bool? = nil, requireChange: Bool
    ) async throws {
        if let vm = machines[name], await vm.isRunning {
            throw ProvisionError("stop VM \(name) before reconfiguring CPU, memory, disk size or balloon")
        }
        let home = home
        try await Blocking.run {
            _ = try home.update(name) { config in
                config = try VMConfigurator().configure(
                    config, cpus: cpus, memory: memory, diskSize: diskSize, balloon: balloon, requireChange: requireChange)
            }
        }
    }

    private func setDNS(_ name: String, servers: [String]) async throws -> Results.DNS {
        let current = try home.loadConfig(name)
        guard current.engine == .qemu else { throw ProvisionError("--dns is for QEMU (Windows) VMs") }
        let canonical = servers.isEmpty ? [] : try VMConfig.parseDNS(servers, allowAuto: false)
        let home = home
        _ = try await Blocking.run {
            try home.update(name) { config in
                config.dns = canonical.isEmpty ? nil : canonical
                let marker = VMConfigurator.pendingDNSResetPath(config.directory)
                if canonical.isEmpty {
                    FileManager.default.createFile(atPath: marker, contents: Data(), attributes: [.posixPermissions: 0o600])
                } else {
                    try? FileManager.default.removeItem(atPath: marker)
                }
            }
        }
        if let agent = agents[name], await agent.isReady {
            if let problem = await agent.syncDNS() {
                throw RPCError(code: RPCError.failed, message: "saved, but \(problem); the next Guest Service session retries")
            }
            return Results.DNS(vm: name, servers: canonical, applied: true)
        }
        return Results.DNS(vm: name, servers: canonical, applied: false)
    }

    /// `vm.balloon`: fixes (with `size`) or hands to glovd (`auto`) how much memory a QEMU guest keeps through its
    /// virtio-balloon, saving the choice in vm.json for every start, and reports it.
    private func balloon(_ name: String, size: String?, auto: Bool) async throws -> Results.Balloon {
        var config = try home.loadConfig(name)
        guard config.engine == .qemu else { throw config.engine.unsupported("vm balloon") }
        guard let configured = config.memoryBytes else { throw ProvisionError("cannot read VM \(name)'s memory \(config.memory)") }
        var fixed: UInt64?
        if let size {
            guard let bytes = VMConfig.bytes(size) else { throw ProvisionError("cannot read size \(size)") }
            guard bytes <= configured else {
                throw ProvisionError("size \(size) is more than VM \(name)'s memory (\(config.memory)); give at most \(config.memory)")
            }
            fixed = bytes
        }
        guard config.balloon else {
            throw ProvisionError(
                "VM \(name) has no balloon device: glov vm stop \(name), glov vm configure \(name) --balloon on, then glov vm start \(name)")
        }
        let vm = await runningVM(name)
        if let vm {
            let qemu = await vm.runningQEMUPath()
            let supported = await Blocking.run { qemu.map(VirtualMachine.supportsBalloon(qemu:)) ?? false }
            guard supported else {
                throw ProvisionError(
                    "VM \(name) runs on a QEMU (\(qemu ?? "unknown")) that does not give the memory a balloon takes back to macOS: "
                        + "brew install akunzai/tap/glov-qemu, then restart the VM (glov vm stop \(name), glov vm start \(name))")
            }
        }
        if fixed != nil || auto {
            let home = home
            let fixed = fixed
            await stopBalloon(name)
            config = try await Blocking.run {
                try home.update(name) { c in
                    c.balloonAuto = auto
                    c.balloonTarget = fixed.flatMap { $0 < configured ? $0 : nil }
                }
            }
            if let vm {
                var actual: UInt64?
                do {
                    // A fixed size is sent here so a guest without the device is reported now, not in the log.
                    if let fixed { try await QMPBalloon(vm: vm).setTarget(fixed) }
                    else { actual = try await QMPBalloon(vm: vm).actual() }
                } catch QMPError.command(let cls, let desc) where cls == "DeviceNotActive" || desc.contains("No balloon device") {
                    throw ProvisionError(
                        "VM \(name) was started without its balloon device: restart it (glov vm stop \(name), glov vm start \(name))")
                }
                await startBalloon(vm, config, resuming: true, actual: actual)
            }
        }
        let mode = config.balloonAuto ? "auto" : "fixed"
        guard let vm else {
            // Auto starts with all of it.
            return Results.Balloon(vm: name, memory: nil, configured: configured, target: config.balloonAuto ? nil : config.balloonTarget, mode: mode)
        }
        let actual: UInt64
        do { actual = try await QMPBalloon(vm: vm).actual() } catch QMPError.command(let cls, let desc)
            where cls == "DeviceNotActive" || desc.contains("No balloon device")
        {
            throw ProvisionError("VM \(name) was started without its balloon device: restart it (glov vm stop \(name), glov vm start \(name))")
        }
        guard let controller = balloons[name] else {
            return Results.Balloon(vm: name, memory: actual, configured: configured, target: config.balloonTarget, mode: mode)
        }
        let target = await controller.currentTarget
        return Results.Balloon(
            vm: name, memory: actual, configured: configured, target: target < configured ? target : nil, mode: mode,
            reason: config.balloonAuto ? await controller.reason : nil, used: await controller.used,
            pressure: await controller.lastPressure?.rawValue)
    }

    /// Starts applying a QEMU VM's saved balloon target, and in auto keeps choosing it, until the VM stops.
    /// `resuming`: the VM was already running, so auto starts from what the guest has rather than all of `-m`.
    private func startBalloon(_ vm: VirtualMachine, _ config: VMConfig, resuming: Bool = false, actual: UInt64? = nil) async {
        await stopBalloon(config.name)
        guard config.engine == .qemu, config.balloon, config.balloonAuto || config.balloonTarget != nil else { return }
        let name = config.name
        let controller = BalloonController(config: config, device: QMPBalloon(vm: vm), resuming: resuming, actual: actual)
        balloons[name] = controller
        await controller.start()
    }

    private func stopBalloon(_ name: String) async {
        if let controller = balloons.removeValue(forKey: name) { await controller.stop() }
    }

    /// Starts (or reattaches to) the VM; with `loadSnapshot` it boots straight into that live snapshot.
    /// `keepGuestService` leaves the existing `VMGuestService` in place (its jobs are marked lost and its channel reset) instead of replacing it.
    private func startVM(_ name: String, loadSnapshot: String? = nil, keepGuestService: Bool = false) async throws {
        if let vm = machines[name], await vm.isRunning {
            if let pid = await vm.processID { throw GlovError.alreadyRunning(pid: pid) }
            throw GlovError.unsupported("VM \(name) is already running")
        }
        // Rebuilt from vm.json each time, so edits to the config apply on the next start.
        let config = try home.loadConfig(name)
        if config.engine == .vz {
            let gate = config.isolated
                ? EgressGate(proxy: EgressProxy(config: config, home: home, tls: try egressTLS(), secrets: secrets)) : nil
            let vm = VZMachine(config: config, egress: gate?.accept)
            try await vm.start(restoring: loadSnapshot)
            machines[name] = vm
            consoles[name] = nil
            await startGuestService(config)
            if loadSnapshot != nil { await agents[name]?.expectTimeSync() }
            return
        }
        let vm = try VirtualMachine(config: config)
        var installing = config.install != nil && config.install?.failure == nil
        var typeBoot = false
        let attached = try await vm.attach()
        if !attached {
            if let refusal = WindowsInstall.refusal(name, config.install) { throw GlovError.unsupported(refusal) }
            try await vm.start(loadSnapshot: loadSnapshot)
            if config.install != nil {
                // Whole seconds: vm.json keeps them, and the 60-minute limit runs from here.
                let started = Date(timeIntervalSince1970: Date().timeIntervalSince1970.rounded(.down))
                do { try home.update(name) { config in config.install?.started = started } } catch {
                    await vm.stop(timeout: .zero)
                    throw error
                }
                typeBoot = true
            }
        } else {
            installing = installing && config.install?.started != nil
        }
        machines[name] = vm
        consoles[name] = nil
        if keepGuestService, let agent = agents[name] { await agent.guestRestored() } else {
            await startGuestService(config)
            if loadSnapshot != nil { await agents[name]?.expectTimeSync() }
        }
        // After the Guest Service: starting that stops the VM's balloon controller with its old Guest Service.
        await startBalloon(vm, config, resuming: attached)
        if installing { watchInstall(name, typeBoot: typeBoot) }
    }

    /// `vm.reboot`: restarts the guest and returns once a Guest Service from a new boot is ready, so a caller never takes
    /// the old boot for the new one. A Windows (QEMU) guest restarts itself through `power.reboot` (a real restart, never
    /// a Fast Startup shutdown; the QEMU process stays); a Linux (VZ) guest is stopped with ACPI and started again by the
    /// host, which needs no Guest Service and so also restarts a guest whose Guest Service is not answering (a guest that
    /// reboots itself would stay in the same VZ process). Only the restart holds the VM's lock: the wait
    /// leaves `vm stop` free. On timeout nothing is forced.
    private func rebootVM(_ name: String, timeout: Double, force: Bool) async throws -> Results.VMInfo {
        guard timeout.isFinite, timeout > 0 else { throw RPCError(code: RPCError.invalidParams, message: "timeout must be a positive number of seconds") }
        let previous: String? = try await exclusive(name) {
            guard let vm = await self.machines[name], await vm.isRunning else { throw GlovError.notRunning }
            let config = try self.home.loadConfig(name)
            if config.install != nil {
                throw RPCError(code: RPCError.failed, message: "VM \(name) is still installing Windows: 'glov vm wait \(name) guest-ready' first")
            }
            let manual = "'glov vm stop \(name)' (with --force if it does not stop) and 'glov vm start \(name)'"
            guard config.guestService, let agent = await self.agents[name] else {
                throw RPCError(code: RPCError.failed, message: "VM \(name) has the Guest Service disabled, so a reboot cannot tell when the new boot is ready; restart it with \(manual)")
            }
            let status = await agent.status()
            let previous = status.state == "ready" ? status.bootId : nil
            if vm.config.engine == .vz {
                try await agent.prepareReboot(force: force)
                await vm.stop(timeout: .seconds(60))
                await self.dropConsole(name)
                await self.stopGuestService(name)
                if let restart = await self.restartForTesting { try await restart(name) } else { try await self.startVM(name) }
                return previous
            }
            // Never a hard reset: without a Guest Service to ask, the caller decides how to restart.
            guard status.state == "ready" else {
                throw RPCError(
                    code: RPCError.failed,
                    message: "the Guest Service of VM \(name) is not ready (\(status.state)\(status.problem.map { ": " + $0 } ?? "")), so it cannot restart Windows; restart the VM with \(manual)")
            }
            try await agent.prepareReboot(force: force)
            try await agent.requestReboot()
            return previous
        }
        let deadline = ContinuousClock.now + .seconds(timeout)
        while true {
            guard let vm = machines[name], await vm.isRunning else {
                throw RPCError(code: RPCError.failed, message: "VM \(name) stopped while rebooting; start it with 'glov vm start \(name)'")
            }
            let status = await agents[name]?.status()
            if let status, status.state == "ready", status.bootId != previous {
                var rebooted = await info(name)
                rebooted.agent = status
                return rebooted
            }
            if ContinuousClock.now >= deadline {
                let current = await info(name)
                let agent = current.agent ?? status
                let state = (agent?.state ?? "stopped") + (agent?.problem.map { ": " + $0 } ?? "")
                    + (agent?.state == "ready" ? ", still the boot from before the reboot" : "")
                throw RPCError(
                    code: RPCError.failed,
                    message: "VM \(name) did not finish rebooting within \(timeout.formatted()) s (VM \(current.state), Guest Service \(state)); look at the guest with 'glov vm screenshot \(name) -o <file>' before deciding on 'glov vm stop --force \(name)'")
            }
            try await Task.sleep(for: .milliseconds(100))
        }
    }

    // Windows install (see WindowsInstall): the first start boots the installer; once the Guest Service answers and setup.ps1
    // is done, the install drives are unplugged and left out of vm.json.

    private func watchInstall(_ name: String, typeBoot: Bool) {
        installs[name]?.cancel()
        installs[name] = Task { await self.runInstall(name, typeBoot: typeBoot) }
    }

    /// Waits for the running install of `name` to end (tests).
    func installFinishedForTesting(_ name: String) async { await installs[name]?.value }

    private func runInstall(_ name: String, typeBoot: Bool) async {
        guard let started = (try? home.loadConfig(name))?.install?.started else { return }
        let outcome = await WindowsInstall.watch(
            started: started, timing: installTiming, typeBoot: typeBoot,
            send: { step in try await self.sendInstallStep(name, step) },
            isRunning: { await self.runningVM(name) != nil },
            finished: { await self.installIsDone(name) },
            log: { DaemonLog.write("\(name): \($0)") })
        switch outcome {
        case .cancelled: return  // a newer watch owns the install now
        case .finished: await finishInstall(name)
        case .timedOut: failInstall(name, "the Guest Service and setup.ps1 were not done within \(installTiming.timeout.components.seconds / 60) minutes")
        case .stopped: failInstall(name, "the VM stopped before the install finished")
        }
        installs[name] = nil
    }

    private func sendInstallStep(_ name: String, _ step: WindowsInstall.Step) async throws {
        let console = try await console(name, input: false)  // the daemon's own input: agentControl does not apply
        switch step {
        case .type(let text): try await console.type(text)
        case .key(let key): try await console.key(key)
        case .pause(let d): try await Task.sleep(for: d)
        }
    }

    /// Tests stand in for the guest: no agent runs in the UEFI shell.
    private var installDoneCheck: (@Sendable (String) async -> Bool)?
    func setInstallDoneCheckForTesting(_ check: @escaping @Sendable (String) async -> Bool) { installDoneCheck = check }

    private func installIsDone(_ name: String) async -> Bool {
        if let installDoneCheck { return await installDoneCheck(name) }
        guard let agent = agents[name], await agent.isReady else { return false }
        return await agent.guestFileExists(WindowsAssets.setupDoneMarker)
    }

    /// Unplugs the install drives from the running guest (so a live snapshot matches the next start) and drops them
    /// from vm.json; deletes the ones in the VM's secrets (the unattend image holds the password).
    private func finishInstall(_ name: String) async {
        guard let config = try? home.loadConfig(name), config.install != nil else { return }
        if let vm = await runningVM(name) {
            do { try await vm.detachDrives(config.installDriveIndices) } catch {
                DaemonLog.write("\(name): cannot unplug the install drives, they go at the next start: \(error)")
            }
        }
        do { try home.update(name) { config in if config.install != nil { config = config.finishingInstall() } } } catch {
            DaemonLog.write("\(name): cannot record the finished install: \(error)")
            return
        }
        deleteSecretDrives(config)
    }

    /// An install that is not resumed: recorded as failed, and the unattend image (plain-text password) deleted.
    private func failInstall(_ name: String, _ reason: String) {
        guard let config = try? home.loadConfig(name), config.install != nil else { return }
        _ = try? home.update(name) { config in config.install?.failure = reason }
        deleteSecretDrives(config)
    }

    private func deleteSecretDrives(_ config: VMConfig) {
        for path in config.install?.drives ?? [] where path.hasPrefix(config.secretsDirectory + "/") {
            try? FileManager.default.removeItem(atPath: path)
        }
    }

    /// The running QEMU machine of `name`, if any.
    private func runningVM(_ name: String) async -> VirtualMachine? {
        if let vm = machines[name] as? VirtualMachine, await vm.isRunning { return vm }
        return nil
    }

    func machineForTesting(_ name: String) async -> VirtualMachine? { await runningVM(name) }

    /// The running VZ machine of `name`, if any.
    private func runningVZ(_ name: String) async -> VZMachine? {
        if let vm = machines[name] as? VZMachine, await vm.isRunning { return vm }
        return nil
    }

    /// Powers off every VM that runs inside this process (VZ), which would otherwise die with it. QEMU VMs keep running.
    public func stopInProcessVMs(timeout: Duration) async {
        await withTaskGroup(of: Void.self) { group in
            for vm in machines.values where vm is VZMachine { group.addTask { await vm.stop(timeout: timeout) } }
        }
    }

    // Snapshots: QEMU keeps them inside the qcow2 disk, VZ as clones under the VM's snapshots directory (VZSnapshots).

    private func listSnapshots(_ name: String) async throws -> [SnapshotInfo] {
        let config = try home.loadConfig(name)
        if config.engine == .vz { return try VZSnapshots.list(config) }
        if let vm = await runningVM(name) { return try await vm.listSnapshots() }
        return try await Snapshots.listStopped(config)
    }

    private func createSnapshot(_ name: String, _ snapshot: String) async throws {
        if let vm = await runningVZ(name) {
            // A live snapshot saves the guest's memory, tmpfs included.
            if await agents[name]?.holdsTmpfsSecrets == true {
                throw RPCError(code: RPCError.failed, message: "VM \(name) was given tmpfs secrets since it booted, and a live snapshot would save them: stop it (glov vm stop \(name)) to take a disk-only snapshot")
            }
            return try await vm.createSnapshot(snapshot)
        }
        if let vm = await runningVM(name) { return try await vm.createSnapshot(snapshot) }
        let config = try home.loadConfig(name)
        if config.engine == .vz { try VZSnapshots.createStopped(config, name: snapshot) } else { try await Snapshots.createStopped(config, name: snapshot) }
    }

    private func deleteSnapshot(_ name: String, _ snapshot: String) async throws {
        let config = try home.loadConfig(name)
        if config.engine == .vz { return try VZSnapshots.delete(config, name: snapshot) }
        if let vm = await runningVM(name) { return try await vm.deleteSnapshot(snapshot) }
        try await Snapshots.deleteStopped(config, name: snapshot)
    }

    private func revertSnapshot(_ name: String, _ snapshot: String) async throws {
        if try home.loadConfig(name).engine == .vz { return try await revertVZSnapshot(name, snapshot) }
        if let vm = await runningVM(name) {
            // snapshot-load into a running guest aborts QEMU, so the running machine is discarded (it is being
            // replaced anyway) and QEMU is started afresh straight into the snapshot.
            try await vm.requireLiveSnapshot(snapshot)
            try await Snapshots.requireMachine(try home.loadConfig(name), name: snapshot)
            await vm.stop(timeout: .zero)
            consoles[name] = nil
            try await startVM(name, loadSnapshot: snapshot, keepGuestService: true)
            return
        }
        let config = try home.loadConfig(name)
        let known = try await Snapshots.listStopped(config)
        guard let found = known.first(where: { $0.name == snapshot }) else { throw SnapshotError.notFound(snapshot, known: known.map(\.name)) }
        if found.live {
            try await Snapshots.requireMachine(config, name: snapshot)
            try await startVM(name, loadSnapshot: snapshot)
        } else {
            try await Snapshots.revertStopped(config, name: snapshot)
        }
    }

    /// Like QEMU: a live snapshot restarts (or starts) the guest into it, a disk-only one needs the VM stopped and leaves it so.
    ///
    /// An isolated VM first gets back the egress policy it had when the snapshot was taken, so a reverted guest never runs
    /// under what a later task was allowed; a refused revert leaves the policy alone. Its secret grants are those of the
    /// snapshot that it still has, so a revert never grants back a secret revoked since. Its subscriptions are vm.json's
    /// as they are now, with their tokens' grants and runtime hosts: signing in and out is the owner's on the Mac, so a
    /// template snapshot taken before a `glov secret login` keeps it, and one taken before a logout does not undo it.
    /// The guest is switched to match at its next Guest Service session.
    private func revertVZSnapshot(_ name: String, _ snapshot: String) async throws {
        let config = try home.loadConfig(name)
        let found = try VZSnapshots.find(config, snapshot)
        let running = await runningVZ(name)
        if running != nil, !found.live { throw SnapshotError.needsStoppedVM(snapshot) }
        if let egress = VZSnapshots.savedEgress(config, snapshot) {  // the caller holds the VM's lock
            try home.update(name) { config in
                let current = Set(config.egress?.secrets ?? [])
                var restored = egress
                restored.secrets = egress.secrets?.filter(current.contains)
                if restored.secrets?.isEmpty == true { restored.secrets = nil }
                restored.subscriptions = config.egress?.subscriptions
                for p in self.harnessProfiles where restored.subscriptions?.contains(p.name) == true {
                    guard let sub = p.subscription else { continue }
                    for n in sub.secrets.map({ p.secretName(vm: name, $0.name) }) where current.contains(n) && restored.secrets?.contains(n) != true {
                        restored.secrets = (restored.secrets ?? []) + [n]
                    }
                    for host in sub.runtime where !restored.allow.contains(host) { restored.allow.append(host) }
                }
                config.egress = restored
            }
        }
        if let vm = running {
            // VZ restores only into a stopped machine. The same actor is restarted, so the Guest Service channel's vsock connector stays valid.
            await vm.stop(timeout: .zero)
            consoles[name] = nil
            try await vm.start(restoring: snapshot)
            if let agent = agents[name] { await agent.guestRestored() } else {
                await startGuestService(config)
                await agents[name]?.expectTimeSync()
            }
            return
        }
        if found.live { try await startVM(name, loadSnapshot: snapshot) } else { try VZSnapshots.revertStopped(config, name: snapshot) }
    }

    private func ok() throws -> JSONValue { try JSONValue(encoding: Results.OK()) }

    private func resolve(_ reference: String) throws -> String { try home.resolve(reference) }

    /// A connection to the SSH relay of a running VZ VM's Guest Service (`SSHRelay`), as a blocking descriptor the caller owns.
    func openSSH(_ reference: String) async throws -> Int32 {
        let name = try resolve(reference)
        let config = try home.loadConfig(name)
        guard config.engine == .vz else {
            throw RPCError(code: RPCError.failed, message: "VM \(name) is a QEMU VM: glov ssh reaches it through its forwarded sshPort")
        }
        guard let vm = machines[name] as? any VsockMachine, await vm.isRunning else {
            throw RPCError(code: RPCError.failed, message: "VM \(name) is not running; start it with glov vm start \(name)")
        }
        do { return try await vm.connect(port: VZConfiguration.sshPort) } catch GuestServiceError.notConnected {
            // Nothing listens there before the Guest Service learned to relay SSH, or while it starts.
            throw RPCError(code: RPCError.failed, message: "VM \(name)'s Guest Service does not relay SSH yet: wait for glov vm wait \(name) guest-ready, or update it with glov vm guest update \(name)")
        }
    }

    private func info(_ name: String) async -> Results.VMInfo {
        let config = try? home.loadConfig(name)
        let install = config?.install
        if let vm = machines[name], await vm.isRunning {
            var agent: Results.GuestServiceStatus?
            if let running = agents[name] { agent = Self.withMacPressure(await running.status(), mac: MacResources.memory()) } else if config?.guestService == false {
                agent = await unavailableGuestService(config: config, state: "disabled", reason: "Guest Service is disabled")
            }
            var info = Results.VMInfo(
                name: name, state: WindowsInstall.state(running: true, install: install), pid: await vm.processID, agent: agent,
                engine: vm.config.engine.rawValue, installFailure: install?.failure)
            if info.state == "installing", let started = install?.started {
                info.install = WindowsInstall.progress(started: started, now: Date(), timing: installTiming, guestReady: agent?.state == "ready")
            }
            return info
        }
        if machines[name] != nil { await stopGuestService(name) }  // the guest went away on its own
        let state = WindowsInstall.state(running: false, install: install)
        return .init(
            name: name, state: state,
            agent: await unavailableGuestService(config: config, state: config?.guestService == false ? "disabled" : "stopped",
                reason: config?.guestService == false ? "Guest Service is disabled" : "VM is not running"),
            engine: config?.engine.rawValue,
            installFailure: install?.failure ?? (state == "install-failed" ? "interrupted" : nil))
    }

    /// An unresponsive Guest Service on a Mac under memory pressure is most likely starved of memory: its `problem` says so.
    static func withMacPressure(_ status: Results.GuestServiceStatus, mac: MacMemory) -> Results.GuestServiceStatus {
        guard status.state == "unresponsive", let note = MemoryBudget.pressureNote(mac) else { return status }
        var named = status
        named.problem = (status.problem.map { $0 + "; " } ?? "") + note + ", which can starve the guest: see 'glov vm stats'"
        return named
    }

    /// The warning for starting `name` now (`MemoryBudget`), or nil.
    private func startWarning(_ name: String) async -> String? {
        guard let config = try? home.loadConfig(name), let memory = config.memoryBytes else { return nil }
        var running: [(name: String, memory: UInt64)] = []
        for (other, vm) in machines.sorted(by: { $0.key < $1.key }) where other != name {
            if await vm.isRunning { running.append((other, vm.config.memoryBytes ?? 0)) }
        }
        return MemoryBudget.warning(starting: (name, memory), running: running, mac: MacResources.memory())
    }

    /// `vm.stats` of `reference`, or of every running VM.
    private func stats(_ reference: String?) async throws -> Results.Stats {
        var running: [(String, any Machine)] = []
        if let reference { running = [try await runningMachine(reference)] } else {
            for (name, vm) in machines.sorted(by: { $0.key < $1.key }) where await vm.isRunning { running.append((name, vm)) }
        }
        var subjects: [StatsCollector.Subject] = []
        for (name, vm) in running {
            let config = vm.config
            var pid = await vm.processID
            var problem: String?
            if pid == nil, config.engine == .vz {
                let held = config.disk ?? config.firmwareVars
                pid = MacResources.process(holding: held)
                if pid == nil { problem = "no Virtualization.framework process holds \(held) open" }
            } else if pid == nil {
                problem = "no qemu process"
            }
            let agent = agents[name], enabled = config.guestService
            var subject = StatsCollector.Subject(config: config, pid: pid, macProblem: problem) {
                guard let agent else { return (nil, enabled ? "Guest Service is not connected" : "Guest Service is disabled") }
                return await agent.guestStats()
            }
            // The device is the running VM's; the mode is vm.json's, which `vm balloon` changes while the VM runs.
            if config.engine == .qemu, config.balloon, let qemu = vm as? VirtualMachine {
                let saved = (try? home.loadConfig(name)) ?? config
                subject.balloonMode = saved.balloonAuto ? "auto" : saved.balloonTarget != nil ? "fixed" : nil
                subject.balloon = { try? await QMPBalloon(vm: qemu).actual() }
            }
            subjects.append(subject)
        }
        return await StatsCollector.collect(subjects, mac: MacResources.memory())
    }

    private func unavailableGuestService(config: VMConfig?, state: String, reason: String) async -> Results.GuestServiceStatus {
        let os = config?.engine == .vz ? "linux" : "windows"
        let version = try? await Blocking.run { [guestServiceDirectory] in try GuestServiceDistribution.target(os: os, directory: guestServiceDirectory).version }
        return .init(state: state, os: os, update: .init(state: "unknown", targetVersion: version, problem: reason))
    }

    /// (Re)starts the channel to the Guest Service of a VM that is running; a no-op when `guestService` is off.
    func startGuestService(_ config: VMConfig) async {
        await stopGuestService(config.name)
        guard config.guestService else { return }
        let agent: VMGuestService
        if config.engine == .vz {
            guard let vm = machines[config.name] as? VZMachine else { return }
            // A vsock connect fails until the Guest Service listens, which takes a boot (and on first boot, cloud-init).
            agent = VMGuestService(config: config, channel: GuestChannel(maxRetryInterval: .seconds(2), log: DaemonLog.vm(config.name)) { try await vm.connect() }, guestServiceDirectory: guestServiceDirectory)
        } else {
            agent = VMGuestService(config: config, guestServiceDirectory: guestServiceDirectory)
        }
        let name = config.name
        await agent.setTmpfsSecrets { [weak self] in await self?.tmpfsSecretValues(name) ?? nil }
        await agent.setHarnessMode { [weak self] in await self?.harnessMode(name) ?? nil }
        if config.egress != nil {
            await agent.setGitSignIns { [weak self] in await self?.gitSignIns(name) ?? nil }
            // Nil (no config to read) skips the sync; an empty list empties the guest's store.
            await agent.setTrust { [weak self] in
                guard let config = try? self?.home.loadConfig(name) else { return nil }
                return config.egress?.trust ?? []
            }
        }
        if config.engine == .qemu {
            await agent.setDNS { [weak self] in
                guard let config = try? self?.home.loadConfig(name) else { return nil }
                return config.dns
            }
        }
        await agent.start()
        agents[config.name] = agent
    }

    func stopGuestServiceForTesting(_ name: String) async { await stopGuestService(name) }

    private func stopGuestService(_ name: String) async {
        await stopBalloon(name)
        if let agent = agents.removeValue(forKey: name) { await agent.stop() }
    }

    /// Changes an isolated VM's egress settings in vm.json through `GlovHome.update`, so no other vm.json writer
    /// (`glov share add` included) loses it, and under the VM's lock so it waits for a snapshot revert. The proxy
    /// re-reads vm.json for every connection, so this applies to the next one.
    private func editEgress(_ name: String, _ change: @escaping @Sendable (inout VMConfig.Egress) throws -> Void) async throws -> Results.Egress {
        try await exclusive(name) {
            _ = try await self.isolatedConfig(name)
            let config = try self.home.update(name) { config in
                var egress = config.egress ?? .init()
                try change(&egress)
                config.egress = egress
            }
            return Self.egressResult(name, config)
        }
    }

    /// VM `name`'s harness profiles and the ones on a subscription; nil when it has no harness.
    func harnessMode(_ name: String) async -> (profiles: [HarnessProfile], subscribed: Set<String>)? {
        guard let config = try? home.loadConfig(name), !(config.harnesses ?? []).isEmpty else { return nil }
        return (profiles(config), Set(config.egress?.subscriptions ?? []))
    }

    /// The profile of `harness`, one VM `name` was created with that has a subscription sign-in.
    private func subscriptionProfile(_ name: String, _ harness: String) throws -> (HarnessProfile, HarnessProfile.Subscription) {
        let config = try isolatedConfig(name)
        guard (config.harnesses ?? []).contains(harness), let profile = harnessProfiles.first(where: { $0.name == harness }) else {
            throw RPCError(code: RPCError.failed, message: "VM \(name) has no harness \(harness): its harnesses are \((config.harnesses ?? []).joined(separator: ", ")), see glov vm harness list")
        }
        guard let sub = profile.subscription else { throw RPCError(code: RPCError.failed, message: "harness \(harness) has no subscription sign-in") }
        return (profile, sub)
    }

    /// Signs VM `name`'s `harness` in to the owner's subscription (docs/isolated-harness-vms.md "Subscriptions"): its
    /// tokens become secrets `<vm>.<harness>.<name>`, granted to the VM; its runtime hosts are allowed; and the guest is
    /// switched to the subscription now if it runs, else at its next Guest Service session.
    private func login(_ name: String, _ harness: String, value: String?) async throws -> Results.SecretLogin {
        let (profile, sub) = try subscriptionProfile(name, harness)
        let names = sub.secrets.map { profile.secretName(vm: name, $0.name) }
        if sub.signIn == nil {
            guard let value, !value.isEmpty, let secret = sub.secrets.first else {
                throw RPCError(code: RPCError.invalidParams, message: "value: paste the token (\(sub.pasteHint ?? "see the harness's docs"))")
            }
            try await secrets.set(profile.secretName(vm: name, secret.name), hosts: secret.hosts, value: value)
        }
        _ = try await editEgress(name) { egress in
            var subscribed = egress.subscriptions ?? []
            if !subscribed.contains(harness) { subscribed.append(harness) }
            egress.subscriptions = subscribed
            for host in sub.runtime where !egress.allow.contains(host) { egress.allow.append(host) }
            var granted = egress.secrets ?? []
            for n in names where !granted.contains(n) { granted.append(n) }
            egress.secrets = granted
        }
        try await resyncSecrets([name])
        if let agent = agents[name], let problem = await agent.syncHarnessMode(force: true) {
            throw RPCError(code: RPCError.failed, message: "signed in on the Mac, but \(problem); the next Guest Service session retries")
        }
        let defined = Set(try await secrets.list().map(\.name))
        return .init(vm: name, harness: harness, command: sub.signIn, missing: names.filter { !defined.contains($0) })
    }

    /// Undoes `login`: removes the harness's subscription secrets and their grants, the runtime hosts no other harness of
    /// the VM needs, and switches the guest back to the API key. The caller runs the profile's `signOut` in the guest
    /// first, while the proxy can still put the real token into a revocation.
    private func logout(_ name: String, _ harness: String) async throws -> Results.SecretLogin {
        let (profile, sub) = try subscriptionProfile(name, harness)
        guard try home.loadConfig(name).egress?.subscriptions?.contains(harness) == true else {
            throw RPCError(code: RPCError.failed, message: "harness \(harness) of VM \(name) is not signed in to a subscription: glov secret login \(name) \(harness)")
        }
        let names = sub.secrets.map { profile.secretName(vm: name, $0.name) }
        let others = (try home.loadConfig(name).harnesses ?? []).filter { $0 != harness }.compactMap { h in harnessProfiles.first { $0.name == h } }
        let subscribedOthers = Set(try home.loadConfig(name).egress?.subscriptions ?? [])
        let needed = Set(others.flatMap { p in p.runtime + (subscribedOthers.contains(p.name) ? p.subscription?.runtime ?? [] : []) } + profile.runtime)
        _ = try await editEgress(name) { egress in
            egress.subscriptions = egress.subscriptions?.filter { $0 != harness }
            if egress.subscriptions?.isEmpty == true { egress.subscriptions = nil }
            egress.allow.removeAll { sub.runtime.contains($0) && !needed.contains($0) }
            egress.secrets = egress.secrets?.filter { !names.contains($0) }
            if egress.secrets?.isEmpty == true { egress.secrets = nil }
        }
        for n in names { _ = try await secrets.remove(n) }
        try await resyncSecrets([name])
        if let agent = agents[name], let problem = await agent.syncHarnessMode(force: true) {
            throw RPCError(code: RPCError.failed, message: "signed out on the Mac, but \(problem); the next Guest Service session retries")
        }
        return .init(vm: name, harness: harness)
    }

    /// The `tmpfs` secrets VM `name` should hold: those its harnesses declare that it is granted and that are defined,
    /// with their values; nil when its harnesses declare none. A value that cannot be read is left out and logged, and so
    /// is one an open VM was not allowed to hold (`Egress.plainSecrets`), since any host could then receive it.
    func tmpfsSecretValues(_ name: String) async -> [(name: String, value: String)]? {
        guard let config = try? home.loadConfig(name) else { return nil }
        let declared = tmpfsDeclared(config)
        guard !declared.isEmpty else { return nil }
        let defined = Set((try? await secrets.list().map(\.name)) ?? [])
        let allowed = config.egress?.open == true ? Set(config.egress?.plainSecrets ?? []) : nil
        var out: [(name: String, value: String)] = []
        for secret in config.egress?.secrets ?? [] where declared.contains(secret) && defined.contains(secret) {
            if let allowed, !allowed.contains(secret) {
                DaemonLog.write("\(name): tmpfs secret \(secret) withheld: the VM is open and the owner did not allow it (--allow-plain-secrets)")
                continue
            }
            do { out.append((secret, try await secrets.value(secret))) } catch {
                DaemonLog.write("\(name): tmpfs secret \(secret) left out: \(error)")
            }
        }
        return out
    }

    /// The git secrets granted to isolated VM `name`; nil when its config or the definitions cannot be read.
    func gitSignIns(_ name: String) async -> [GitSignIn.Entry]? {
        guard let config = try? home.loadConfig(name), config.egress != nil, let definitions = try? await secrets.list() else { return nil }
        let granted = Set(config.egress?.secrets ?? [])
        return definitions.compactMap { d in
            guard granted.contains(d.name), let forge = d.forge else { return nil }
            return GitSignIn.Entry(name: d.name, forge: forge, hosts: d.hosts)
        }
    }

    /// Brings the `tmpfs` secrets of each running VM in `names` up to date, after a grant, revoke or value change.
    /// Throws when one could not be delivered, so the caller hears of a revoked value still in a guest.
    private func resyncSecrets(_ names: [String]) async throws {
        var problems: [String] = []
        for name in names {
            guard let agent = agents[name] else { continue }
            if let problem = await agent.syncSecrets() { problems.append("\(name): \(problem)") }
            if let problem = await agent.syncGitSignIns() { problems.append("\(name): \(problem)") }
        }
        guard problems.isEmpty else {
            throw RPCError(code: RPCError.failed, message: "saved, but \(problems.joined(separator: "; ")); the next Guest Service session retries, or stop the VM to clear its tmpfs")
        }
    }

    private func secretGrants(_ name: String) async throws -> Results.SecretGrants {
        let granted = try home.loadConfig(name).egress?.secrets ?? []
        let defined = Set(try await secrets.list().map(\.name))
        return Results.SecretGrants(vm: name, names: granted, missing: granted.filter { !defined.contains($0) })
    }

    /// The secrets the harnesses of `config` take as real values in tmpfs (`HarnessProfile.Secret.delivery`).
    func tmpfsDeclared(_ config: VMConfig) -> Set<String> {
        Set(profiles(config).flatMap(\.secrets).filter { $0.delivery == .tmpfs }.map(\.name))
    }

    /// The harness profiles `config`'s VM was created or extended with.
    private func profiles(_ config: VMConfig) -> [HarnessProfile] {
        (config.harnesses ?? []).compactMap { h in harnessProfiles.first { $0.name == h } }
    }

    static func plainSecretsRefusal(_ name: String, _ plain: [String], retry: String) -> RPCError {
        RPCError(
            code: RPCError.plainSecrets,
            message: "VM \(name) would hold \(plain.joined(separator: ", ")) as real values in tmpfs while open, and then any host could receive them; if that is acceptable, run \(retry)")
    }

    private static func trustResult(_ name: String, _ config: VMConfig) -> Results.Trust {
        Results.Trust(vm: name, certificates: (config.egress?.trust ?? []).map(TrustedCertificate.summary))
    }

    private static func egressResult(_ name: String, _ config: VMConfig) -> Results.Egress {
        Results.Egress(
            vm: name, allow: config.egress?.allow ?? [], mcpEdits: config.egress?.mcpEdits == true, private: config.egress?.privateAllow,
            open: config.egress?.open)
    }

    /// The egress CA and its TLS contexts, made when the first isolated VM starts and shared by all of them.
    private func egressTLS() throws -> EgressTLS {
        if let tls = egressTLSCache { return tls }
        let tls = try EgressTLS(ca: try EgressCA.loadOrCreate(home: home))
        egressTLSCache = tls
        return tls
    }

    /// The VM's config, refusing one that is not isolated: only those have an egress policy.
    private func isolatedConfig(_ name: String) throws -> VMConfig {
        let config = try home.loadConfig(name)
        guard config.isolated else {
            throw RPCError(code: RPCError.failed, message: "VM \(name) is not isolated: egress policy applies only to VMs created with glov vm create --engine vz --isolated")
        }
        return config
    }

    /// Refuses a VM whose `agentControl` is off. Read from vm.json each time, so a change applies at once.
    private func requireControl(_ name: String) throws {
        if try home.loadConfig(name).agentControl == false {
            throw RPCError(
                code: RPCError.failed,
                message: "agent control is disabled for VM \(name) (\"agentControl\": false in its vm.json): exec, file, copy and input tools are refused; screenshots still work")
        }
    }

    /// The Guest Service of a running VM, after the `agentControl` check unless `control` is false.
    private func agent(_ reference: String, control: Bool = true) throws -> (String, VMGuestService) {
        let name = try resolve(reference)
        if control { try requireControl(name) }
        guard let agent = agents[name] else {
            let enabled = (try? home.loadConfig(name))?.guestService ?? true
            throw RPCError(
                code: RPCError.failed,
                message: enabled ? "VM \(name) is not running, so it has no Guest Service (vm_start it, then vm_wait guest-ready)" : "VM \(name) has the Guest Service disabled (\"agent\": false in its vm.json)")
        }
        return (name, agent)
    }

    /// `share.sync`: from vm.json as it is now, on a running QEMU VM (a VZ share is a live mount: refused) whose Guest Service is ready.
    private func shareSync(_ p: Params.ShareSync) async throws -> Results.ShareSync {
        let name = try resolve(p.vm)
        try requireControl(name)
        let config = try home.loadConfig(name)
        if config.engine == .vz { throw RPCError(code: RPCError.failed, message: "share sync: " + VMConfig.Share.liveMount) }
        let (shareName, subdir) = try ShareReference.parse(p.share)
        let share = try config.share(shareName)
        if config.install != nil {
            throw RPCError(code: RPCError.failed, message: "VM \(name) is still installing Windows: vm_wait guest-ready, then sync")
        }
        let (_, agent) = try agent(name)  // fails with "not running" when it is not
        guard await agent.isReady else {
            let status = await agent.status()
            throw RPCError(
                code: RPCError.failed,
                message: "the Guest Service of VM \(name) is not ready (\(status.state)\(status.problem.map { ": " + $0 } ?? "")): vm_wait guest-ready, then sync")
        }
        return try await agent.shareSync(share, subdir: subdir, verify: p.verify ?? false, cacheDirectory: config.directory + "/sync")
    }

    private func runningMachine(_ reference: String) async throws -> (String, any Machine) {
        let name = try resolve(reference)
        guard let vm = machines[name], await vm.isRunning else { throw GlovError.notRunning }
        return (name, vm)
    }

    /// One console per running VM, so it keeps the size of the last screenshot for pointer input.
    private func console(_ reference: String, input: Bool = true) async throws -> Console {
        let name = try resolve(reference)
        if input { try requireControl(name) }
        let (_, machine) = try await runningMachine(reference)
        if let c = consoles[name] { return c }
        let c: Console
        switch machine {
        case let vm as VirtualMachine: c = try await vm.console()
        case let vm as VZMachine: c = try await vm.console()
        default: throw machine.config.engine.unsupported("screen access")
        }
        consoles[name] = c
        return c
    }
}
