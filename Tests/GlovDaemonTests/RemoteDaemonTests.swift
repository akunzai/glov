import Foundation
import GlovCore
import GlovDaemonAPI
import Testing

@Suite struct RemoteDaemonTests {
    @Test(.timeLimit(.minutes(1)))
    func sshConnectionSelectsTheRemoteDaemon() async throws {
        let f = try await GuestServiceFixture(agent: false)
        defer { f.server.stop(); try? FileManager.default.removeItem(atPath: f.home.path) }
        let ssh = try makeSSHFixture(home: f.home)
        let client = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", sshExecutable: ssh))
        let list = try await client.call(Method.vmList).decode(Results.List.self)
        #expect(list.vms.map(\.name) == ["win11"])
    }

    @Test(.timeLimit(.minutes(1)), arguments: [0, 1, 99])
    func incompatibleDaemonsNeverReceiveTheOperation(version: Int) async throws {
        let peer = try RemoteProtocolPeer(version: version)
        defer { peer.stop() }
        let client = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", home: peer.home.path, sshExecutable: try makeSSHFixture(home: peer.home)))
        do {
            _ = try await client.call("counter.increment")
            Issue.record("an incompatible daemon must be rejected")
        } catch RemoteDaemonError.incompatible {}
        let count = try await DaemonClient(socketPath: peer.home.socketPath).call("counter.read")
        #expect(count == 0)
    }

    @Test(.timeLimit(.minutes(1)))
    func aLostReplyIsNotRetriedAndTheNextCallCanReconnect() async throws {
        let peer = try RemoteProtocolPeer()
        defer { peer.stop() }
        let client = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", home: peer.home.path, sshExecutable: try makeSSHFixture(home: peer.home)))
        let first = try await client.callWithConnectionID("counter.read")
        do {
            _ = try await client.call("counter.increment")
            Issue.record("a lost reply must report an uncertain outcome")
        } catch RemoteDaemonError.outcomeUnknown(let method, _) { #expect(method == "counter.increment") }
        let resumed = try await client.callWithConnectionID("counter.read")
        #expect(resumed.result == 1 && resumed.connectionID != first.connectionID)
        do {
            _ = try await client.call("input.type", ["vm": "fixture", "text": "stale"], expectedConnectionID: first.connectionID)
            Issue.record("old-frame input must be discarded before the operation")
        } catch RemoteDaemonError.staleInput {}
        #expect(try await client.call("counter.read") == 1)
    }

    @Test(.timeLimit(.minutes(1)))
    func handshakeAndOperationStayOnTheSameDaemonConnection() async throws {
        let peer = try RemoteProtocolPeer(requireHandshake: true)
        defer { peer.stop() }
        let client = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", home: peer.home.path, sshExecutable: try makeSSHFixture(home: peer.home)))
        #expect(try await client.call("counter.read") == 0)
    }

    @Test(.timeLimit(.minutes(1)))
    func remoteFileCopiesUseDaemonHostPathsAndKeepTheAllowlist() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        let client = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", home: f.home.path, sshExecutable: try makeSSHFixture(home: f.home)))
        let source = f.shared + "/source.bin", destination = f.shared + "/result.bin"
        try FileManager.default.createDirectory(atPath: f.shared, withIntermediateDirectories: true)
        let payload = Data([0, 1, 34, 92, 255])
        try payload.write(to: URL(fileURLWithPath: source))
        let up = try await client.call(Method.copyToVM, Params.CopyToVM(vm: "win11", hostPath: source, guestPath: "C:\\fixture.bin"), as: Results.Copy.self)
        #expect(up.files == 1 && up.bytes == payload.count)
        let down = try await client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\fixture.bin", hostPath: destination), as: Results.Copy.self)
        let copied = try Data(contentsOf: URL(fileURLWithPath: destination))
        #expect(down.files == 1 && copied == payload)
        await #expect(throws: RPCError.self) {
            _ = try await client.call(Method.copyToVM, Params.CopyToVM(vm: "win11", hostPath: f.home.path + "/code.fd", guestPath: "C:\\denied"), as: Results.Copy.self)
        }
        await #expect(throws: RPCError.self) {
            _ = try await client.call(Method.copyFromVM, Params.CopyFromVM(vm: "win11", guestPath: "C:\\fixture.bin", hostPath: "relative"), as: Results.Copy.self)
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func submittedJobsSurviveTheSubmittingClient() async throws {
        let f = try await GuestServiceFixture()
        defer { f.cleanup() }
        f.fake.script = { _ in [.out("stdout", "still running\n"), .hold] }
        let ssh = try makeSSHFixture(home: f.home)
        func submit() async throws -> String {
            let client = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", home: f.home.path, sshExecutable: ssh))
            return try await client.call(Method.execStart, Params.Exec(vm: "win11", argv: ["fixture"]), as: Results.JobStarted.self).jobId
        }
        let job = try await submit()
        let reconnected = DaemonClient(remote: RemoteDaemon(host: "fixture.invalid", home: f.home.path, sshExecutable: ssh))
        let output = try await reconnected.call(Method.execRead, Params.ExecRead(vm: "win11", jobId: job), as: Results.ExecRead.self)
        #expect(!output.exited && !output.lost && output.data == "still running\n")
        _ = try await reconnected.call(Method.execKill, Params.ExecKill(vm: "win11", jobId: job), as: Results.OK.self)
    }
}

