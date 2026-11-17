import Foundation
import GlovCore
import GlovDaemonAPI

/// `share.sync`: mirrors a share's host folder into its guest path through the Guest Service's `sync.manifest` and
/// `sync.apply` (docs/guest-protocol.md, "Share sync").
extension VMGuestService {
    /// A `sync.apply` call stays well under the 1 MiB frame: data is base64 (4/3) and each op carries its path.
    static let applyBudget = 900 * 1024
    static let applyChunk = 512 * 1024
    static let applyMaxOps = 256

    /// Runs the sync in a task of its own, kept in `syncing`, so that `glov vm guest update --force` can cancel it; cancelling
    /// the caller (its client went away) cancels it too.
    func shareSync(_ share: VMConfig.Share, subdir: String, verify: Bool, cacheDirectory: String) async throws -> Results.ShareSync {
        guard !isUpdating, syncing[share.name] == nil else {
            throw RPCError(code: RPCError.failed, message: "a sync of share \(share.name) is already running on VM \(config.name)")
        }
        // The task frees the share itself, so that whoever awaits it (Guest Service update) finds the share free. It runs on
        // this actor, so not before the entry below is made.
        syncProgress[share.name] = SyncStage(label: subdir.isEmpty ? share.name : share.name + "/" + subdir)
        let task = Task {
            defer { syncing[share.name] = nil; syncProgress[share.name] = nil }
            return try await syncCancellably(share, subdir: subdir, verify: verify, cacheDirectory: cacheDirectory)
        }
        syncing[share.name] = task
        return try await withTaskCancellationHandler { try await task.value } onCancel: { task.cancel() }
    }

    /// Where a running sync is; `share.status` adds what the guest reports.
    struct SyncStage {
        var label: String
        var stage = "starting"
        var started = ContinuousClock.now
        var sentBytes = 0
        var totalBytes = 0
    }

    /// The running syncs (of `share` only, when given), each with the guest's `sync.status` while the guest is busy
    /// with it. A Guest Service without `sync.status`, or one that does not answer within 5 s, leaves `guest` nil.
    func shareStatus(_ share: String?) async -> [Results.SyncProgress] {
        var list: [Results.SyncProgress] = []
        for (name, s) in syncProgress.sorted(by: { $0.key < $1.key }) where share == nil || share == name {
            var p = Results.SyncProgress(
                share: s.label, stage: s.stage, seconds: (ContinuousClock.now - s.started) / .seconds(1), sentBytes: s.sentBytes, totalBytes: s.totalBytes)
            if s.stage == "guest listing" || s.stage == "applying",
                let r = try? await channel.request("sync.status", ["id": .string(name)], timeout: .seconds(5), cancellable: true),
                r["busy"] == .bool(true)
            {
                func int(_ k: String) -> Int { if case .int(let n)? = r[k] { n } else { 0 } }
                func str(_ k: String) -> String { if case .string(let v)? = r[k] { v } else { "" } }
                var seconds = 0.0
                if case .double(let d)? = r["seconds"] { seconds = d } else if case .int(let n)? = r["seconds"] { seconds = Double(n) }
                p.guest = .init(phase: str("phase"), path: str("path"), files: int("files"), bytes: int("bytes"), seconds: seconds)
            }
            list.append(p)
        }
        return list
    }

    /// A listing that made no progress for `syncWatch.stall`, or that the guest no longer works on while its answer is missing.
    struct SyncStalled: Error, CustomStringConvertible {
        var description: String
    }

    private func syncCancellably(_ share: VMConfig.Share, subdir: String, verify: Bool, cacheDirectory: String) async throws -> Results.ShareSync {
        let label = subdir.isEmpty ? share.name : share.name + "/" + subdir
        do {
            return try await sync(share, label: label, subdir: subdir, verify: verify, cacheDirectory: cacheDirectory)
        } catch let error as SyncStalled {
            await stopGuestSync(share)
            log("share sync of \(label) stopped: \(error)")
            throw RPCError(code: RPCError.failed, message: "share sync of \(label) to VM \(config.name) stopped: \(error)")
        } catch where Task.isCancelled {
            await stopGuestSync(share)
            log("share sync of \(label) cancelled")
            throw RPCError(code: RPCError.failed, message: "share sync of \(label) to VM \(config.name) was cancelled")
        }
    }

    /// Tells the guest to stop the share's listing or batch. Awaited, in a task of its own (the caller's is cancelled),
    /// so that the cancel reaches the guest before the next sync's first call. An older Guest Service does not know it.
    private func stopGuestSync(_ share: VMConfig.Share) async {
        await Task { _ = try? await channel.request("sync.cancel", ["id": .string(share.name)], timeout: .seconds(5)) }.value
    }

