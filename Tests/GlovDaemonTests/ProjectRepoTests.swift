import Foundation
import GlovCore
@testable import GlovDaemonAPI
import Synchronization
import Testing

/// `ProjectRepo` against a guest repository in a local folder, as the remote `glov-<vm>` (a path instead of the SSH
/// host): what the Mac sends, what comes back and what glov says.
@Suite struct ProjectRepoTests {
    final class Lines: Sendable {
        let lines = Mutex<[String]>([])
        func add(_ line: String) { lines.withLock { $0.append(line) } }
        var text: String { lines.withLock { $0.joined(separator: "\n") } }
    }

    struct Fixture {
        let root: String, mac: String, guest: String
        let log = Lines()
        var repo: ProjectRepo { ProjectRepo(dir: mac, vm: "t", home: GlovHome(path: root), log: { [log] in log.add($0) }) }

        /// Runs the guest's scripts here, as the guest would.
        var run: ProjectRepo.GuestRun {
            { argv, _ in
                let p = Process()
                p.executableURL = URL(fileURLWithPath: argv[0])
                p.arguments = Array(argv.dropFirst())
                p.standardOutput = FileHandle.nullDevice
                p.standardError = FileHandle.nullDevice
                try await runToExit(p)
                return (Int(p.terminationStatus), "")
            }
        }

        init() async throws {
            root = try shortTempDir()
            mac = root + "/mac"
            guest = root + "/guest"
            try FileManager.default.createDirectory(atPath: guest, withIntermediateDirectories: true)
            try await Self.git(["init", "-q", "-b", "main", mac])
            try await commit(mac, "a.txt", "one\n")
            try await Self.git(["-C", mac, "remote", "add", "glov-t", guest])
            // The owner, as land makes them the committer; signing only where a test turns it on.
            for (key, value) in [("user.name", "Owner"), ("user.email", "owner@example.com"), ("commit.gpgsign", "false")] {
                try await Self.git(["-C", mac, "config", key, value])
            }
            let set = try await run(["/bin/sh", "-c", ProjectGit.guestRepoScript, "sh", guest, "main", "Owner", "owner@example.com", ""], "user")
            #expect(set.code == 0)
        }

        func commit(_ dir: String, _ file: String, _ text: String) async throws {
            try Data(text.utf8).write(to: URL(fileURLWithPath: dir + "/" + file))
            try await Self.git(["-C", dir, "add", file])
            try await Self.git(["-C", dir, "-c", "user.name=t", "-c", "user.email=t@t.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "-m", file])
        }

        @discardableResult
        static func git(_ arguments: [String]) async throws -> String {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/usr/bin/env")
            p.arguments = ["git"] + arguments
            let out = Pipe()
            p.standardOutput = out
            p.standardError = FileHandle.nullDevice
            try await runToExit(p)
            let data = await Blocking.run { out.fileHandleForReading.readDataToEndOfFile() }
            guard p.terminationStatus == 0 else { throw CocoaError(.executableLoad) }
            return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        }
    }

    @Test func theBranchGoesToTheGuestAndItsCommitsComeBack() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        #expect(await f.repo.push(guest: f.guest, run: f.run) == "main")
        #expect(f.log.text.contains("pushed main to the guest"))
        #expect(try String(contentsOfFile: f.guest + "/a.txt", encoding: .utf8) == "one\n")

        try await f.commit(f.guest, "b.txt", "two\n")
        #expect(await f.repo.fetch(branch: "main"))
        #expect(f.log.text.contains("glov-t/main is 1 commit ahead of main; review with git log main..glov-t/main"))
        #expect(try await Fixture.git(["-C", f.mac, "log", "-1", "--format=%s", "glov-t/main"]) == "b.txt")
        // Nothing of the guest's lands on the Mac's own branch.
        #expect(!FileManager.default.fileExists(atPath: f.mac + "/b.txt"))
    }

