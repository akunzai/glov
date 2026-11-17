import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

/// `share.sync` through the daemon against the fake Guest Service (Windows paths).
@Suite struct ShareSyncTests {
    private func put(_ path: String, _ text: String) throws {
        try FileManager.default.createDirectory(atPath: (path as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
        try Data(text.utf8).write(to: URL(fileURLWithPath: path))
    }

    /// The fixture's VM with a share of `<home>/proj` at C:\work\proj.
    private func fixture(exclude: [String] = [], mode: VMConfig.Share.Mode = .rw) async throws -> (GuestServiceFixture, String) {
        let f = try await GuestServiceFixture()
        let host = f.home.path + "/proj"
        try FileManager.default.createDirectory(atPath: host, withIntermediateDirectories: true)
        try f.home.update("win11") { config in config = try config.adding(.init(name: "proj", hostPath: host, guestPath: "C:\\work\\proj", mode: mode, exclude: exclude)) }
        return (f, host)
    }

    private func sync(_ f: GuestServiceFixture, _ share: String = "proj", verify: Bool? = nil) async throws -> Results.ShareSync {
        try await f.client.call(Method.shareSync, Params.ShareSync(vm: "win11", share: share, verify: verify), as: Results.ShareSync.self)
    }

    @Test func aSyncMirrorsTheHostFolderAndLeavesExcludedGuestPathsAlone() async throws {
        let (f, host) = try await fixture(exclude: ["bin/", "/root-only"])
        defer { f.cleanup() }
        try put(host + "/a.txt", "a")
        try put(host + "/src/b.cs", "bb")
        try put(host + "/bin/x.dll", "host build output")
        try put(host + "/root-only/r", "r")
        try put(host + "/sub/root-only/y", "y")
        f.fake.files = ["C:\\work\\proj\\bin\\z": Data("guest build output".utf8), "C:\\work\\proj\\stale.txt": Data("old".utf8)]
        f.fake.dirs = ["C:\\work\\proj\\bin"]

        let r = try await sync(f)
        #expect(r.writtenPaths == ["a.txt", "src/b.cs", "sub/root-only/y"] && r.written == 3)
        #expect(r.deletedPaths == ["stale.txt"] && r.unchanged == 0 && r.bytes == 4)
        #expect(r.guestPath == "C:\\work\\proj" && r.overwritten.isEmpty)
        #expect(f.fake.files == [
            "C:\\work\\proj\\a.txt": Data("a".utf8), "C:\\work\\proj\\src\\b.cs": Data("bb".utf8),
            "C:\\work\\proj\\sub\\root-only\\y": Data("y".utf8), "C:\\work\\proj\\bin\\z": Data("guest build output".utf8),
        ])

        // Nothing changed: nothing moves, and the host hashes nothing again.
        let again = try await sync(f)
        #expect(again.written == 0 && again.deleted == 0 && again.unchanged == 3 && again.hostHashed == 0)
        #expect(try await sync(f, verify: true).hostHashed == 3)
    }