    private func log(_ message: String) { DaemonLog.write("\(config.name): \(message)") }

    private func sync(_ share: VMConfig.Share, label: String, subdir: String, verify: Bool, cacheDirectory: String) async throws -> Results.ShareSync {
        let started = ContinuousClock.now
        // Matching excludes runs regular expressions over every path: it runs off the actor, with the scan, so an
        // unlucky pattern cannot hold up the Guest Service's requests and heartbeats.
        let excludes = try ShareExcludes(share.exclude)
        if !subdir.isEmpty, await Blocking.run({ excludes.excludesPathOrParent(subdir, isDir: true) }) {
            throw RPCError(code: RPCError.failed, message: "\(subdir) is excluded from share \(share.name)")
        }
        var isDir = ObjCBool(false)
        guard FileManager.default.fileExists(atPath: share.hostPath, isDirectory: &isDir), isDir.boolValue, let root = HostPath.realpath(share.hostPath) else {
            throw RPCError(code: RPCError.failed, message: "the host folder of share \(share.name) is missing: \(share.hostPath); nothing was synced")
        }
        // The subdirectory comes from the caller: it must resolve to a directory inside the share, through no link.
        let scopePath = subdir.isEmpty ? root : root + "/" + subdir
        guard let real = HostPath.realpath(scopePath), FileManager.default.fileExists(atPath: real, isDirectory: &isDir), isDir.boolValue else {
            throw RPCError(code: RPCError.failed, message: "no directory \(subdir) in share \(share.name) (\(scopePath))")
        }
        guard real == scopePath else {
            throw RPCError(code: RPCError.failed, message: "\(subdir) in share \(share.name) leads through a symbolic link (to \(real)); nothing was synced")
        }

        // The scan runs on a thread of its own; it checks the flag between files, since it cannot see this task.
        let cancelled = CancelFlag()
        syncProgress[share.name]?.stage = "host scan"
        let host = try await withTaskCancellationHandler {
            try await Blocking.run {
                try HostTree.scan(
                    root: root, scope: subdir, excludes: excludes, cachePath: cacheDirectory + "/" + share.name + ".json", verify: verify,
                    cancelled: { cancelled.isSet })
            }
        } onCancel: { cancelled.set() }
        try Task.checkCancellation()
        syncProgress[share.name]?.stage = "guest listing"
        let guest = try await guestManifest(share, subdir: subdir, excludes: excludes, verify: verify)
        let guestFiles = guest.files, guestDirs = guest.dirs
        let plan = await Blocking.run {
            SyncPlan(hostFiles: host.files, hostDirs: host.dirs, guestFiles: guestFiles, guestDirs: guestDirs, excludes: excludes, untouched: host.skipped)
        }

        var ops: [JSONValue] = [["op": "mkdir", "path": .string(subdir)]]
        ops += plan.deletes.map { ["op": "delete", "path": .string($0)] }
        ops += plan.rmdirs.map { ["op": "rmdir", "path": .string($0)] }
        ops += plan.mkdirs.map { ["op": "mkdir", "path": .string($0)] }
        var failures: [SyncFailure] = []
        syncProgress[share.name]?.stage = "applying"
        syncProgress[share.name]?.totalBytes = plan.writes.reduce(0) { $0 + (host.files[$1]?.size ?? 0) }
        let bytes = try await apply(share, hostRoot: root, ops: ops, writes: plan.writes.map { ($0, host.files[$0]!.hash) }, failures: &failures)

        let failed = Set(failures.map(\.path))
        let limit = Results.ShareSync.listLimit
        let result = Results.ShareSync(
            share: label, guestPath: subdir.isEmpty ? share.guestPath : share.guestPath + "\\" + subdir.replacingOccurrences(of: "/", with: "\\"), written: plan.writes.count { !failed.contains($0) },
            deleted: plan.deletes.count { !failed.contains($0) }, unchanged: plan.unchanged, bytes: bytes, hostHashed: host.hashed,
            guestHashed: guest.hashed, writtenPaths: Array(plan.writes.filter { !failed.contains($0) }.prefix(limit)),
            deletedPaths: Array(plan.deletes.filter { !failed.contains($0) }.prefix(limit)), overwritten: Array(plan.overwritten.prefix(limit)),
            skipped: Array(host.skipped.sorted().prefix(limit)), seconds: (ContinuousClock.now - started) / .seconds(1))
        guard failures.isEmpty else {
            var message = "share sync of \(label) to VM \(config.name) failed for \(failures.count) file(s)"
            // Windows reports replacing or deleting a held file as access denied, as it does a read-only file.
            let groups: [(String, (SyncFailure) -> Bool)] = [
                ("in use in the guest (close the program holding them, then sync again)", { $0.locked && !$0.denied }),
                ("in use or access denied in the guest (close the program holding them, or clear their read-only attribute)", { $0.locked && $0.denied }),
                ("access denied in the guest (read-only, or not writable by the Guest Service)", { !$0.locked && $0.denied }),
                ("failed", { !$0.locked && !$0.denied }),
            ]
            for (reason, matches) in groups {
                let some = failures.filter(matches)
                if !some.isEmpty { message += "; \(reason): " + some.prefix(limit).map { "\($0.path) (\($0.message))" }.joined(separator: ", ") }
            }
            message += ". The rest was synced: \(result.written) written, \(result.deleted) deleted, \(result.unchanged) unchanged"
            if !result.overwritten.isEmpty { message += "; guest changes overwritten: " + result.overwritten.joined(separator: ", ") }
            throw RPCError(code: RPCError.failed, message: message)
        }
        return result
    }