    /// A pre-push hook of the Mac's is for its forges: git-lfs's would try to upload to the guest and fail the push.
    @Test func thePushToTheGuestRunsNoPrePushHook() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        let hook = f.mac + "/.git/hooks/pre-push"
        try Data("#!/bin/sh\nexit 1\n".utf8).write(to: URL(fileURLWithPath: hook))
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: hook)
        #expect(await f.repo.push(guest: f.guest, run: f.run) == "main")
        #expect(f.log.text.contains("pushed main to the guest"))
    }

    @Test func aGuestAheadOrDivergedKeepsItsOwnAndNothingIsForced() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        _ = await f.repo.push(guest: f.guest, run: f.run)
        try await f.commit(f.guest, "b.txt", "two\n")
        let ahead = try await Fixture.git(["-C", f.guest, "rev-parse", "HEAD"])
        f.log.lines.withLock { $0 = [] }
        #expect(await f.repo.push(guest: f.guest, run: f.run) == "main")
        #expect(!f.log.text.contains("pushed"))

        try await f.commit(f.mac, "c.txt", "three\n")
        f.log.lines.withLock { $0 = [] }
        #expect(await f.repo.push(guest: f.guest, run: f.run) == "main")
        #expect(f.log.text.contains("main and glov-t/main have diverged; the guest keeps its own"))
        #expect(try await Fixture.git(["-C", f.guest, "rev-parse", "HEAD"]) == ahead)
    }

    @Test func uncommittedChangesStayAndADetachedHeadSendsNothing() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        try Data("dirty\n".utf8).write(to: URL(fileURLWithPath: f.mac + "/a.txt"))
        _ = await f.repo.push(guest: f.guest, run: f.run)
        #expect(f.log.text.contains("warning: 1 change not committed stays on this Mac; the guest gets only commits"))
        #expect(try String(contentsOfFile: f.guest + "/a.txt", encoding: .utf8) == "one\n")

        try await Fixture.git(["-C", f.mac, "checkout", "-q", "--detach"])
        f.log.lines.withLock { $0 = [] }
        #expect(await f.repo.push(guest: f.guest, run: f.run) == nil)
        #expect(f.log.text.contains("HEAD is detached on this Mac; nothing pushed to the guest"))
    }

    // MARK: glov vm land

    /// Pushes the Mac's branch to the guest and makes `files` commits there, by the agent.
    func guestCommits(_ f: Fixture, _ files: [String]) async throws {
        _ = await f.repo.push(guest: f.guest, run: f.run)
        for file in files { try await f.commit(f.guest, file, file + "\n") }
    }

    func landRefs(_ f: Fixture) async throws -> String {
        try await Fixture.git(["-C", f.mac, "for-each-ref", "--format=%(refname)", "refs/glov-land/"])
    }

    @Test func landMakesTheGuestsCommitsTheOwnersSignedAndGivesTheBranchBack() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        try await guestCommits(f, ["b.txt", "c.txt"])
        // An SSH signing key of the owner's, which the guest never sees.
        let key = f.root + "/signing"
        let keygen = Process()
        keygen.executableURL = URL(fileURLWithPath: "/usr/bin/ssh-keygen")
        keygen.arguments = ["-q", "-t", "ed25519", "-N", "", "-f", key]
        try await runToExit(keygen)
        for (k, v) in [("gpg.format", "ssh"), ("user.signingkey", key), ("commit.gpgsign", "true")] {
            try await Fixture.git(["-C", f.mac, "config", k, v])
        }
        var asked: String?
        try await f.repo.land(branch: nil) { asked = $0; return true }
        #expect(asked == "make them again on main as your commits? [y/N] ")
        #expect(f.log.text.contains("glov-t/main has 2 commits for main:"))
        #expect(f.log.text.contains("review them first: git log -p main..refs/remotes/glov-t/main"))
        #expect(f.log.text.contains("main is yours now, here and in t"))
        let landed = try await Fixture.git(["-C", f.mac, "log", "-2", "--format=%s|%an|%cn", "main"])
        #expect(landed == "c.txt|t|Owner\nb.txt|t|Owner")
        let raw = try await Fixture.git(["-C", f.mac, "cat-file", "commit", "main"])
        #expect(raw.contains("gpgsig -----BEGIN SSH SIGNATURE-----"))
        // The guest has the signed commits in its branch and working tree.
        #expect(try await Fixture.git(["-C", f.guest, "rev-parse", "HEAD"]) == (try await Fixture.git(["-C", f.mac, "rev-parse", "main"])))
        #expect(try String(contentsOfFile: f.guest + "/c.txt", encoding: .utf8) == "c.txt\n")
        #expect(try await landRefs(f).isEmpty)
        // Again: nothing left.
        f.log.lines.withLock { $0 = [] }
        try await f.repo.land(branch: nil) { _ in Issue.record("asked again"); return false }
        #expect(f.log.text.contains("glov-t/main has nothing main lacks"))
    }

    @Test func aDeclinedLandChangesNothingAndAnotherBranchMustBeCheckedOut() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        try await guestCommits(f, ["b.txt"])
        let before = try await Fixture.git(["-C", f.mac, "rev-parse", "main"])
        await #expect(throws: ProjectRepo.Failure.self) { try await f.repo.land(branch: nil) { _ in false } }
        #expect(try await Fixture.git(["-C", f.mac, "rev-parse", "main"]) == before)
        #expect(try await landRefs(f).isEmpty)
        do { try await f.repo.land(branch: "other") { _ in true } } catch {
            #expect("\(error)".hasPrefix("check other out in \(f.mac) first"))
        }
        // Changes not committed on the Mac stop it before anything moves.
        try Data("dirty\n".utf8).write(to: URL(fileURLWithPath: f.mac + "/a.txt"))
        do { try await f.repo.land(branch: nil) { _ in true }; Issue.record("landed") } catch {
            #expect("\(error)".contains("has changes not committed"))
        }
    }

    @Test func aConflictStopsForTheOwnerAndTheNextLandOnlyGivesTheBranchBack() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        _ = await f.repo.push(guest: f.guest, run: f.run)
        try await f.commit(f.guest, "a.txt", "guest\n")
        try await f.commit(f.mac, "a.txt", "mac\n")
        do { try await f.repo.land(branch: nil) { _ in true }; Issue.record("landed") } catch {
            #expect("\(error)".contains("the cherry-pick stopped: fix that in \(f.mac)"))
        }
        do { try await f.repo.land(branch: nil) { _ in true }; Issue.record("landed") } catch {
            #expect("\(error)".hasPrefix("a cherry-pick is under way"))
        }
        try Data("both\n".utf8).write(to: URL(fileURLWithPath: f.mac + "/a.txt"))
        try await Fixture.git(["-C", f.mac, "add", "a.txt"])
        try await Fixture.git(["-C", f.mac, "-c", "core.editor=true", "cherry-pick", "--continue"])
        f.log.lines.withLock { $0 = [] }
        try await f.repo.land(branch: nil) { _ in Issue.record("asked again"); return false }
        #expect(f.log.text.contains("main is yours now"))
        #expect(try String(contentsOfFile: f.guest + "/a.txt", encoding: .utf8) == "both\n")
        #expect(try await landRefs(f).isEmpty)
    }

    @Test func aGuestThatCommittedMeanwhileKeepsItsBranchAndTheNextLandTakesOnlyTheNewCommit() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        try await guestCommits(f, ["b.txt"])
        // The guest commits while the owner reviews.
        do {
            try await f.repo.land(branch: nil) { _ in
                let p = Process()
                p.executableURL = URL(fileURLWithPath: "/bin/sh")
                p.arguments = ["-c", "cd \"$1\" && echo c > c.txt && git add c.txt && git -c user.name=t -c user.email=t@t.invalid -c commit.gpgsign=false commit -q -m c.txt", "sh", f.guest]
                try await runToExit(p)
                return true
            }
            Issue.record("the guest took the branch back")
        } catch {
            #expect("\(error)".hasPrefix("main is landed here, but the guest did not take it back"))
        }
        #expect(try await landRefs(f) == "refs/glov-land/t/main.landed")
        f.log.lines.withLock { $0 = [] }
        try await f.repo.land(branch: nil) { _ in true }
        #expect(f.log.text.contains("glov-t/main has 1 commit for main:"))
        #expect(try await Fixture.git(["-C", f.mac, "log", "--format=%s", "main"]) == "c.txt\nb.txt\na.txt")
        #expect(try await landRefs(f).isEmpty)
    }

    @Test func mergeCommitsFromTheGuestAreLandedByHand() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        _ = await f.repo.push(guest: f.guest, run: f.run)
        try await Fixture.git(["-C", f.guest, "checkout", "-q", "-b", "side"])
        try await f.commit(f.guest, "b.txt", "b\n")
        try await Fixture.git(["-C", f.guest, "checkout", "-q", "main"])
        try await f.commit(f.guest, "c.txt", "c\n")
        try await Fixture.git(["-C", f.guest, "-c", "user.name=t", "-c", "user.email=t@t.invalid", "merge", "-q", "--no-edit", "side"])
        do { try await f.repo.land(branch: nil) { _ in true }; Issue.record("landed") } catch {
            #expect("\(error)".hasPrefix("glov-t/main has merge commits that main lacks: land them by hand"))
        }
    }

    // MARK: glov vm delete

    /// What delete would lose: per fetched guest branch, its commits no local branch has by content, so a landed commit
    /// (another hash, same change) does not count.
    @Test func deleteListsTheGuestsCommitsNoLocalBranchHas() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        #expect(await f.repo.unlanded().isEmpty)  // nothing fetched yet
        try await guestCommits(f, ["b.txt", "c.txt"])
        try await Fixture.git(["-C", f.guest, "checkout", "-q", "-b", "side"])
        try await f.commit(f.guest, "d.txt", "d\n")
        _ = await f.repo.fetch(branch: nil)
        #expect(await f.repo.unlanded() == ["glov-t/main: 2 commits main lacks", "glov-t/side: 3 commits HEAD lacks"])
        try await Fixture.git(["-C", f.guest, "checkout", "-q", "main"])
        try await f.repo.land(branch: nil) { _ in true }
        #expect(await f.repo.unlanded() == ["glov-t/side: 1 commit HEAD lacks"])
    }

    @Test func forgettingTheVMRemovesItsRemoteLandStateAndSSHHost() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        try await guestCommits(f, ["b.txt"])
        _ = await f.repo.fetch(branch: nil)
        try await Fixture.git(["-C", f.mac, "update-ref", "refs/glov-land/t/main.landed", "HEAD"])
        let host = { (vm: String) in ProjectGit.SSHHost(vm: vm, key: "/k", knownHosts: "/h", relay: "relay \(vm)") }
        try Data(ProjectGit.withSSHHost(ProjectGit.withSSHHost("", host("t")), host("u")).utf8)
            .write(to: URL(fileURLWithPath: f.repo.sshConfigPath))
        await f.repo.forget()
        #expect(try await Fixture.git(["-C", f.mac, "remote"]).isEmpty)
        #expect(try await Fixture.git(["-C", f.mac, "for-each-ref", "refs/remotes/", "refs/glov-land/"]).isEmpty)
        let left = try String(contentsOfFile: f.repo.sshConfigPath, encoding: .utf8)
        #expect(!ProjectGit.hasSSHHost(left, vm: "t") && ProjectGit.hasSSHHost(left, vm: "u"))
    }

    // MARK: a guest tree in use, and runs at once

    /// A push would move the tree under a harness at work (updateInstead): the guest keeps it, and glov says why.
    @Test func noPushWhileAProgramWorksInTheGuestsTree() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        _ = await f.repo.push(guest: f.guest, run: f.run)
        try await f.commit(f.mac, "b.txt", "b\n")
        let before = try await Fixture.git(["-C", f.guest, "rev-parse", "HEAD"])
        let base = f.run
        let busy: ProjectRepo.GuestRun = { argv, user in
            argv.contains(ProjectGit.guestBusyScript) ? (0, "claude\n") : try await base(argv, user)
        }
        #expect(await f.repo.push(guest: f.guest, run: busy) == "main")
        #expect(try await Fixture.git(["-C", f.guest, "rev-parse", "HEAD"]) == before)
        #expect(f.log.text.contains("not pushed: claude runs in \(f.guest) in the guest, whose tree stays as it is; end it"))
        // Once it is done, the next run pushes.
        #expect(await f.repo.push(guest: f.guest, run: f.run) == "main")
        #expect(try await Fixture.git(["-C", f.guest, "rev-parse", "HEAD"]) == (try await Fixture.git(["-C", f.mac, "rev-parse", "main"])))
    }

    /// Runs for many VMs at once each keep their block: the edits take turns under ssh_config.lock.
    @Test func concurrentSSHConfigEditsKeepEveryVMsBlock() async throws {
        let f = try await Fixture()
        defer { try? FileManager.default.removeItem(atPath: f.root) }
        let names = (0..<24).map { "vm\($0)" }
        let home = f.repo.home, mac = f.mac
        await withTaskGroup(of: Void.self) { group in
            for name in names {
                group.addTask {
                    await Blocking.run {
                        let repo = ProjectRepo(dir: mac, vm: name, home: home, log: { _ in })
                        try? repo.editSSHConfig {
                            Thread.sleep(forTimeInterval: 0.002)  // widen the window a lost update needs
                            return ProjectGit.withSSHHost($0, .init(vm: name, key: "/k", knownHosts: "/h", relay: "relay"))
                        }
                    }
                }
            }
        }
        let text = try String(contentsOfFile: f.repo.sshConfigPath, encoding: .utf8)
        #expect(names.filter { !ProjectGit.hasSSHHost(text, vm: $0) }.isEmpty)
    }
}