    @Test func aRewriteKeepingSizeAndMtimeIsStillSent() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/t.txt", "1234")
        _ = try await sync(f)
        let mtime = try #require(try FileManager.default.attributesOfItem(atPath: host + "/t.txt")[.modificationDate] as? Date)
        try await Task.sleep(for: .milliseconds(20))
        try put(host + "/t.txt", "abcd")
        try FileManager.default.setAttributes([.modificationDate: mtime], ofItemAtPath: host + "/t.txt")
        let r = try await sync(f)
        #expect(r.writtenPaths == ["t.txt"] && r.hostHashed == 1)
        #expect(f.fake.files["C:\\work\\proj\\t.txt"] == Data("abcd".utf8))
    }

    @Test func aLargeFileIsStreamedOverSeveralCalls() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        let big = Data(repeating: 0xff, count: 1_300_000)  // Exercise the batch budget with slash-heavy base64.
        try FileManager.default.createDirectory(atPath: host, withIntermediateDirectories: true)
        try big.write(to: URL(fileURLWithPath: host + "/big.bin"))
        let r = try await sync(f)
        #expect(r.bytes == 1_300_000 && f.fake.files["C:\\work\\proj\\big.bin"] == big)
        #expect(f.fake.requests("sync.apply").count >= 3, "each call stays under the 1 MiB frame")
    }

    @Test func aSubdirectorySyncTouchesOnlyThatDirectory() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/a/x", "x")
        try FileManager.default.createDirectory(atPath: host + "/a/s", withIntermediateDirectories: true)
        try put(host + "/b/y", "y")
        f.fake.files = ["C:\\work\\proj\\b\\old": Data("o".utf8)]
        let r = try await sync(f, "proj/a")
        #expect(r.share == "proj/a" && r.writtenPaths == ["a/x"] && r.deleted == 0)
        #expect(r.guestPath == "C:\\work\\proj\\a")
        #expect(try await sync(f, "proj/a/s").guestPath == "C:\\work\\proj\\a\\s")
        #expect(f.fake.files["C:\\work\\proj\\b\\old"] != nil && f.fake.files["C:\\work\\proj\\b\\y"] == nil)
    }

    @Test func guestEditsAreOverwrittenAndListed() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/e.txt", "host")
        try put(host + "/s.txt", "same")
        _ = try await sync(f)
        f.fake.files["C:\\work\\proj\\e.txt"] = Data("guest edit".utf8)
        f.fake.files["C:\\work\\proj\\new-in-guest.txt"] = Data("n".utf8)
        let r = try await sync(f)
        #expect(r.overwritten == ["e.txt", "new-in-guest.txt"])
        #expect(f.fake.files["C:\\work\\proj\\e.txt"] == Data("host".utf8) && f.fake.files["C:\\work\\proj\\new-in-guest.txt"] == nil)
    }

    @Test func lockedGuestFilesFailTheSyncAndAreNamed() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/held.dll", "v1")
        try put(host + "/free.txt", "v1")
        _ = try await sync(f)
        try put(host + "/held.dll", "v2")
        try put(host + "/free.txt", "v2")
        f.fake.lockedPaths = ["C:\\work\\proj\\held.dll"]
        let e = try #require(await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"]))
        #expect(e.message.contains("in use in the guest") && e.message.contains("held.dll") && !e.message.contains("free.txt ("))
        #expect(f.fake.files["C:\\work\\proj\\free.txt"] == Data("v2".utf8), "the rest still lands")
    }

    @Test func accessDeniedIsNotReportedAsInUse() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        for name in ["held.dll", "ro.txt", "either.txt"] { try put(host + "/" + name, "v1") }
        _ = try await sync(f)
        for name in ["held.dll", "ro.txt", "either.txt"] { try put(host + "/" + name, "v2") }
        f.fake.lockedPaths = ["C:\\work\\proj\\held.dll"]
        f.fake.deniedPaths = ["C:\\work\\proj\\ro.txt": false, "C:\\work\\proj\\either.txt": true]
        let e = try #require(await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])).message
        #expect(e.contains("; in use in the guest (close the program holding them, then sync again): held.dll (in use)"), "\(e)")
        #expect(e.contains("; in use or access denied in the guest (close the program holding them, or clear their read-only attribute): either.txt (denied)"), "\(e)")
        #expect(e.contains("; access denied in the guest (read-only, or not writable by the Guest Service): ro.txt (denied)"), "\(e)")
    }

    @Test func aSyncFailsWithTheReasonWhenItCannotRun() async throws {
        let (f, _) = try await fixture()
        defer { f.cleanup() }
        #expect(await f.failure(Method.shareSync, ["vm": "win11", "share": "nope"])?.message.contains("no share named \"nope\"") == true)
        for bad in ["proj/../x", "proj//x", "proj/./x"] {
            #expect(await f.failure(Method.shareSync, ["vm": "win11", "share": .string(bad)])?.message.contains("'..'") == true, "\(bad)")
        }
        f.fake.supportsSync = false
        #expect(await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])?.message.contains("too old for share sync") == true)
        f.fake.answering = false
        try await Task.sleep(for: .seconds(14))  // two missed heartbeats
        let unresponsive = await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])
        #expect(unresponsive?.message.contains("not ready (unresponsive") == true, "\(String(describing: unresponsive))")
        await f.dispatcher.stopGuestServiceForTesting("win11")
        #expect(await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])?.message.contains("is not running") == true)
    }

    @Test func aSubdirectoryThroughASymlinkIsRefused() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        let outside = f.home.path + "/outside"
        try put(outside + "/id_ed25519", "secret")
        try FileManager.default.createSymbolicLink(atPath: host + "/link", withDestinationPath: outside)
        let e = await f.failure(Method.shareSync, ["vm": "win11", "share": "proj/link"])
        #expect(e?.message.contains("symbolic link") == true, "\(String(describing: e))")
        let whole = try await sync(f)
        #expect(whole.written == 0 && whole.skipped == ["link"])
        #expect(!f.fake.files.keys.contains { $0.contains("id_ed25519") }, "nothing outside the share reaches the guest")
    }

    @Test func aMissingHostFolderSyncsNothing() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        f.fake.files = ["C:\\work\\proj\\keep.txt": Data("k".utf8)]
        try FileManager.default.removeItem(atPath: host)
        #expect(await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])?.message.contains("host folder of share proj is missing") == true)
        #expect(f.fake.files["C:\\work\\proj\\keep.txt"] != nil)
    }

    @Test func sharesJoinTheCopyAllowlistByMode() async throws {
        let (f, host) = try await fixture(mode: .ro)
        defer { f.cleanup() }
        try put(host + "/in.txt", "i")
        f.fake.files["C:\\out.txt"] = Data("o".utf8)
        _ = try await f.client.call(Method.copyToVM, Params.CopyToVM(vm: "win11", hostPath: host + "/in.txt", guestPath: "C:\\in.txt"), as: Results.Copy.self)
        #expect(f.fake.files["C:\\in.txt"] == Data("i".utf8), "a ro share may be read")
        let refused = await f.failure(Method.copyFromVM, ["vm": "win11", "guestPath": "C:\\out.txt", "hostPath": .string(host + "/out.txt")])
        #expect(refused?.message.contains("allowlist") == true, "a ro share is never written")

        try f.home.update("win11") { config in config.shares[0].mode = .rw }  // while the VM runs: it applies at once
        _ = try await f.client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\out.txt", hostPath: host + "/out.txt"), as: Results.Copy.self)
        #expect(FileManager.default.fileExists(atPath: host + "/out.txt"))
    }

    @Test func aVZVMHasNoSyncedShares() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        try f.home.update("win11") { config in config.engine = .vz }
        #expect(await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])?.message.contains("live virtiofs mount") == true)
    }

    // MARK: a sync stuck in the guest

    /// Starts a sync whose listing the guest never answers, and waits until the listing was asked for.
    private func stuckSync(_ f: GuestServiceFixture) async throws -> Task<Results.ShareSync, any Error> {
        f.fake.holdManifest = true
        let sync = Task { try await self.sync(f) }
        await eventually { !f.fake.requests("sync.manifest").isEmpty }
        return sync
    }

    @Test(.timeLimit(.minutes(1)))
    func aClientThatLeavesCancelsItsSyncAndFreesTheShare() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/a.txt", "a")
        f.fake.holdManifest = true
        let socket = f.home.socketPath
        let fd = try await Blocking.run { try UnixSocket.connect(path: socket) }
        let request = try JSONEncoder().encode(RPCRequest(id: 1, method: Method.shareSync, params: ["vm": "win11", "share": "proj"]))
        #expect(await Blocking.run { UnixSocket.write(fd: fd, request + Data("\n".utf8)) })
        await eventually { !f.fake.requests("sync.manifest").isEmpty }
        close(fd)  // a killed `glov share sync`

        await eventually { f.fake.requests("sync.cancel").first?["id"] == "proj" }
        // The share is freed once the guest has answered the cancel.
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        await eventually { await a.syncing.isEmpty }
        f.fake.holdManifest = false
        let r = try await sync(f)
        #expect(r.writtenPaths == ["a.txt"])
    }

    @Test(.timeLimit(.minutes(1)))
    func aListingThatMakesNoProgressIsStoppedNamingItsFile() async throws {
        let (f, _) = try await fixture()
        defer { f.cleanup() }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        await a.setSyncWatchForTesting(interval: .milliseconds(20), stall: .milliseconds(200))
        f.fake.syncStatus = ["busy": true, "phase": "manifest", "path": "tools/logs/WebUI.log", "files": 3, "bytes": 100, "seconds": 1]
        f.fake.holdManifest = true
        let error = await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])
        #expect(error?.message.contains("no progress") == true && error?.message.contains("tools/logs/WebUI.log") == true, "\(String(describing: error))")
        #expect(f.fake.requests("sync.cancel").count == 1)
        #expect(await a.syncing.isEmpty)
    }

    /// The guest finished listing, but its answer never came: say so at once rather than after the stall limit.
    @Test(.timeLimit(.minutes(1)))
    func aListingWhoseAnswerIsLostFailsSayingSo() async throws {
        let (f, _) = try await fixture()
        defer { f.cleanup() }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        await a.setSyncWatchForTesting(interval: .milliseconds(20), stall: .seconds(30))
        f.fake.syncStatus = ["busy": false, "files": 0, "bytes": 0, "seconds": 0]
        f.fake.holdManifest = true
        let start = ContinuousClock.now
        let error = await f.failure(Method.shareSync, ["vm": "win11", "share": "proj"])
        #expect(error?.message.contains("the guest is not listing, yet its listing never reached the host") == true, "\(String(describing: error))")
        #expect(error?.message.contains("glov vm reboot win11") == true)
        #expect(ContinuousClock.now - start < .seconds(10))
        #expect(await a.syncing.isEmpty)
    }

    @Test(.timeLimit(.minutes(1)))
    func aListingThatKeepsHashingIsNotStopped() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/a.txt", "a")
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        await a.setSyncWatchForTesting(interval: .milliseconds(20), stall: .milliseconds(200))
        f.fake.syncStatus = ["busy": true, "phase": "manifest", "path": "big.bin", "files": 1, "bytes": 0, "seconds": 1]
        let sync = try await stuckSync(f)
        for bytes in 1...20 {  // 400 ms of steady progress, twice the stall limit
            f.fake.syncStatus = ["busy": true, "phase": "manifest", "path": "big.bin", "files": 1, "bytes": .int(bytes * 1000), "seconds": 1]
            try await Task.sleep(for: .milliseconds(20))
        }
        let running = await a.syncing["proj"] != nil
        #expect(running && f.fake.requests("sync.cancel").isEmpty)
        await a.syncing["proj"]?.cancel()
        await #expect(throws: RPCError.self) { _ = try await sync.value }
    }

    @Test(.timeLimit(.minutes(1)))
    func guestServiceUpdateWaitsForASyncButForceCancelsIt() async throws {
        let (f, _) = try await fixture()
        defer { f.cleanup() }
        let a = try #require(await f.dispatcher.guestServiceForTesting("win11"))
        let sync = try await stuckSync(f)

        let refused = await #expect(throws: GuestServiceUpdateError.self) { try await a.updateGuestService(.init(vm: "win11"), home: f.home) }
        #expect("\(refused!)".contains("share sync"))
        // Past the busy check, the update fails for want of a bundled Guest Service: that is not this test's concern.
        do { _ = try await a.updateGuestService(.init(vm: "win11", force: true), home: f.home) } catch {
            #expect(!"\(error)".contains("in progress"), "\(error)")
        }
        let cancelled = await #expect(throws: RPCError.self) { _ = try await sync.value }
        #expect(cancelled?.message.contains("cancelled") == true)
        let idle = await a.syncing.isEmpty
        #expect(f.fake.requests("sync.cancel").count == 1 && idle)
    }

    @Test(.timeLimit(.minutes(1)))
    func shareStatusShowsTheStageAndTheFileTheGuestIsOn() async throws {
        let (f, host) = try await fixture()
        defer { f.cleanup() }
        try put(host + "/a.txt", "a")
        var r = try await runglov(["share", "status", "win11"], home: f.home)
        #expect(r.status == 0 && r.out == "no share sync running\n", "\(r.err)")

        f.fake.syncStatus = ["busy": true, "phase": "manifest", "path": "tools/logs/WebUI.log", "files": 3, "bytes": 2_097_152, "seconds": .double(41.5)]
        let sync = try await stuckSync(f)
        let status = try await f.client.call(Method.shareStatus, Params.ShareStatus(vm: "win11"), as: Results.ShareStatus.self)
        #expect(status.syncs.map(\.share) == ["proj"] && status.syncs.first?.stage == "guest listing")
        #expect(status.syncs.first?.guest == .init(phase: "manifest", path: "tools/logs/WebUI.log", files: 3, bytes: 2_097_152, seconds: 41.5))
        r = try await runglov(["share", "status", "win11", "proj"], home: f.home)
        #expect(r.status == 0 && r.out.contains("proj: guest listing for ") && r.out.contains("guest hashing tools/logs/WebUI.log (3 files, 2.0 MiB in 41 s)"), "\(r.out)\(r.err)")
        #expect(await f.failure(Method.shareStatus, ["vm": "win11", "share": "nope"])?.message.contains("nope") == true)

        f.fake.syncStatus = nil  // an older agent: the stage alone
        r = try await runglov(["share", "status", "win11"], home: f.home)
        #expect(r.out.contains("guest progress unavailable"), "\(r.out)")
        await f.dispatcher.guestServiceForTesting("win11")?.syncing["proj"]?.cancel()
        _ = try? await sync.value
        #expect(try await f.client.call(Method.shareStatus, Params.ShareStatus(vm: "win11"), as: Results.ShareStatus.self).syncs.isEmpty)
    }
}