    private struct GuestTree {
        var files: [String: SyncPlan.GuestFile] = [:]
        var dirs: Set<String> = []
        var hashed = 0
    }

    private func guestManifest(_ share: VMConfig.Share, subdir: String, excludes: ShareExcludes, verify: Bool) async throws -> GuestTree {
        var params: [String: JSONValue] = [
            "id": .string(share.name), "root": .string(share.guestPath), "subdir": .string(subdir), "verify": .bool(verify),
            "exclude": try JSONValue(encoding: excludes.rules),
        ]
        var tree = GuestTree()
        var received = 0
        while true {
            params["offset"] = .int(received)
            // The first page walks and hashes the guest tree; the Guest Service answers heartbeats meanwhile, and sync.status
            // tells a long walk from a stuck one.
            let timeout: Duration = .seconds(received == 0 ? 3600 : 60)
            let request: @Sendable () async throws -> JSONValue = { [params] in
                try await self.syncRequest("sync.manifest", .object(params), timeout: timeout)
            }
            let page: JSONValue = try await received == 0 ? watchingProgress(share, request) : request()
            guard case .array(let entries)? = page["entries"], case .int(let total)? = page["total"] else {
                throw GuestServiceError.protocolViolation("sync.manifest returned no entries")
            }
            if received == 0, case .int(let n)? = page["hashed"] { tree.hashed = n }
            for e in entries {
                guard case .string(let path)? = e["path"] else { continue }
                if e["dir"] == .bool(true) {
                    tree.dirs.insert(path)
                } else {
                    var hash = ""
                    if case .string(let h)? = e["hash"] { hash = h }
                    var size = 0
                    if case .int(let n)? = e["size"] { size = n }
                    tree.files[path] = .init(size: size, hash: hash, modified: e["modified"] == .bool(true))
                }
            }
            received += entries.count
            if received >= total || entries.isEmpty { return tree }
        }
    }

    typealias SyncFailure = (path: String, message: String, locked: Bool, denied: Bool)

    private struct Batch {
        var ops: [JSONValue] = []
        var size = 0
    }

    /// Sends `ops`, then each of `writes` (path and host hash) streamed from the host folder, in `sync.apply` calls of at
    /// most `applyBudget` bytes; the last one commits the guest manifest. Returns the bytes sent.
    private func apply(
        _ share: VMConfig.Share, hostRoot: String, ops: [JSONValue], writes: [(path: String, hash: String)], failures: inout [SyncFailure]
    ) async throws -> Int {
        var batch = Batch()
        var sent = 0
        for op in ops {
            var cost = 64
            if case .string(let p)? = op["path"] { cost += p.utf8.count }
            try await add(op, cost: cost, to: &batch, share, &failures)
        }
        for w in writes {
            // Opened without following links: a file swapped for a symbolic link since the scan is refused.
            let handle: FileHandle
            do { handle = FileHandle(fileDescriptor: try HostTree.openBelow(root: hostRoot, w.path, directory: false), closeOnDealloc: true) } catch {
                failures.append((w.path, "not sent: \(error)", false, false))
                continue
            }
            defer { try? handle.close() }
            var offset = 0
            while true {
                // Up to the room left in this call, so small files share one; a fresh call takes a full chunk.
                let room = Self.applyBudget - batch.size - 200 - w.path.utf8.count
                let want = room >= 4096 ? min(Self.applyChunk, room * 3 / 4) : Self.applyChunk
                let data = try await Blocking.run { try handle.read(upToCount: want) ?? Data() }
                // A short read is the end; a file that ends on a chunk boundary gets an empty last op.
                let end = data.count < want
                var op: [String: JSONValue] = ["op": "write", "path": .string(w.path), "offset": .int(offset), "data": .string(data.base64EncodedString())]
                if end {
                    op["end"] = true
                    op["hash"] = .string(w.hash)
                }
                try await add(.object(op), cost: 200 + w.path.utf8.count + (data.count + 2) / 3 * 4, to: &batch, share, &failures)
                offset += data.count
                sent += data.count
                syncProgress[share.name]?.sentBytes = sent
                if end { break }
            }
        }
        try await flush(&batch, share, commit: true, &failures)
        return sent
    }