/// An independent protocol peer makes the effect of a lost mutation observable through a later RPC.
private final class RemoteProtocolPeer {
    let home: GlovHome
    private let listener: UnixSocket.Listener
    init(version: Int = DaemonProtocol.version, requireHandshake: Bool = false) throws {
        home = GlovHome(path: try shortTempDir())
        listener = UnixSocket.Listener(fd: try UnixSocket.listen(path: home.socketPath))
        let state = State(version: version)
        listener.start { fd in
            let handle = UnixSocket.Handle(fd: fd)
            Task {
                var handshaken = false
                for await line in UnixSocket.lines(fd: fd, onEnd: { handle.close() }) {
                    if let request = try? JSONDecoder().decode(RPCRequest.self, from: line) {
                        let response: RPCResponse?
                        if version == 0 && request.method == Method.daemonInfo {
                            response = RPCResponse(id: request.id ?? .null, error: RPCError(code: RPCError.methodNotFound, message: "unknown method"))
                        } else if requireHandshake && !handshaken && request.method != Method.daemonInfo {
                            response = RPCResponse(id: request.id ?? .null, error: RPCError(code: RPCError.failed, message: "handshake required on this connection"))
                        } else if let result = await state.reply(request.method) {
                            if request.method == Method.daemonInfo { handshaken = true }
                            response = RPCResponse(id: request.id ?? .null, result: result)
                        } else { response = nil }
                        if let response, let data = try? JSONEncoder().encode(response) { _ = await handle.send(data + Data("\n".utf8)) }
                        else { handle.shutdown(); break }
                    }
                }
            }
        }
    }
    func stop() { listener.stop(); try? FileManager.default.removeItem(atPath: home.path) }

    private actor State {
        let version: Int
        var count = 0
        init(version: Int) { self.version = version }
        func reply(_ method: String) -> JSONValue? {
            switch method {
            case Method.daemonInfo:
                return ["protocolVersion": .int(version), "instanceID": "fixture"]
            case "counter.increment": count += 1; return nil // apply, then lose the reply
            case "input.type": count += 1; return [:]
            default: return .int(count)
            }
        }
    }
}

/// A system-SSH boundary fixture. Forwards real Unix socket traffic; it never emulates daemon responses.
func makeSSHFixture(home: GlovHome) throws -> String {
    let script = home.path + "/fixture-ssh"
    let encoder = JSONEncoder()
    encoder.outputFormatting = .withoutEscapingSlashes
    let quotedHome = String(decoding: try encoder.encode(home.path), as: UTF8.self)
    let source = """
    #!/usr/bin/env python3
    import os, select, socket, sys, threading
    if '-L' not in sys.argv:
        print(\(quotedHome))
        sys.exit(0)
    local, remote = sys.argv[sys.argv.index('-L') + 1].split(':', 1)
    server = socket.socket(socket.AF_UNIX)
    server.bind(local)
    server.listen()
    def forward(client):
        peer = socket.socket(socket.AF_UNIX)
        try:
            peer.connect(remote)
            while True:
                ready, _, _ = select.select([client, peer], [], [])
                for src in ready:
                    data = src.recv(65536)
                    if not data: return
                    (peer if src is client else client).sendall(data)
        except (OSError, ConnectionError):
            pass
        finally:
            client.close()
            peer.close()
    while True:
        client, _ = server.accept()
        threading.Thread(target=forward, args=(client,), daemon=True).start()
    """
    try source.write(toFile: script, atomically: true, encoding: .utf8)
    try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script)
    return script
}
