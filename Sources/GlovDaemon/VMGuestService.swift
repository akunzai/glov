import Foundation
import GlovCore
import GlovDaemonAPI

/// One process started through `exec.start`. Output is kept twice: the last `ringCapacity` bytes in memory
/// and everything in `logPath`. Confined to its `VMGuestService`.
final class Job {
    static let ringCapacity = 1 << 20

    let id: String
    let pid: Int
    let logPath: String
    private var log: FileHandle?
    private(set) var ring: [UInt8] = []
    /// Bytes of output seen so far; `ring` holds the last `ring.count` of them.
    private(set) var total = 0
    private var newlines = 0
    private var endsWithNewline = true
    var code: Int?
    var lost = false
    var maintenance = false

    init(id: String, pid: Int, logPath: String) throws {
        self.id = id
        self.pid = pid
        self.logPath = logPath
        FileManager.default.createFile(atPath: logPath, contents: nil, attributes: [.posixPermissions: 0o600])
        log = try FileHandle(forWritingTo: URL(fileURLWithPath: logPath))
    }

    var finished: Bool { code != nil || lost }
    var lineCount: Int { newlines + (endsWithNewline ? 0 : 1) }
    var ringStart: Int { total - ring.count }

    func append(_ data: Data) {
        guard !data.isEmpty, !finished else { return }
        try? log?.write(contentsOf: data)
        ring += data
        if ring.count > Self.ringCapacity { ring.removeFirst(ring.count - Self.ringCapacity) }
        total += data.count
        newlines += data.reduce(0) { $0 + ($1 == 10 ? 1 : 0) }
        endsWithNewline = data.last == 10
    }

    func close() {
        try? log?.close()
        log = nil
    }

    /// The last `lines` lines, at most `maxBytes`, with CRLF turned into LF.
    func tail(lines: Int = 50, maxBytes: Int = 32 * 1024) -> String {
        var seen = 0
        var start = ring.count
        while start > 0 {
            if ring[start - 1] == 10, start != ring.count { seen += 1; if seen == lines { break } }
            start -= 1
        }
        if ring.count - start > maxBytes { start = ring.count - maxBytes }
        return Self.text(ring[start...])
    }

    /// Length of `bytes` without a UTF-8 sequence cut off at its end.
    static func completeLength(_ bytes: ArraySlice<UInt8>) -> Int {
        var end = bytes.endIndex
        if let lead = (max(bytes.startIndex, end - 4)..<end).last(where: { bytes[$0] & 0xC0 != 0x80 }) {
            let b = bytes[lead]
            let needed = b >= 0xF0 ? 4 : b >= 0xE0 ? 3 : b >= 0xC0 ? 2 : 1
            if lead + needed > end { end = lead }
        }
        return end - bytes.startIndex
    }

    static func text(_ bytes: ArraySlice<UInt8>) -> String {
        String(decoding: bytes, as: UTF8.self).replacingOccurrences(of: "\r\n", with: "\n")
    }
}

