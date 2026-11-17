import Foundation
import GlovCore
import Testing

@testable import GlovDaemonAPI

/// `glov share add|list|remove` and `vm create --share` as child processes, with no daemon running.
@Suite struct CLIShareTests {
    private func makeHome() throws -> (GlovHome, String) {
        let home = GlovHome(path: try shortTempDir())
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        let host = try shortTempDir()
        try FileManager.default.createDirectory(atPath: host + "/proj", withIntermediateDirectories: true)
        return (home, host)
    }

    @Test(.timeLimit(.minutes(1))) func sharesAreAddedListedAndRemovedInVMJson() async throws {
        let (home, host) = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path); try? FileManager.default.removeItem(atPath: host) }
        try home.create(VMConfig(name: "win11", directory: home.directory(of: "win11"), firmwareCode: home.path + "/code.fd", tpm: false))

        var r = try await runglov(["share", "list", "win11"], home: home)
        #expect(r.status == 0 && r.out == "no shares\n", "\(r.err)")
        r = try await runglov(["share", "add", "win", "proj", host + "/proj/", "c:/work/proj", "--ro", "--exclude", "bin/", "--exclude", "/root-only"], home: home)
        #expect(r.status == 0, "\(r.err)")
        let real = try #require(HostPath.realpath(host + "/proj"))
        #expect(try home.loadConfig("win11").shares == [
            .init(name: "proj", hostPath: real, guestPath: "C:\\work\\proj", mode: .ro, exclude: ["bin/", "/root-only"])
        ])
        r = try await runglov(["share", "list", "win11"], home: home)
        #expect(r.out == "proj (ro): \(real) -> C:\\work\\proj, exclude bin/ /root-only\n")

        let sessions = home.path + "/" + HarnessSessions.folder
        try FileManager.default.createDirectory(atPath: sessions + "/win11/sub", withIntermediateDirectories: true)
        for (args, message) in [
            (["share", "add", "win11", "proj", host + "/proj", "C:\\other"], "already has a share named proj"),
            (["share", "add", "win11", "p2", host + "/proj", "C:\\work\\proj\\sub"], "overlaps share proj"),
            (["share", "add", "win11", "p2", host + "/missing", "C:\\p2"], "not an existing directory"),
            (["share", "add", "win11", "p2", host + "/proj", "C:\\"], "drive root"),
            (["share", "add", "win11", "p2", home.path, "C:\\p2"], "glov home"),
            // Only glov's own conversation records share may be in the glov home, and only as a folder of `.sessions`.
            (["share", "add", "win11", "p2", sessions + "/win11", "C:\\p2"], "glov home"),
            (["share", "add", "win11", HarnessSessions.shareName, sessions, "C:\\p2"], "glov home"),
            (["share", "add", "win11", HarnessSessions.shareName, sessions + "/win11/sub", "C:\\p2"], "glov home"),
            (["share", "add", "win11", "bad/name", host + "/proj", "C:\\p2"], "invalid share name"),
            (["share", "remove", "win11", "nope"], "no share named \"nope\""),
        ] {
            let r = try await runglov(args, home: home)
            #expect(r.status != 0 && r.err.contains(message), "\(args): \(r.err)")
        }
        r = try await runglov(["share", "add", "win11", HarnessSessions.shareName, sessions + "/win11", "C:\\records"], home: home)
        #expect(r.status == 0, "\(r.err)")
        r = try await runglov(["share", "remove", "win11", HarnessSessions.shareName], home: home)
        #expect(r.status == 0, "\(r.err)")
        r = try await runglov(["share", "remove", "win11", "proj"], home: home)
        #expect(try r.status == 0 && home.loadConfig("win11").shares.isEmpty, "\(r.err)")
    }

    @Test(.timeLimit(.minutes(1))) func vmCreateTakesSharesAndAVZVMRefusesThem() async throws {
        let (home, host) = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path); try? FileManager.default.removeItem(atPath: host) }
        var r = try await runglov(
            ["vm", "create", "--no-start", "w", "--firmware-code", home.path + "/code.fd", "--disk", "/d/disk.qcow2", "--share", "proj,\(host)/proj,C:\\work\\proj,ro,exclude=obj/"],
            home: home)
        #expect(r.status == 0, "\(r.err)")
        let share = try #require(try home.loadConfig("w").shares.first)
        #expect(share.name == "proj" && share.mode == .ro && share.exclude == ["obj/"] && share.guestPath == "C:\\work\\proj")

        r = try await runglov(["vm", "create", "--no-start", "x", "--firmware-code", home.path + "/code.fd", "--disk", "/d.qcow2", "--share", "p"], home: home)
        #expect(r.status != 0 && r.err.contains("NAME,HOST_PATH[,GUEST_PATH]") && !home.vmNames().contains("x"), "\(r.err)")
        r = try await runglov(["vm", "create", "--no-start", "u", "--engine", "vz", "--share", "p,\(host)/proj,/etc"], home: home)
        #expect(r.status != 0 && r.err.contains("must be a folder below") && !home.vmNames().contains("u"), "\(r.err)")
        r = try await runglov(["vm", "create", "--no-start", "u", "--engine", "vz", "--share", "p,\(host)/proj,exclude=bin/"], home: home)
        #expect(r.status != 0 && r.err.contains("live virtiofs mount") && !home.vmNames().contains("u"), "\(r.err)")
        r = try await runglov(["vm", "create", "--no-start", "x", "--firmware-code", home.path + "/code.fd", "--disk", "/d.qcow2", "--share", "p,\(host)/proj,ro"], home: home)
        #expect(r.status != 0 && r.err.contains("needs a guest path"), "\(r.err)")

        var vz = VMConfig(name: "z", directory: home.directory(of: "z"), engine: .vz, firmwareCode: "")
        vz.firmwareVars = home.directory(of: "z") + "/vars"
        try home.create(vz)
        r = try await runglov(["share", "add", "z", "p", host + "/proj", "C:\\p"], home: home)
        #expect(r.status != 0 && r.err.contains("absolute Linux path"), "\(r.err)")
        r = try await runglov(["share", "add", "z", "p", host + "/proj", "--exclude", "bin/"], home: home)
        #expect(r.status != 0 && r.err.contains("live virtiofs mount"), "\(r.err)")
    }

    @Test(.timeLimit(.minutes(1))) func aVZShareDefaultsItsGuestPathAndKeepsOtherPathsAbsolute() async throws {
        let (home, host) = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path); try? FileManager.default.removeItem(atPath: host) }
        var vz = VMConfig(name: "z", directory: home.directory(of: "z"), engine: .vz, firmwareCode: "")
        vz.firmwareVars = home.directory(of: "z") + "/vars"
        try home.create(vz)
        try FileManager.default.createDirectory(atPath: host + "/other", withIntermediateDirectories: true)
        var r = try await runglov(["share", "add", "z", "proj", host + "/proj"], home: home)
        #expect(r.status == 0, "\(r.err)")
        r = try await runglov(["share", "add", "z", "other", host + "/other", "/srv//other/", "--ro"], home: home)
        #expect(r.status == 0, "\(r.err)")
        let real = try #require(HostPath.realpath(host))
        #expect(try home.loadConfig("z").shares == [
            .init(name: "proj", hostPath: real + "/proj", guestPath: "/mnt/proj", mode: .rw),
            .init(name: "other", hostPath: real + "/other", guestPath: "/srv/other", mode: .ro),
        ])
        r = try await runglov(["share", "sync", "z", "proj"], home: home)
        #expect(r.status != 0, "no daemon runs, so this fails either way: \(r.err)")
        r = try await runglov(["vm", "create", "--no-start", "w", "--engine", "vz", "--share", "s,\(host)/proj"], home: home)
        #expect(!r.err.contains("guest path"), "--share NAME,HOST is enough for a VZ VM: \(r.err)")
    }
}
