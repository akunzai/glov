import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct SSHRelayTests {
    /// A running VZ VM whose vsock port is one end of a socket pair; the other end is handed to the test as "sshd".
    actor VsockFixture: VsockMachine {
        nonisolated let config: VMConfig
        var isRunning = true
        let processID: Int32? = nil
        var ports: [UInt32] = []
        let sshd: (@Sendable (Int32) -> Void)?
        /// Nil `sshd`: nothing listens on the port, as with a Guest Service from before the relay.
        init(config: VMConfig, sshd: (@Sendable (Int32) -> Void)?) { self.config = config; self.sshd = sshd }
        func guestIsRunning() async -> Bool { isRunning }
        func stop(timeout: Duration) async { isRunning = false }
        func connect(port: UInt32) async throws -> Int32 {
            ports.append(port)
            guard let sshd else { throw GuestServiceError.notConnected }
            var pair: [Int32] = [0, 0]
            precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
            sshd(pair[1])
            return pair[0]
        }
    }

    static func echo(_ fd: Int32) {
        Thread.detachNewThread {
            defer { close(fd) }
            var buffer = [UInt8](repeating: 0, count: 4096)
            while case let n = buffer.withUnsafeMutableBytes({ UnixSocket.readWhenReadable(fd, $0) }), n > 0 { _ = write(fd, buffer, n) }
        }
    }

    /// Runs blocking socket I/O on a thread of its own: on the cooperative pool it could take the last thread the
    /// daemon's `openSSH` task needs while the daemon's relay thread waits for that task.
    static func onThread<T: Sendable>(_ body: @escaping @Sendable () throws -> T) async throws -> T {
        try await withCheckedThrowingContinuation { c in Thread.detachNewThread { c.resume(with: Result { try body() }) } }
    }

    /// Opens a relay stream on the daemon socket; returns the fd and the daemon's first line.
    static func open(_ home: GlovHome, _ vm: String) throws -> (Int32, String) {
        let fd = try UnixSocket.connect(path: home.socketPath)
        EgressProxyTests.send(fd, "\(SSHRelay.header) \(vm)\n")
        var line: [UInt8] = []
        var byte: UInt8 = 0
        while read(fd, &byte, 1) == 1, byte != 10 { line.append(byte) }
        return (fd, String(decoding: line, as: UTF8.self))
    }

    @Test(.timeLimit(.minutes(1)))
    func aRelayStreamReachesTheGuestsSSHPortAndJSONRPCStillWorks() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let firmware = home.path + "/code.fd"
        FileManager.default.createFile(atPath: firmware, contents: Data())
        for (name, engine) in [("box", VMConfig.Engine.vz), ("win", .qemu), ("off", .vz), ("old", .vz)] {
            try home.create(VMConfig(name: name, directory: home.directory(of: name), engine: engine, firmwareCode: engine == .qemu ? firmware : "", tpm: false))
        }
        let dispatcher = Dispatcher(home: home)
        let box = VsockFixture(config: try home.loadConfig("box"), sshd: Self.echo)
        await dispatcher.attachMachineForTesting(box)
        await dispatcher.attachMachineForTesting(FixtureMachine(config: try home.loadConfig("win")))
        await dispatcher.attachMachineForTesting(VsockFixture(config: try home.loadConfig("old"), sshd: nil))
        let server = try DaemonServer(home: home, dispatcher: dispatcher)
        server.start()
        defer { server.stop() }

        let (fd, first, echoed) = try await Self.onThread {
            let (fd, first) = try Self.open(home, "bo")
            EgressProxyTests.send(fd, "SSH-2.0-OpenSSH_10.5\r\n")
            return (fd, first, EgressProxyTests.receive(fd, atLeast: 22))
        }
        defer { close(fd) }
        #expect(first == "ok")
        #expect(echoed == "SSH-2.0-OpenSSH_10.5\r\n")
        #expect(await box.ports == [VZConfiguration.sshPort])

        try await Self.onThread {
            for (vm, message) in [
                ("win", "VM win is a QEMU VM: glov ssh reaches it through its forwarded sshPort"),
                ("off", "VM off is not running; start it with glov vm start off"),
                ("old", "VM old's Guest Service does not relay SSH yet: wait for glov vm wait old guest-ready, or update it with glov vm guest update old"),
                ("nope", "no VM named \"nope\" (known: box, off, old, win)"),
            ] {
                let (refused, line) = try Self.open(home, vm)
                #expect(line == "error: " + message, "\(vm)")
                #expect(EgressProxyTests.receive(refused) == "")
                close(refused)
            }
        }
        let list = try await DaemonClient(socketPath: home.socketPath).call(Method.vmList)
        #expect("\(list)".contains("box"))
    }

    @Test(.timeLimit(.minutes(1)))
    func glovSSHRelayPipesItsStdioToTheGuestAndReportsRefusals() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        for name in ["box", "off"] {
            try home.create(VMConfig(name: name, directory: home.directory(of: name), engine: .vz, firmwareCode: "", tpm: false))
        }
        let dispatcher = Dispatcher(home: home)
        await dispatcher.attachMachineForTesting(VsockFixture(config: try home.loadConfig("box"), sshd: Self.echo))
        let server = try DaemonServer(home: home, dispatcher: dispatcher)
        server.start()
        defer { server.stop() }

        // ssh's own bytes go through untouched, both ways, and the end of stdin ends the session.
        let banner = "SSH-2.0-OpenSSH_10.5\r\n" + String(repeating: "x", count: 100_000)
        let relayed = try await runglov(["ssh-relay", "box"], home: home, stdin: banner)
        #expect(relayed.status == 0 && relayed.out == banner && relayed.err == "")

        let refused = try await runglov(["ssh-relay", "off"], home: home)
        #expect(refused.status != 0 && refused.out == "")
        #expect(refused.err == "glov: VM off is not running; start it with glov vm start off\n")
    }
}
