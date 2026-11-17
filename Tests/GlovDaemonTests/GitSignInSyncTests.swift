import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// glovd setting git, gh and glab up in a running guest for its git secrets (a fake Guest Service, no VM).
@Suite struct GitSignInSyncTests {
    static let ghHosts = "/home/\(CloudInitSeed.user)/" + GitSignIn.ghHostsPath

    @Test(.timeLimit(.minutes(1))) func aGrantSetsTheGuestUpAndARevokeUndoesIt() async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
        try home.create(VMConfig(
            name: "box", directory: home.directory(of: "box"), firmwareCode: home.path + "/code.fd", tpm: false,
            isolated: true, egress: .init(allow: ["github.com"])))
        let config = try home.loadConfig("box")
        let fake = try FakeGuestService(socketPath: config.guestSocketPath)
        defer { fake.stop() }
        fake.os = "linux"
        fake.files = [GitSignIn.gitConfigPath: Data("[core]\n\teditor = vim\n".utf8)]
        fake.script = { argv in
            if argv.first == "rm", let path = argv.last { fake.files[path] = nil }
            return [.exit(0)]
        }
        let dispatcher = Dispatcher(home: home, secrets: SecretBroker(home: home, values: MemoryValues()))
        await dispatcher.startGuestService(config)
        defer { Task { await dispatcher.stopGuestServiceForTesting("box") } }
        for _ in 0..<400 where await dispatcher.guestServiceForTesting("box")?.isReady != true { try await Task.sleep(for: .milliseconds(25)) }
        func call(_ method: String, _ params: some Encodable) async throws {
            let r = await dispatcher.handle(.init(id: 1, method: method, params: try JSONValue(encoding: params)))
            #expect(r.error == nil, "\(method): \(String(describing: r.error))")
        }
        func file(_ path: String) -> String { fake.files[path].map { String(decoding: $0, as: UTF8.self) } ?? "" }

        try await call(Method.secretSource, Params.Secret(name: "gh", hosts: ["github.com", "api.github.com"], command: ["/bin/echo", "x"], forge: "github"))
        try await call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["gh"]))
        #expect(file(GitSignIn.signInsPath) == "github.com github gh\n")
        #expect(file(GitSignIn.helperPath) == GitSignIn.helper)
        #expect(file(GitSignIn.gitConfigPath).hasPrefix("[core]\n\teditor = vim\n") && file(GitSignIn.gitConfigPath).contains("insteadOf = git@github.com:"))
        #expect(file(Self.ghHosts).contains("oauth_token: glov-secret:gh") && !file(Self.ghHosts).contains("x\n"))

        try await call(Method.secretRevoke, Params.SecretGrant(vm: "box", names: ["gh"]))
        #expect(file(GitSignIn.signInsPath) == "")
        #expect(file(GitSignIn.gitConfigPath) == "[core]\n\teditor = vim\n")
        #expect(fake.files[Self.ghHosts] == nil)
    }
}
