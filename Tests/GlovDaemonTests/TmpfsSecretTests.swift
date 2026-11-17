import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

/// `tmpfs` secrets reaching a Linux guest through `secrets.sync` (a fake Guest Service, no VM).
@Suite struct TmpfsSecretTests {
    static let profile = try! JSONDecoder().decode(HarnessProfile.self, from: Data("""
        {"name": "t", "title": "T", "command": "t", "verified": {"version": "1", "date": "2026-11-03"}, "docs": [],
         "secrets": [{"name": "cred", "delivery": "tmpfs", "file": ".t/cred.json", "hosts": ["t.example.com"]},
                     {"name": "key", "delivery": "placeholder", "env": "T_KEY", "hosts": ["t.example.com"]}],
         "environment": {}, "files": [], "runtime": [], "install": [], "installScript": "", "userNamespaces": false, "notes": []}
        """.utf8))

    struct Fixture {
        let home: GlovHome
        let dispatcher: Dispatcher
        let fake: FakeGuestService
        let values = MemoryValues()

        init(harnesses: [String] = ["t"]) async throws {
            home = GlovHome(path: try shortTempDir())
            try Data(count: 4096).write(to: URL(fileURLWithPath: home.path + "/code.fd"))
            // QEMU's socket channel stands in for vsock; the delivery does not depend on the engine.
            try home.create(VMConfig(
                name: "box", directory: home.directory(of: "box"), firmwareCode: home.path + "/code.fd", tpm: false,
                isolated: true, egress: .init(), harnesses: harnesses))
            let config = try home.loadConfig("box")
            fake = try FakeGuestService(socketPath: config.guestSocketPath)
            fake.os = "linux"
            dispatcher = Dispatcher(home: home, secrets: SecretBroker(home: home, values: values))
            await dispatcher.setHarnessProfilesForTesting([TmpfsSecretTests.profile])
            await dispatcher.startGuestService(config)
            for _ in 0..<400 where await dispatcher.guestServiceForTesting("box")?.isReady != true { try await Task.sleep(for: .milliseconds(25)) }
        }

        func call(_ method: String, _ params: some Encodable) async throws -> RPCResponse {
            await dispatcher.handle(.init(id: 1, method: method, params: try JSONValue(encoding: params)))
        }

        /// The secrets of each `secrets.sync` so far, as name: value.
        var syncs: [[String: String]] {
            fake.requests("secrets.sync").map { p in
                guard case .array(let list)? = p["secrets"] else { return [:] }
                var out: [String: String] = [:]
                for s in list {
                    if case .string(let n)? = s["name"], case .string(let d)? = s["data"], let data = Data(base64Encoded: d) { out[n] = String(decoding: data, as: UTF8.self) }
                }
                return out
            }
        }

        func waitForSyncs(_ n: Int) async throws {
            for _ in 0..<400 where fake.requests("secrets.sync").count < n { try await Task.sleep(for: .milliseconds(25)) }
        }

        func cleanup() async {
            await dispatcher.stopGuestServiceForTesting("box")
            fake.stop()
            try? FileManager.default.removeItem(atPath: home.path)
        }
    }

    @Test(.timeLimit(.minutes(1))) func grantingDeliversOnlyTmpfsSecretsAndRevokingRemovesThem() async throws {
        let f = try await Fixture()
        _ = try await f.call(Method.secretSet, Params.Secret(name: "cred", hosts: ["t.example.com"], value: "{\"token\":\"v1\"}"))
        _ = try await f.call(Method.secretSet, Params.Secret(name: "key", hosts: ["t.example.com"], value: "k"))
        // The session's own sync: nothing granted yet, so the guest is told to hold nothing.
        try await f.waitForSyncs(1)
        #expect(f.syncs.first == [:])
        #expect(f.fake.requests("secrets.sync").first?["owner"] == .string(CloudInitSeed.user))
        #expect(await f.dispatcher.guestServiceForTesting("box")?.holdsTmpfsSecrets == false)

        #expect(try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred", "key"])).error == nil)
        // The placeholder secret stays on the host.
        #expect(f.syncs.last == ["cred": "{\"token\":\"v1\"}"])
        #expect(await f.dispatcher.guestServiceForTesting("box")?.holdsTmpfsSecrets == true)

        // A new value reaches the guest at once.
        _ = try await f.call(Method.secretSet, Params.Secret(name: "cred", hosts: ["t.example.com"], value: "{\"token\":\"v2\"}"))
        #expect(f.syncs.last == ["cred": "{\"token\":\"v2\"}"])

        #expect(try await f.call(Method.secretRevoke, Params.SecretGrant(vm: "box", names: ["cred"])).error == nil)
        #expect(f.syncs.last == [:])
        // Its memory may still hold the value until it reboots.
        #expect(await f.dispatcher.guestServiceForTesting("box")?.holdsTmpfsSecrets == true)
        f.fake.bootId = "boot-2"
        f.fake.dropConnection()
        for _ in 0..<400 where await f.dispatcher.guestServiceForTesting("box")?.holdsTmpfsSecrets != false { try await Task.sleep(for: .milliseconds(25)) }
        #expect(await f.dispatcher.guestServiceForTesting("box")?.holdsTmpfsSecrets == false)
        await f.cleanup()
    }

    @Test(.timeLimit(.minutes(1))) func aNewSessionDeliversWhatIsGrantedAndRemovingTheSecretTakesItAway() async throws {
        let f = try await Fixture()
        _ = try await f.call(Method.secretSet, Params.Secret(name: "cred", hosts: ["t.example.com"], value: "c"))
        _ = try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred"]))
        let before = f.fake.requests("secrets.sync").count
        f.fake.dropConnection()
        try await f.waitForSyncs(before + 1)
        #expect(f.syncs.last == ["cred": "c"])
        #expect(try await f.call(Method.secretRemove, Params.Secret(name: "cred")).error == nil)
        #expect(f.syncs.last == [:])
        await f.cleanup()
    }

    @Test(.timeLimit(.minutes(1))) func aFailedDeliveryIsReportedByTheGrant() async throws {
        let f = try await Fixture()
        try await f.waitForSyncs(1)
        f.fake.secretsSyncFails = true
        _ = try await f.call(Method.secretSet, Params.Secret(name: "cred", hosts: ["t.example.com"], value: "VALUE-xyz"))
        let message = try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred"])).error?.message
        #expect(message?.contains("saved, but box: cannot deliver tmpfs secrets (cred)") == true)
        #expect(message?.contains("VALUE") == false)
        #expect(try f.home.loadConfig("box").egress?.secrets == ["cred"])
        await f.cleanup()
    }

    @Test(.timeLimit(.minutes(1))) func aVMWithoutTmpfsSecretsIsNeverSynced() async throws {
        let f = try await Fixture(harnesses: [])
        _ = try await f.call(Method.secretSet, Params.Secret(name: "cred", hosts: ["t.example.com"], value: "c"))
        _ = try await f.call(Method.secretGrant, Params.SecretGrant(vm: "box", names: ["cred"]))
        try await Task.sleep(for: .milliseconds(200))
        #expect(f.fake.requests("secrets.sync").isEmpty)
        await f.cleanup()
    }
}