/// Everything the daemon does with one VM's Guest Service: the channel, jobs, file and copy operations.
public actor VMGuestService {
    static let maxFinishedJobs = 50

    let config: VMConfig
    let channel: GuestChannel
    private var jobs: [String: Job] = [:]
    private var running: [Int: Job] = [:]
    private var finishedOrder: [String] = []
    private var nextJob: Int
    /// Output that arrived for a pid before `exec.start` returned it, while a start is in flight.
    private var orphans: [Int: [GuestEvent]] = [:]
    private var startsInFlight = 0
    private var pump: Task<Void, Never>?
    /// Shares being synced now, with the task syncing each: one sync per share at a time.
    var syncing: [String: Task<Results.ShareSync, any Error>] = [:]
    /// Where each running sync is, for `share.status`.
    var syncProgress: [String: SyncStage] = [:]
    /// How often a sync asks the guest how its listing is going, and how long the listing may make no progress.
    var syncWatch = (interval: Duration.seconds(10), stall: Duration.seconds(120))
    /// Set when the guest was restored from a snapshot: its clock is from the past, so the next session starts with `time.sync`.
    private var syncTimeOnSession = false
    /// VZ shares: how each was mounted since the session began (`mountShares`).
    private var shareMounts: [Results.ShareMount] = []
    private var mounting: Task<Void, Never>?
    private var resizingPartition: Task<Void, Never>?
    /// A session arrived during an update; its mounts run once the update ends.
    private var mountsPending = false
    private var checking: Task<Void, Never>?
    private var sessionID = UUID()
    private var updateStatus = Results.GuestServiceCheck(state: "unknown", problem: "not checked yet")
    private let guestServiceDirectory: String
    public private(set) var isUpdating = false
    /// The `tmpfs` secrets the guest should hold now, with their values (`secrets.sync`); nil when none of the VM's
    /// harnesses declares one. Set by the daemon before `start`.
    var tmpfsSecrets: (@Sendable () async -> [(name: String, value: String)]?)?
    /// The boot (`bootId`) that was given a `tmpfs` secret: its memory may hold one, so it takes no live snapshot.
    private var secretsBoot: String?
    private var secretSync: Task<String?, Never>?
    /// The VM's harness profiles and the ones signed in to a subscription (`glov secret login`); nil when the VM has
    /// no harness. Set by the daemon before `start`.
    var harnessMode: (@Sendable () async -> (profiles: [HarnessProfile], subscribed: Set<String>)?)?
    private var modeSync: Task<String?, Never>?
    /// The VM's granted git secrets (`GitSignIn`); nil when it has none to set up or remove.
    var gitSignIns: (@Sendable () async -> [GitSignIn.Entry]?)?
    private var trust: (@Sendable () async -> [VMConfig.Egress.Trust]?)?
    private var trustSync: Task<String?, Never>?
    private var gitSync: Task<String?, Never>?

    /// The QEMU virtio-serial channel (`guestSocketPath`).
    public init(config: VMConfig, guestServiceDirectory: String = GuestServiceDistribution.directory) {
        self.init(config: config, channel: GuestChannel(socketPath: config.guestSocketPath, log: DaemonLog.vm(config.name)), guestServiceDirectory: guestServiceDirectory)
    }

    public init(config: VMConfig, channel: GuestChannel, guestServiceDirectory: String = GuestServiceDistribution.directory) {
        self.guestServiceDirectory = guestServiceDirectory
        self.config = config
        self.channel = channel
        // Log files outlive the daemon; never reuse a job id whose log is still on disk. Those of an earlier daemon
        // count as finished jobs: the newest are kept within `maxFinishedJobs`, and later jobs push them out in turn.
        let existing = (try? FileManager.default.contentsOfDirectory(atPath: config.jobsDirectory)) ?? []
        let numbers = existing.compactMap { $0.hasPrefix("j") && $0.hasSuffix(".log") ? Int($0.dropFirst().dropLast(4)) : nil }.sorted()
        nextJob = numbers.last.map { $0 + 1 } ?? 1
        for n in numbers.dropLast(Self.maxFinishedJobs) { try? FileManager.default.removeItem(atPath: config.jobsDirectory + "/j\(n).log") }
        finishedOrder = numbers.suffix(Self.maxFinishedJobs).map { "j\($0)" }
        // A maintenance job's log is removed when it ends; one left by a daemon that stopped first is of no use.
        for name in existing where name.hasPrefix("maintenance-") { try? FileManager.default.removeItem(atPath: config.jobsDirectory + "/" + name) }
    }

    public func start() async {
        let events = await channel.events()
        pump = Task { for await e in events { self.handle(e) } }
        await channel.start()
    }

    public func stop() async {
        mounting?.cancel()
        resizingPartition?.cancel()
        checking?.cancel()
        await channel.stop()
        pump?.cancel()
        for job in jobs.values { job.close() }
    }

    /// The guest was restored from a snapshot: jobs from before are gone, the session is reopened and the guest clock set once the Guest Service answers.
    public func guestRestored() async {
        syncTimeOnSession = true
        loseRunningJobs()
        await channel.reset()
    }

    /// Called for a VM started straight into a snapshot, whose channel is new.
    public func expectTimeSync() { syncTimeOnSession = true }

    /// The Mac woke: a QEMU guest's clock stood still while it slept. Set it now if the Guest Service answers,
    /// else when its next session opens. The flag goes first, so a session opening during the check takes it over.
    public func hostWoke() async {
        syncTimeOnSession = true
        guard await channel.isReady, syncTimeOnSession else {
            DaemonLog.write("\(config.name): host woke; setting the guest clock on its next session")
            return
        }
        syncTimeOnSession = false
        DaemonLog.write("\(config.name): host woke; setting the guest clock")
        await syncGuestClock()
    }

    func syncGuestClock() async {
        let millis = Int(Date().timeIntervalSince1970 * 1000)
        do { _ = try await channel.request("time.sync", ["unixMillis": .int(millis)]) } catch {
            DaemonLog.write("time.sync failed for \(config.name): \(error)")
        }
    }

    public var isReady: Bool { get async { await channel.isReady } }

    func setSyncWatchForTesting(interval: Duration, stall: Duration) { syncWatch = (interval, stall) }

    /// Whether `path` exists in the guest; false too when the Guest Service cannot answer.
    func guestFileExists(_ path: String) async -> Bool {
        guard let r = try? await channel.request("file.stat", ["path": .string(path)]) else { return false }
        return r["exists"] == .bool(true)
    }

    public func status() async -> Results.GuestServiceStatus {
        guard let info = await channel.info else {
            return .init(state: await channel.problem == nil ? "connecting" : "incompatible", problem: await channel.problem, update: .init(state: "unknown", problem: "Guest Service is not connected"))
        }
        let shares = shareMounts.isEmpty ? nil : shareMounts
        let busy = busyWork.map(\.label)
        if await channel.unresponsive {
            // The user and boot seen last may no longer be true: the guest may be rebooting.
            return .init(state: "unresponsive", os: info.os, version: info.version, problem: await channel.problem, shares: shares, update: .init(state: "unknown", currentVersion: info.version, problem: "Guest Service is not responding"), busy: busy)
        }
        return .init(
            state: "ready", os: info.os, version: info.version, user: info.user, bootId: info.bootId, problem: await channel.problem,
            shares: shares, update: updateStatus, connectionID: sessionID.uuidString, busy: busy)
    }

    /// The guest's `guest.stats`, or why there is none: not ready, too old, or no answer.
    func guestStats() async -> (Results.GuestStats?, String?) {
        let status = await status()
        guard status.state == "ready" else { return (nil, "Guest Service \(status.state)" + (status.problem.map { ": " + $0 } ?? "")) }
        do {
            let r = try await channel.request("guest.stats", timeout: .seconds(5), cancellable: true)
            return (try r.decode(Results.GuestStats.self), nil)
        } catch GuestServiceError.remote(let code, _) where code == RPCError.methodNotFound {
            return (nil, "Guest Service \(status.version ?? "") has no guest.stats; update it with 'glov vm guest update \(config.name)'")
        } catch {
            return (nil, "\(error)")
        }
    }

    // MARK: agent maintenance

    private func updater(binary: String, home: GlovHome) -> GuestServiceUpdate {
        GuestServiceUpdate(vm: config.name, binary: binary, home: home,
            status: { [self] in .init(name: config.name, state: "running", agent: await status()) },
            execute: { [self] argv, timeout in
                try await execRun(.init(vm: config.name, argv: argv, timeout: timeout), maintenance: true)
            }, copy: { [self] source, target in
                _ = try await copyToVM(.init(vm: config.name, hostPath: source, guestPath: target), current: try home.loadConfig(config.name))
            })
    }

    public func checkGuestService(binary: String? = nil) async -> Results.GuestServiceCheck {
        if isUpdating { return .init(state: "unknown", problem: "Guest Service update in progress") }
        let generation = sessionID
        let session = await channel.info
        var result: Results.GuestServiceCheck
        do {
            let os = session?.os ?? defaultOS
            let target = try await Blocking.run { [guestServiceDirectory] in try GuestServiceDistribution.target(os: os, binary: binary, directory: guestServiceDirectory) }
            var check = updater(binary: target.path, home: GlovHome(path: (config.directory as NSString).deletingLastPathComponent))
            check.targetVersion = target.version
            result = await check.check()
        } catch {
            result = .init(state: "unknown", currentVersion: session?.version, problem: "\(error)")
        }
        // A result from an old connection must never overwrite the next session's check.
        if binary == nil, generation == sessionID, !isUpdating, !Task.isCancelled { updateStatus = result }
        return result
    }

    public func updateGuestService(_ p: Params.GuestService, home: GlovHome) async throws -> GuestServiceUpdate.Outcome {
        guard !isUpdating else { throw GuestServiceUpdateError("a Guest Service update is already running") }
        let work = busyWork.map(\.label)
        let busy = work.isEmpty ? [] : [work.joined(separator: ", ")]
        guard busy.isEmpty || p.force == true else {
            throw GuestServiceUpdateError("\(busy.joined(separator: "; ")); wait for them or use --force to allow interruption")
        }
        // A sync can be repeated and holds no job: --force cancels it rather than waiting for it.
        if p.force == true { await cancelSyncs() }
        // --force interrupts jobs only; whatever else runs now (an exec start, a mount, or work begun during the wait
        // above) must finish first.
        let pending = busyWork.filter { $0.kind != .job }.map(\.label)
        guard pending.isEmpty else {
            throw GuestServiceUpdateError("\(pending.joined(separator: ", ")) in progress; wait and retry the Guest Service update (--force cancels a share sync, not an exec start or share mount)")
        }
        isUpdating = true
        defer {
            isUpdating = false
            if mountsPending {
                mountsPending = false
                mounting = Task { await self.mountShares() }
            }
        }
        // Let the session's read-only check finish before replacing the service.
        await checking?.value
        let os = (await channel.info)?.os ?? defaultOS
        let target = try await Blocking.run { [guestServiceDirectory] in try GuestServiceDistribution.target(os: os, binary: p.binary, directory: guestServiceDirectory) }
        var update = updater(binary: target.path, home: home)
        let timeout = p.timeout ?? 120
        guard timeout.isFinite, timeout > 0, timeout <= 3600 else { throw GuestServiceUpdateError("timeout must be between 0 and 3600 seconds") }
        update.timeout = timeout
        updateStatus = .init(state: "unknown", problem: "Guest Service update in progress")
        do {
            let outcome = try await update.run()
            isUpdating = false
            _ = await checkGuestService()
            return outcome
        } catch {
            updateStatus = .init(state: "unknown", currentVersion: (await channel.info)?.version, problem: "update failed: \(error)")
            throw error
        }
    }

    // MARK: reboot

    /// Readies the guest for `vm.reboot`: refuses while managed work runs, naming it, unless `force`, which cancels share
    /// syncs and mounts (a reboot ends jobs anyway, and a new session mounts the shares again).
    func prepareReboot(force: Bool) async throws {
        let busy = busyWork.map(\.label)
        guard busy.isEmpty || force else {
            throw RPCError(code: RPCError.failed, message: "VM \(config.name) is busy: \(busy.joined(separator: ", ")); wait for them, or reboot with --force to interrupt them")
        }
        guard force else { return }
        await cancelSyncs()
        mounting?.cancel()
    }

    // MARK: managed work

    /// One piece of managed work a Guest Service update or a reboot would cut off.
    struct BusyWork: Equatable {
        enum Kind { case job, execStart, shareSync, shareMount, update }
        var kind: Kind
        var label: String
    }

    /// Everything managed running in the guest, in the words `status().busy`, an update and a reboot all use. Maintenance
    /// execs (the Guest Service update's own) are left out.
    var busyWork: [BusyWork] {
        running.values.filter { !$0.maintenance }.map(\.id).sorted().map { .init(kind: .job, label: "job \($0)") }
            + (startsInFlight > 0 ? [.init(kind: .execStart, label: "an exec start")] : [])
            + syncing.keys.sorted().map { .init(kind: .shareSync, label: "share sync \($0)") }
            + shareMounts.filter { $0.state == "mounting" }.map { .init(kind: .shareMount, label: "share mount \($0.name)") }
            + (isUpdating ? [.init(kind: .update, label: "a Guest Service update")] : [])
    }

    /// Cancels every share sync and waits for each to end.
    private func cancelSyncs() async {
        let syncs = Array(syncing.values)
        for sync in syncs { sync.cancel() }
        for sync in syncs { _ = try? await sync.value }
    }

    /// The jobs from before a new session or a snapshot restore are gone: report each as lost.
    private func loseRunningJobs() {
        for job in running.values { finish(job, lost: true) }
        running = [:]
    }

    /// The guest OS to assume before the Guest Service has said.
    private var defaultOS: String { config.engine == .vz ? "linux" : "windows" }

    /// Asks a Windows Guest Service to restart the guest; it answers before the guest goes down.
    func requestReboot() async throws { _ = try await channel.request("power.reboot") }

    // MARK: share mounts

    /// Mounts each of the VM's shares (virtiofs devices attached at start) through the Guest Service's exec, as root: the
    /// directory is created, a path that already is a mount point is left alone. A failure is logged and shown in the
    /// agent status; the VM carries on. A new session (a reboot) runs this again, since mounts do not survive one.
    func mountShares() async {
        for (i, share) in config.shares.enumerated() {
            if Task.isCancelled { return }
            var state = Results.ShareMount(name: share.name, guestPath: share.guestPath, state: "mounted")
            do { try await mount(share) } catch {
                if Task.isCancelled { return }
                let why = (error as? ShareMountFailure)?.message ?? "\(error)"
                state.state = "failed"
                state.problem = why
                DaemonLog.write("\(config.name): cannot mount share \(share.name) at \(share.guestPath): \(why)")
            }
            if i < shareMounts.count, shareMounts[i].name == share.name { shareMounts[i] = state }
        }
    }

    /// Mounts one share as root without trusting the guest's file system: every directory from `/` down to the guest
    /// path must be a real directory owned by root and not writable by group or others (so no other user can swap a
    /// component for a link between the check and the mount); missing ones are created one at a time with mode 0755 and
    /// checked again. Any failed check skips the share. A path that already is a mount point is left alone.
    private func mount(_ share: VMConfig.Share) async throws {
        let components = ShareMount.components(of: share.guestPath)
        var exists = true
        for (i, path) in components.enumerated() {
            let last = i == components.count - 1
            if last, exists, try await run(ShareMount.isMountedArguments(share)).code == 0 { return }  // its root is the host's folder
            if exists {
                let r = try await run(ShareMount.statArguments(path))
                if r.code == 0 {
                    if let problem = ShareMount.directoryProblem(path, statOutput: r.tail) { throw ShareMountFailure(message: problem) }
                    continue
                }
                exists = false  // everything below a missing directory is missing too
            }
            let made = try await run(ShareMount.createComponentArguments(path))
            guard made.code == 0 else { throw ShareMountFailure(message: failure("mkdir", made)) }
            let r = try await run(ShareMount.statArguments(path))
            if r.code != 0 { throw ShareMountFailure(message: failure("stat", r)) }
            if let problem = ShareMount.directoryProblem(path, statOutput: r.tail) { throw ShareMountFailure(message: problem) }
        }
        let r = try await run(ShareMount.mountArguments(share))
        guard r.code == 0 else { throw ShareMountFailure(message: failure("mount", r)) }
    }

    private func failure(_ command: String, _ r: Results.ExecRun) -> String {
        let output = r.tail.trimmingCharacters(in: .whitespacesAndNewlines)
        return "\(command) " + (r.exited ? "exited with \(r.code ?? -1)" : "did not finish") + (output.isEmpty ? "" : ": " + output)
    }

    private struct ShareMountFailure: Error { var message: String }

    private func run(_ argv: [String]) async throws -> Results.ExecRun {
        try await execRun(.init(vm: config.name, argv: argv, timeout: 30))
    }

    private func resizeWindowsSystemPartition() async {
        let marker = VMConfigurator.pendingPartitionResizePath(config.directory)
        guard FileManager.default.fileExists(atPath: marker) else { return }
        do {
            let r = try await execRun(
                .init(vm: config.name, argv: ["powershell.exe", "-NoProfile", "-Command", VMConfigurator.windowsResizePartitionScript], timeout: 30),
                maintenance: true)
            if r.code == 0 {
                try? FileManager.default.removeItem(atPath: marker)
            } else {
                DaemonLog.write("\(config.name): cannot expand Windows system partition: \(failure("Resize-Partition", r))")
            }
        } catch {
            DaemonLog.write("\(config.name): cannot expand Windows system partition: \(error)")
        }
    }

    func setTmpfsSecrets(_ source: @escaping @Sendable () async -> [(name: String, value: String)]?) { tmpfsSecrets = source }

    /// Whether the guest was given a `tmpfs` secret since it booted; a live snapshot would save it.
    var holdsTmpfsSecrets: Bool {
        get async {
            guard let secretsBoot else { return false }
            guard let info = await channel.info else { return true }
            return info.bootId == secretsBoot
        }
    }

    /// Makes `/run/glov/secrets` hold exactly the VM's granted `tmpfs` secrets. One sync at a time, in call order, so
    /// an older list never lands after a newer one. Returns the problem, if any; it is logged by name, never by value.
    @discardableResult
    func syncSecrets() async -> String? {
        let previous = secretSync
        let task = Task { () -> String? in
            _ = await previous?.value
            return await self.deliverSecrets()
        }
        secretSync = task
        return await task.value
    }

    func setHarnessMode(_ source: @escaping @Sendable () async -> (profiles: [HarnessProfile], subscribed: Set<String>)?) { harnessMode = source }

    /// Points each harness in the guest at the subscription or at the API key, as `harnessMode` says: the keys of
    /// /etc/environment and the settings both modes set (`HarnessMode`), then the marker. Without `force` it does
    /// nothing when the marker already says so. One at a time, in call order. Returns the problem, if any.
    @discardableResult
    func syncHarnessMode(force: Bool) async -> String? {
        let previous = modeSync
        let task = Task { () -> String? in
            _ = await previous?.value
            return await self.applyHarnessMode(force: force)
        }
        modeSync = task
        return await task.value
    }

    private func applyHarnessMode(force: Bool) async -> String? {
        guard let harnessMode, let (profiles, subscribed) = await harnessMode() else { return nil }
        guard let info = await channel.info, info.os == "linux" else { return nil }
        do {
            let marker = try await readGuestText(HarnessMode.markerPath)
            if !force, HarnessMode.parseMarker(marker ?? "") == subscribed { return nil }
            let vm = config.name
            let environment = try await readGuestText("/etc/environment") ?? ""
            let wanted = HarnessMode.environment(environment, profiles: profiles, subscribed: subscribed, vm: vm)
            if wanted != environment { try await writeGuestText("/etc/environment", wanted, as: "system") }
            let edits = HarnessMode.edits(profiles: profiles, subscribed: subscribed, vm: vm)
            var paths: [String] = []
            for edit in edits where !paths.contains(edit.path) { paths.append(edit.path) }
            for path in paths {
                let full = "/home/\(CloudInitSeed.user)/\(path)"
                let text = try await readGuestText(full) ?? (path.hasSuffix(".toml") ? "" : "{}")
                guard let edited = HarnessMode.apply(edits.filter { $0.path == path }, to: text, path: path) else {
                    throw RPCError(code: RPCError.failed, message: "~/\(path) is not valid JSON; fix or remove it, then glov secret login again")
                }
                if edited != text { try await writeGuestText(full, edited, as: "user") }
            }
            try await writeGuestText(HarnessMode.markerPath, HarnessMode.marker(subscribed), as: "system")
            DaemonLog.write("\(vm): harnesses on subscriptions now \(subscribed.isEmpty ? "none" : subscribed.sorted().joined(separator: ", "))")
            return nil
        } catch {
            let problem = "cannot switch the harness sign-in: \(error)"
            DaemonLog.write("\(config.name): \(problem)")
            return problem
        }
    }

    func setGitSignIns(_ source: @escaping @Sendable () async -> [GitSignIn.Entry]?) { gitSignIns = source }

    func setTrust(_ source: @escaping @Sendable () async -> [VMConfig.Egress.Trust]?) { trust = source }

    /// Makes the guest's `trustDirectory` hold exactly the VM's trusted certificates and rebuilds the system bundle when
    /// that changed. One at a time, in call order. Returns the problem, if any.
    @discardableResult
    func syncTrust() async -> String? {
        let previous = trustSync
        let task = Task { () -> String? in
            _ = await previous?.value
            return await self.applyTrust()
        }
        trustSync = task
        return await task.value
    }

    /// Where the guest keeps `glov vm trust` certificates; update-ca-certificates reads subdirectories too.
    static let trustDirectory = "/usr/local/share/ca-certificates/glov-trust"

    /// A VM made before Node read the system bundle has `NODE_EXTRA_CA_CERTS` naming the egress CA alone in
    /// /etc/environment, which first boot writes once: point it at the bundle, so login shells (ssh, a harness's tmux)
    /// trust `glov vm trust` certificates too. A VM without the line (no egress CA) is left alone.
    static func nodeTrustFix(environment e: String = "/etc/environment") -> String {
        let b = CloudInitSeed.systemBundlePath
        return #"""
            if grep -q '^NODE_EXTRA_CA_CERTS=' "\#(e)" && ! grep -qx 'NODE_EXTRA_CA_CERTS=\#(b)' "\#(e)"; then
            sed 's#^NODE_EXTRA_CA_CERTS=.*#NODE_EXTRA_CA_CERTS=\#(b)#' "\#(e)" > "\#(e).glov~"; chmod 644 "\#(e).glov~"; mv -f "\#(e).glov~" "\#(e)"
            fi
            """# + "\n"
    }

    /// One script that writes the certificates to a temporary directory and swaps it in only when it differs. Names
    /// and PEM text are safe to embed: `TrustedCertificate` allows only `[A-Za-z0-9._-]` names and re-encodes the PEM.
    static func trustScript(_ list: [VMConfig.Egress.Trust]) -> String {
        var s = "set -e\n" + nodeTrustFix() + #"d=\#(trustDirectory); t=$(mktemp -d)"# + "\n"
        for item in list { s += "cat > \"$t/\(item.name).crt\" <<'GLOV_PEM'\n\(item.certificate)GLOV_PEM\n" }
        s += #"""
            if [ -z "$(ls -A "$t")" ] && [ ! -e "$d" ]; then rm -rf "$t"; exit 0; fi
            if [ -d "$d" ] && diff -r -q "$t" "$d" >/dev/null 2>&1; then rm -rf "$t"; exit 0; fi
            rm -rf "$d"
            if [ -n "$(ls -A "$t")" ]; then chmod 755 "$t"; chmod 644 "$t"/*; mv "$t" "$d"; else rm -rf "$t"; fi
            update-ca-certificates --fresh >/dev/null
            echo changed
            """#
        return s
    }

    private func applyTrust() async -> String? {
        guard let trust, let list = await trust() else { return nil }
        guard let info = await channel.info, info.os == "linux" else { return nil }
        do {
            let r = try await execRun(.init(vm: config.name, argv: ["sh", "-c", Self.trustScript(list)], as: "system", timeout: 60), maintenance: true)
            guard r.exited, r.code == 0 else { throw RPCError(code: RPCError.failed, message: failure("update the trust store", r)) }
            if r.tail.contains("changed") {
                DaemonLog.write("\(config.name): trusted certificates now \(list.isEmpty ? "none" : list.map(\.name).joined(separator: ", "))")
            }
            return nil
        } catch {
            let problem = "cannot update the trust store: \(error)"
            DaemonLog.write("\(config.name): \(problem)")
            return problem
        }
    }

    /// Sets git, gh and glab in the guest up for the VM's granted git secrets (`GitSignIn`), writing only files that
    /// differ. One at a time, in call order. Returns the problem, if any.
    @discardableResult
    func syncGitSignIns() async -> String? {
        let previous = gitSync
        let task = Task { () -> String? in
            _ = await previous?.value
            return await self.applyGitSignIns()
        }
        gitSync = task
        return await task.value
    }

    private func applyGitSignIns() async -> String? {
        guard let gitSignIns, let entries = await gitSignIns() else { return nil }
        guard let info = await channel.info, info.os == "linux" else { return nil }
        let home = "/home/\(CloudInitSeed.user)/"
        do {
            if !entries.isEmpty { try await put(GitSignIn.helperPath, GitSignIn.helper, as: "system", mode: "755") }
            try await put(GitSignIn.signInsPath, GitSignIn.signIns(entries), as: "system", mode: "644")
            let gitConfig = try await readGuestText(GitSignIn.gitConfigPath) ?? ""
            let wanted = GitSignIn.gitConfig(gitConfig, entries: entries)
            if wanted != gitConfig { try await writeGuestText(GitSignIn.gitConfigPath, wanted, as: "system", mode: "644") }
            // The guest user's files, written as that user by root: a VM without a harness has no console sign-in
            // for exec as the user.
            try await put(home + GitSignIn.ghHostsPath, GitSignIn.ghHosts(entries), as: CloudInitSeed.user, mode: "600")
            try await put(home + GitSignIn.glabConfigPath, GitSignIn.glabConfig(entries), as: CloudInitSeed.user, mode: "600")
            DaemonLog.write("\(config.name): git sign-ins now \(entries.isEmpty ? "none" : entries.map(\.name).sorted().joined(separator: ", "))")
            return nil
        } catch {
            let problem = "cannot set up git sign-ins: \(error)"
            DaemonLog.write("\(config.name): \(problem)")
            return problem
        }
    }

    /// Writes `text` to `path` when it differs, as "system" or as the named guest user; nil removes a file glov wrote
    /// (one holding a placeholder).
    private func put(_ path: String, _ text: String?, as runAs: String, mode: String) async throws {
        let current = try await readGuestText(path)
        guard let text else {
            guard let current, current.contains("glov-secret:") else { return }
            let r = try await execRun(.init(vm: config.name, argv: ["rm", "-f", path], as: "system", timeout: 20), maintenance: true)
            guard r.exited, r.code == 0 else { throw RPCError(code: RPCError.failed, message: failure("remove \(path)", r)) }
            return
        }
        if current != text { try await writeGuestText(path, text, as: runAs, mode: mode) }
    }

    /// A guest text file, nil when it does not exist. Harness settings are small: one read.
    private func readGuestText(_ path: String) async throws -> String? {
        let stat = try await channel.request("file.stat", ["path": .string(path)])
        guard stat["exists"] == .bool(true) else { return nil }
        let r = try await channel.request("file.read", ["path": .string(path), "offset": .int(0), "length": .int(524_288)])
        guard case .string(let b64)? = r["data"], let data = Data(base64Encoded: b64) else { throw GuestServiceError.protocolViolation("file.read returned no data") }
        guard r["eof"] == .bool(true) || data.count < 524_288 else { throw RPCError(code: RPCError.failed, message: "\(path) is too large") }
        return String(decoding: data, as: UTF8.self)
    }

    /// Replaces a guest file whole, written by `as` ("system" or "user") so a user's file stays the user's.
    /// `runAs` other than "system" or "user" names a guest account root writes as (`runuser`).
    private func writeGuestText(_ path: String, _ text: String, as runAs: String, mode: String? = nil) async throws {
        let script = #"set -e; mkdir -p "$(dirname "$1")"; cat > "$1.glov~"; if [ -n "$2" ]; then chmod "$2" "$1.glov~"; fi; mv -f "$1.glov~" "$1""#
        var argv = ["sh", "-c", script, "sh", path, mode ?? ""], account = runAs
        if !["system", "user"].contains(runAs) { argv = ["runuser", "-u", runAs, "--"] + argv; account = "system" }
        let r = try await execRun(.init(vm: config.name, argv: argv, as: account, stdin: text, timeout: 20), maintenance: true)
        guard r.exited, r.code == 0 else { throw RPCError(code: RPCError.failed, message: failure("write \(path)", r)) }
    }

    private func deliverSecrets() async -> String? {
        guard let tmpfsSecrets, let list = await tmpfsSecrets() else { return nil }
        guard let info = await channel.info, info.os == "linux" else { return nil }
        let params: JSONValue = .object([
            "owner": .string(CloudInitSeed.user),
            "secrets": .array(list.map { .object(["name": .string($0.name), "data": .string(Data($0.value.utf8).base64EncodedString())]) }),
        ])
        // Before the request: a guest whose answer is lost may have written them all the same.
        if !list.isEmpty { secretsBoot = info.bootId }
        do {
            _ = try await channel.request("secrets.sync", params)
            DaemonLog.write("\(config.name): tmpfs secrets now \(list.isEmpty ? "none" : list.map(\.name).joined(separator: ", "))")
            return nil
        } catch {
            let problem = "cannot deliver tmpfs secrets (\(list.map(\.name).joined(separator: ", "))): \(error)"
            DaemonLog.write("\(config.name): \(problem)")
            return problem
        }
    }

    // MARK: events

    private func handle(_ event: GuestEvent) {
        switch event {
        case .session:
            sessionID = UUID()
            checking?.cancel()
            updateStatus = .init(state: "unknown", problem: isUpdating ? "Guest Service update in progress" : "check in progress")
            if !isUpdating { checking = Task { _ = await self.checkGuestService() } }
            // A new session (a reconnect, or a new boot): whatever was running before is no longer reported on.
            loseRunningJobs()
            if syncTimeOnSession {
                syncTimeOnSession = false
                Task { await self.syncGuestClock() }
            }
            // tmpfs does not outlive a boot; whatever the guest holds now is brought to what is granted now.
            if tmpfsSecrets != nil { Task { await self.syncSecrets() } }
            // A sign-in or sign-out while the guest was away (or a revert) takes effect now.
            if harnessMode != nil { Task { await self.syncHarnessMode(force: false) } }
            if gitSignIns != nil { Task { await self.syncGitSignIns() } }
            if trust != nil { Task { await self.syncTrust() } }
            if config.engine == .qemu, config.install == nil, FileManager.default.fileExists(atPath: VMConfigurator.pendingPartitionResizePath(config.directory)) {
                resizingPartition?.cancel()
                if !isUpdating { resizingPartition = Task { await self.resizeWindowsSystemPartition() } }
            }
            if config.engine == .vz, !config.shares.isEmpty {
                mounting?.cancel()
                shareMounts = config.shares.map { .init(name: $0.name, guestPath: $0.guestPath, state: "mounting") }
                // An update restarts the Guest Service before releasing its exec guard; mount after that guard is released.
                if isUpdating { mountsPending = true } else { mounting = Task { await self.mountShares() } }
            }
        case .output(let pid, _, let data):
            if let job = running[pid] { job.append(data) } else if startsInFlight > 0 { orphans[pid, default: []].append(event) }
        case .exit(let pid, let code):
            if let job = running[pid] {
                job.code = code
                running[pid] = nil
                finish(job, lost: false)
            } else if startsInFlight > 0 {
                orphans[pid, default: []].append(event)
            }
        }
    }

    private func finish(_ job: Job, lost: Bool) {
        if lost { job.lost = true }
        job.close()
        if job.maintenance {
            jobs[job.id] = nil
            try? FileManager.default.removeItem(atPath: job.logPath)
            return
        }
        finishedOrder.append(job.id)
        while finishedOrder.count > Self.maxFinishedJobs {
            let old = finishedOrder.removeFirst()
            jobs[old] = nil
            try? FileManager.default.removeItem(atPath: config.jobsDirectory + "/" + old + ".log")
        }
    }

    // MARK: exec

    private func startJob(_ p: Params.Exec, maintenance: Bool = false) async throws -> Job {
        guard !isUpdating || maintenance else { throw GuestServiceUpdateError("Guest Service update in progress; retry after it finishes") }
        guard !p.argv.isEmpty else { throw RPCError(code: RPCError.invalidParams, message: "argv must not be empty") }
        var params: [String: JSONValue] = ["argv": .array(p.argv.map { .string($0) }), "stdin": .bool(p.stdin != nil)]
        if let cwd = p.cwd { params["cwd"] = .string(cwd) }
        if let env = p.env { params["env"] = .object(env.mapValues { .string($0) }) }
        if let user = p.as { params["as"] = .string(user) }

        startsInFlight += 1
        defer { startsInFlight -= 1; if startsInFlight == 0 { orphans = [:] } }
        let result = try await channel.request("exec.start", .object(params))
        guard case .int(let pid)? = result["pid"] else { throw GuestServiceError.protocolViolation("exec.start returned no pid") }

        try FileManager.default.createDirectory(atPath: config.jobsDirectory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let id = maintenance ? "maintenance-" + UUID().uuidString : "j\(nextJob)"
        let job = try Job(id: id, pid: pid, logPath: config.jobsDirectory + "/" + id + ".log")
        job.maintenance = maintenance
        if !maintenance { nextJob += 1 }
        jobs[job.id] = job
        running[pid] = job
        for event in orphans.removeValue(forKey: pid) ?? [] { handle(event) }

        if let text = p.stdin {
            let data = JSONValue.object(["pid": .int(pid), "data": .string(Data(text.utf8).base64EncodedString()), "eof": true])
            do { _ = try await channel.request("exec.stdin", data) } catch {
                _ = try? await channel.request("exec.kill", ["pid": .int(pid)])
                throw error
            }
        }
        return job
    }

    func execStart(_ p: Params.Exec) async throws -> Results.JobStarted { .init(jobId: try await startJob(p).id) }

    func execRun(_ p: Params.Exec, maintenance: Bool = false) async throws -> Results.ExecRun {
        let job = try await startJob(p, maintenance: maintenance)
        let deadline = ContinuousClock.now + .seconds(min(max(p.timeout ?? 30, 0), 50))
        // shortcut: polling every 25 ms, fine for a 50 s wait; use continuations if many runs wait at once.
        while !job.finished && ContinuousClock.now < deadline { try await Task.sleep(for: .milliseconds(25)) }
        return .init(
            jobId: job.id, exited: job.code != nil, code: job.code, lost: job.lost, tail: job.tail(), lineCount: job.lineCount, logPath: job.logPath)
    }

    private func job(_ id: String) throws -> Job {
        guard let job = jobs[id] else {
            throw RPCError(code: RPCError.failed, message: "no job \(String(reflecting: id)) on this VM (known: \(jobs.keys.sorted().joined(separator: ", ")); only the last \(Self.maxFinishedJobs) finished jobs are kept)")
        }
        return job
    }

    func execRead(_ p: Params.ExecRead) throws -> Results.ExecRead {
        let job = try job(p.jobId)
        let wanted = min(max(p.cursor ?? 0, 0), job.total)
        let from = max(wanted, job.ringStart)
        let limit = min(max(p.maxBytes ?? 32 * 1024, 1), 256 * 1024)
        let end = min(job.total, from + limit)
        let slice = job.ring[(from - job.ringStart)..<(end - job.ringStart)]
        // A UTF-8 sequence cut by maxBytes is left for the next read, unless nothing more will come.
        let keep = end == job.total && job.finished ? slice.count : Job.completeLength(slice)
        return .init(
            data: Job.text(slice.prefix(keep)), cursor: from + keep, exited: job.code != nil, code: job.code, lost: job.lost,
            dropped: wanted < job.ringStart, logPath: job.logPath)
    }

    func execKill(_ p: Params.ExecKill) async throws {
        let job = try job(p.jobId)
        if job.lost { throw RPCError(code: RPCError.failed, message: "job \(job.id) was lost when the Guest Service reconnected; it may still run in the guest, so kill it by name with exec") }
        if let code = job.code { throw RPCError(code: RPCError.failed, message: "job \(job.id) already exited with code \(code)") }
        _ = try await channel.request("exec.kill", ["pid": .int(job.pid)])
    }
}
