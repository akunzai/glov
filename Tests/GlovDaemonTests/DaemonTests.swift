import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// A daemon in this process over a real socket, with two stopped VMs.
private struct Fixture {
    let home: GlovHome
    let server: DaemonServer
    var client: DaemonClient { DaemonClient(socketPath: home.socketPath) }

    init() throws {
        home = GlovHome(path: try shortTempDir())
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        for name in ["win11", "win11-test", "ubuntu"] {
            try home.create(VMConfig(name: name, directory: home.directory(of: name), firmwareCode: home.path + "/code.fd", tpm: false))
        }
        server = try DaemonServer(home: home, dispatcher: Dispatcher(home: home))
        server.start()
    }

    func cleanup() {
        server.stop()
        try? FileManager.default.removeItem(atPath: home.path)
    }
}

@Suite struct DaemonTests {
    @Test func listsAndReportsStoppedVMs() async throws {
        let f = try Fixture()
        defer { f.cleanup() }
        let list = try await f.client.call(Method.vmList).decode(Results.List.self)
        #expect(list.vms.map { [$0.name, $0.state, $0.engine ?? ""] } == [
            ["ubuntu", "stopped", "qemu"], ["win11", "stopped", "qemu"], ["win11-test", "stopped", "qemu"],
        ])
        #expect(list.vms.allSatisfy { $0.agent?.update?.state == "unknown" })
        let status = try await f.client.call(Method.vmStatus, Params.VM(vm: "ub"), as: Results.VMInfo.self)
        #expect(status.name == "ubuntu" && status.state == "stopped" && status.engine == "qemu")
        #expect(status.agent?.update?.problem == "VM is not running")
    }

    @Test func reportsErrorsInWords() async throws {
        let f = try Fixture()
        defer { f.cleanup() }
        func message(_ method: String, _ params: JSONValue?) async -> RPCError? {
            do { _ = try await f.client.call(method, params); return nil } catch let e as RPCError { return e } catch { return RPCError(code: 0, message: "\(error)") }
        }
        #expect(await message("nope", nil)?.code == RPCError.methodNotFound)
        #expect(await message(Method.vmStatus, [:]) == RPCError(code: RPCError.invalidParams, message: "missing parameter 'vm'"))
        #expect(await message(Method.vmStatus, ["vm": "win"])?.message.contains("ambiguous: win11, win11-test") == true)
        #expect(await message(Method.vmStatus, ["vm": "zzz"])?.message.contains("known: ubuntu, win11, win11-test") == true)
        #expect(await message(Method.screenshot, ["vm": "ubuntu"]) == RPCError(code: RPCError.failed, message: "VM is not running"))
        #expect(await message(Method.click, ["vm": "ubuntu", "x": "left", "y": 1])?.code == RPCError.invalidParams)
        #expect(await message(Method.vmWait, ["vm": "ubuntu", "state": "stopped"])?.code == RPCError.invalidParams)
    }

    @Test func answersGarbageWithAParseErrorAndKeepsTheConnection() async throws {
        let f = try Fixture()
        defer { f.cleanup() }
        let fd = try await Blocking.run { [path = f.home.socketPath] in try UnixSocket.connect(path: path) }
        defer { close(fd) }
        var lines = UnixSocket.lines(fd: fd).makeAsyncIterator()
        _ = await Blocking.run { UnixSocket.write(fd: fd, Data("not json\n{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"vm.list\"}\n".utf8)) }
        let first = try JSONDecoder().decode(RPCResponse.self, from: try #require(await lines.next()))
        #expect(first.error?.code == RPCError.parseError && first.id == .null)
        let second = try JSONDecoder().decode(RPCResponse.self, from: try #require(await lines.next()))
        #expect(second.id == 7 && second.error == nil)
    }

    @Test func socketIsPrivateAndASecondDaemonIsRefused() throws {
        let f = try Fixture()
        defer { f.cleanup() }
        let mode = try FileManager.default.attributesOfItem(atPath: f.home.socketPath)[.posixPermissions] as? Int
        #expect(mode == 0o600)
        #expect(throws: DaemonError.self) { try DaemonServer(home: f.home, dispatcher: Dispatcher(home: f.home)) }
    }

    @Test func clientSaysHowToStartTheDaemon() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        do { _ = try await DaemonClient(socketPath: home.socketPath).call(Method.vmList); Issue.record("expected a failure") } catch {
            #expect("\(error)".contains("glovd"))
            #expect(error is DaemonClientError)
        }
    }

    @Test func aStaleSocketFileIsReplaced() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        FileManager.default.createFile(atPath: home.socketPath, contents: nil)
        let server = try DaemonServer(home: home, dispatcher: Dispatcher(home: home))
        server.stop()
    }
}
