import Foundation
import Testing

@testable import GlovCore
@testable import GlovDaemon
@testable import GlovDaemonAPI

/// The daemon mounting a VZ VM's shares through the Guest Service's exec (a fake agent, no VM).
@Suite struct ShareMountTests {
    private func config(_ dir: String) -> VMConfig {
        VMConfig(
            name: "lin", directory: dir, engine: .vz, firmwareCode: "", tpm: false,
            shares: [
                .init(name: "proj", hostPath: "/h/proj", guestPath: "/mnt/proj", mode: .rw),
                .init(name: "ref", hostPath: "/h/ref", guestPath: "/srv/ref", mode: .ro),
                .init(name: "done", hostPath: "/h/done", guestPath: "/mnt/done", mode: .rw),
            ])
    }

    /// Runs a VMGuestService over a fake Linux guest whose commands `answer` decides (exit code, output); returns the commands it saw.
    private func mountAll(_ answer: @escaping @Sendable ([String]) -> (Int, String)) async throws -> (argvs: [[String]], shares: [Results.ShareMount]) {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let c = config(dir)
        let fake = try FakeGuestService(socketPath: c.guestSocketPath)
        fake.os = "linux"
        fake.script = { argv in
            let (code, out) = answer(argv)
            return (out.isEmpty ? [] : [.out("stderr", out)]) + [.exit(code)]
        }
        let agent = VMGuestService(config: c)
        await agent.start()
        defer { fake.stop() }
        var shares: [Results.ShareMount] = []
        for _ in 0..<400 {
            shares = await agent.status().shares ?? []
            if shares.count == 3, !shares.contains(where: { $0.state == "mounting" }) { break }
            try await Task.sleep(for: .milliseconds(25))
        }
        await agent.stop()
        let argvs: [[String]] = fake.requests("exec.start").compactMap {
            if case .array(let a)? = $0["argv"] { return a.compactMap { if case .string(let s) = $0 { s } else { nil } } }
            return nil
        }
        return (argvs, shares)
    }

    /// A guest where "/", "/mnt" and "/srv" exist as safe directories and `unsafe` maps other paths to their stat output.
    private func guest(unsafe: [String: String] = [:], mountPoints: Set<String> = [], mountFails: String? = nil) -> @Sendable ([String]) -> (Int, String) {
        let made = Locked<Set<String>>([])
        return { argv in
            switch argv.first {
            case "stat":
                let path = argv.last!
                if let out = unsafe[path] { return (0, out) }
                if ["/", "/mnt", "/srv", "/mnt/done"].contains(path) || made.value.contains(path) { return (0, "0 755 directory\n") }
                return (1, "stat: cannot statx '\(path)': No such file or directory\n")
            case "mkdir": made.update { $0.insert(argv.last!) }; return (0, "")
            case "install": return (0, "")
            case "mountpoint": return (mountPoints.contains(argv.last!) ? 0 : 32, "")
            case "mount": return argv.contains(mountFails ?? "\u{0}") ? (32, "mount: wrong fs type\n") : (0, "")
            default: return (127, "")
            }
        }
    }

    /// A mount in progress is busy work: status names it, an update refuses naming it even with --force, and a reboot
    /// refuses naming it unless forced.
    @Test(.timeLimit(.minutes(1))) func aMountInProgressIsNamedByStatusUpdateAndReboot() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        let c = config(dir)
        let fake = try FakeGuestService(socketPath: c.guestSocketPath)
        defer { fake.stop() }
        fake.os = "linux"
        let answer = guest()
        fake.script = { argv in
            if argv.first == "mount" { return [.hold] }
            let (code, out) = answer(argv)
            return (out.isEmpty ? [] : [.out("stderr", out)]) + [.exit(code)]
        }
        let agent = VMGuestService(config: c)
        await agent.start()
        var busy: [String] = []
        for _ in 0..<400 {
            busy = await agent.status().busy ?? []
            if busy.contains("share mount proj") { break }
            try await Task.sleep(for: .milliseconds(25))
        }
        #expect(busy.contains("share mount proj"), "\(busy)")
        let home = GlovHome(path: (dir as NSString).deletingLastPathComponent)
        for force in [false, true] {
            let error = await #expect(throws: GuestServiceUpdateError.self) { try await agent.updateGuestService(.init(vm: "lin", force: force), home: home) }
            #expect("\(error!)".contains("share mount proj"), "\(error!)")
        }
        let refused = await #expect(throws: RPCError.self) { try await agent.prepareReboot(force: false) }
        #expect(refused?.message.contains("share mount proj") == true)
        try await agent.prepareReboot(force: true)
        await agent.stop()
    }

    @Test(.timeLimit(.minutes(1))) func eachComponentIsCheckedThenCreatedAndMountedAndAMountedShareIsSkipped() async throws {
        let (argvs, shares) = try await mountAll(guest(mountPoints: ["/mnt/done"]))
        let stat = { (p: String) in ["stat", "-c", "%u %a %F", "--", p] }
        #expect(argvs == [
            stat("/"), stat("/mnt"), ["mountpoint", "-q", "--", "/mnt/proj"], stat("/mnt/proj"), ["mkdir", "-m", "0755", "--", "/mnt/proj"], stat("/mnt/proj"),
            ["mount", "-t", "virtiofs", "--", "proj", "/mnt/proj"],
            stat("/"), stat("/srv"), ["mountpoint", "-q", "--", "/srv/ref"], stat("/srv/ref"), ["mkdir", "-m", "0755", "--", "/srv/ref"], stat("/srv/ref"),
            ["mount", "-t", "virtiofs", "-o", "ro", "--", "ref", "/srv/ref"],
            stat("/"), stat("/mnt"), ["mountpoint", "-q", "--", "/mnt/done"],
        ])
        #expect(shares.map(\.state) == ["mounted", "mounted", "mounted"])
    }

    @Test(.timeLimit(.minutes(1))) func aComponentThatIsALinkOrNotRootsIsRefusedAndNothingIsCreatedOrMounted() async throws {
        for (out, why) in [("0 777 symbolic link", "not a real directory"), ("1000 755 directory", "owned by uid 1000"), ("0 775 directory", "writable by group")] {
            let (argvs, shares) = try await mountAll(guest(unsafe: ["/srv": out]))
            #expect(shares.map(\.state) == ["mounted", "failed", "mounted"], "\(out)")
            #expect(shares[1].problem?.contains("/srv is") == true && shares[1].problem?.contains(why) == true, "\(String(describing: shares[1].problem))")
            #expect(!argvs.contains { $0.last == "/srv/ref" && $0.first != "stat" }, "no mkdir, mount or mountpoint under a bad component")
        }
    }

    @Test(.timeLimit(.minutes(1))) func aFailedMountIsReportedAndTheOthersStillMount() async throws {
        let (argvs, shares) = try await mountAll(guest(mountFails: "proj"))
        #expect(shares.map(\.state) == ["failed", "mounted", "mounted"])
        #expect(shares[0].problem == "mount exited with 32: mount: wrong fs type")
        #expect(argvs.contains(["mount", "-t", "virtiofs", "-o", "ro", "--", "ref", "/srv/ref"]))
    }
}

private final class Locked<T>: @unchecked Sendable {
    private let lock = NSLock()
    private var _value: T
    init(_ v: T) { _value = v }
    var value: T { lock.withLock { _value } }
    func update(_ f: (inout T) -> Void) { lock.withLock { f(&_value) } }
}