    private func add(_ op: JSONValue, cost: Int, to batch: inout Batch, _ share: VMConfig.Share, _ failures: inout [SyncFailure]) async throws {
        if !batch.ops.isEmpty && (batch.size + cost > Self.applyBudget || batch.ops.count >= Self.applyMaxOps) {
            try await flush(&batch, share, commit: false, &failures)
        }
        batch.ops.append(op)
        batch.size += cost
    }

    private func flush(_ batch: inout Batch, _ share: VMConfig.Share, commit: Bool, _ failures: inout [SyncFailure]) async throws {
        let params: JSONValue = ["id": .string(share.name), "root": .string(share.guestPath), "ops": .array(batch.ops), "commit": .bool(commit)]
        batch = Batch()
        let r = try await syncRequest("sync.apply", params, timeout: .seconds(120))
        guard case .array(let list)? = r["failures"] else { return }
        for f in list {
            guard case .string(let path)? = f["path"] else { continue }
            var message = ""
            if case .string(let m)? = f["message"] { message = m }
            failures.append((path, message, f["locked"] == .bool(true), f["denied"] == .bool(true)))
        }
    }

    /// Runs `request` while asking the guest every `syncWatch.interval` how far the share's call has got; throws
    /// `SyncStalled`, naming the file it is on, once nothing moved for `syncWatch.stall`, or as soon as the guest says
    /// three times in a row that it is idle while the answer is still missing: the request or its answer was lost on the way
    ///. An older Guest Service has no sync.status: then the request just runs to its timeout.
    private func watchingProgress(_ share: VMConfig.Share, _ request: @escaping @Sendable () async throws -> JSONValue) async throws -> JSONValue {
        let watch = syncWatch
        return try await withThrowingTaskGroup(of: JSONValue?.self) { group in
            group.addTask { try await request() }
            group.addTask { [channel, name = config.name] in
                var last: JSONValue?
                var since = ContinuousClock.now
                var idlePolls = 0
                while true {
                    try await Task.sleep(for: watch.interval)
                    let status: JSONValue
                    do {
                        status = try await channel.request("sync.status", ["id": .string(share.name)], timeout: .seconds(5), cancellable: true)
                    } catch GuestServiceError.remote(let code, _) where code == RPCError.methodNotFound {
                        return nil
                    } catch is CancellationError {
                        throw CancellationError()
                    } catch {
                        continue  // a busy or briefly silent agent: counts as no progress
                    }
                    // The guest ends its progress before it sends the answer, and a request slow to arrive finds it idle too:
                    // three polls in a row leave both, and a wait for an earlier call's lock (10 s), time to end.
                    idlePolls = status["busy"] == .bool(false) ? idlePolls + 1 : 0
                    if idlePolls >= 3 {
                        throw SyncStalled(description: "the guest is not listing, yet its listing never reached the host; "
                            + "sync again, and if this repeats, run glov vm reboot \(name)")
                    }
                    let moved: JSONValue = ["phase": status["phase"] ?? .null, "path": status["path"] ?? .null, "files": status["files"] ?? .null, "bytes": status["bytes"] ?? .null]
                    if moved != last {
                        last = moved
                        since = .now
                    } else if ContinuousClock.now - since >= watch.stall {
                        var path = ""
                        if case .string(let p)? = status["path"] { path = p }
                        throw SyncStalled(description: status["busy"] == .bool(true) && !path.isEmpty
                            ? "the guest made no progress for \(watch.stall) reading \(path) (another program may hold it)"
                            : "the guest made no progress for \(watch.stall)")
                    }
                }
            }
            defer { group.cancelAll() }
            while let result = try await group.next() {
                if let page = result { return page }
            }
            throw CancellationError()
        }
    }

    private func syncRequest(_ method: String, _ params: JSONValue, timeout: Duration) async throws -> JSONValue {
        do { return try await channel.request(method, params, timeout: timeout, cancellable: true) } catch GuestServiceError.remote(let code, _) where code == RPCError.methodNotFound {
            throw RPCError(
                code: RPCError.failed,
                message: "the Guest Service of VM \(config.name) is too old for share sync (no \(method)): update glov-guest in the guest")
        }
    }
}
