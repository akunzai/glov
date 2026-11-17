import Foundation
import GlovCore
import NIOSSL
import Synchronization
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct TokenExchangeTests {
    @Test func theEndpointsOfSubscribedHarnessesKeepTheirTokensUnderVMNames() {
        let endpoints = TokenExchange.endpoints(vm: "box", subscriptions: ["codex"])
        let token = endpoints.first { $0.path == "/oauth/token" }
        #expect(token?.host == "auth.openai.com")
        #expect(token?.answer["access_token"] == .init(name: "box.codex.access", hosts: ["chatgpt.com"]))
        #expect(token?.answer["refresh_token"] == .init(name: "box.codex.refresh", hosts: ["auth.openai.com"]))
        #expect(TokenExchange.endpoints(vm: "box", subscriptions: []).isEmpty)
    }

    @Test func wholePlaceholderValuesInFormAndJSONBodiesGetTheirValues() throws {
        let form = Data("grant_type=refresh_token&refresh_token=glov-secret%3Abox.codex.refresh&note=glov-secret:x+y".utf8)
        #expect(TokenExchange.names(in: form, .form) == ["box.codex.refresh"])
        let swapped = try #require(TokenExchange.substitute(form, .form, values: ["box.codex.refresh": "r/1 +="]))
        #expect(String(decoding: swapped, as: UTF8.self) == "grant_type=refresh_token&refresh_token=r%2F1%20%2B%3D&note=glov-secret:x+y")

        let json = Data(#"{"grant_type":"refresh_token","refresh_token":"glov-secret:box.codex.refresh","n":1}"#.utf8)
        #expect(TokenExchange.names(in: json, .json) == ["box.codex.refresh"])
        let object = try JSONSerialization.jsonObject(with: try #require(TokenExchange.substitute(json, .json, values: ["box.codex.refresh": "r1"]))) as? [String: Any]
        #expect(object?["refresh_token"] as? String == "r1" && object?["n"] as? Int == 1)
        #expect(TokenExchange.substitute(Data("{".utf8), .json, values: [:]) == nil)
        #expect(TokenExchange.format("application/json; charset=utf-8") == .json)
        #expect(TokenExchange.format("application/x-www-form-urlencoded") == .form)
        #expect(TokenExchange.format("text/plain") == nil)
    }

    @Test func aChunkedAnswerIsReadWholeAndReframed() throws {
        let raw = Data("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n5\r\n{\"a\":\r\n4\r\n\"b\"}\r\n0\r\n\r\n".utf8)
        let response = try #require(TokenExchange.parse(raw))
        #expect(response.status == 200 && String(decoding: response.body, as: UTF8.self) == #"{"a":"b"}"#)
        #expect(String(decoding: response.framed, as: UTF8.self)
            == "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 9\r\nConnection: close\r\n\r\n{\"a\":\"b\"}")
        #expect(TokenExchange.parse(Data("HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort".utf8)) == nil)
        #expect(TokenExchange.parse(Data("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\nx".utf8)) == nil)
    }

    @Test func theAnswerKeepsDeclaredTokensAndGivesPlaceholders() throws {
        let kept = TokenExchange.keep(Data(#"{"access_token":"A","id_token":"I","expires_in":3600}"#.utf8), answer: [
            "access_token": .init(name: "box.codex.access", hosts: ["chatgpt.com"]),
            "refresh_token": .init(name: "box.codex.refresh", hosts: ["auth.openai.com"]),
        ])
        let (body, tokens) = try #require(kept)
        #expect(tokens.map(\.0.name) == ["box.codex.access"] && tokens.map(\.1) == ["A"])
        let object = try JSONSerialization.jsonObject(with: body) as? [String: Any]
        #expect(object?["access_token"] as? String == "glov-secret:box.codex.access" && object?["id_token"] as? String == "I")
        #expect(TokenExchange.keep(Data("[]".utf8), answer: [:]) == nil)
    }

    /// The secrets a fake broker holds.
    final class Store: Sendable {
        let values = Mutex<[String: (hosts: [String], value: String)]>([:])
        var definitions: [SecretDefinition] {
            values.withLock { $0.map { try! SecretDefinition(name: $0.key, source: .keychain, hosts: $0.value.hosts) } }
        }
    }

    @Test(.timeLimit(.minutes(1)))
    func signInAndRefreshKeepTheTokensOnTheMacAndTheGuestSeesPlaceholders() async throws {
        let home = GlovHome(path: try shortTempDir()), upstreamHome = GlovHome(path: try shortTempDir())
        defer { for h in [home, upstreamHome] { try? FileManager.default.removeItem(atPath: h.path) } }
        let round = Mutex(0)
        let upstream = try SecretSubstitutionTests.Upstream(home: upstreamHome, host: "auth.openai.com") { request in
            let n = round.withLock { $0 += 1; return $0 }
            if request.contains("bad") { return "HTTP/1.1 401 Unauthorized\r\nContent-Length: 4\r\nConnection: close\r\n\r\nnope" }
            if request.contains("echo") {
                let body = "invalid: " + (request.components(separatedBy: "\r\n\r\n").last ?? "")
                return "HTTP/1.1 400 Bad Request\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)"
            }
            let body = #"{"access_token":"A\#(n)REAL","refresh_token":"R\#(n)REAL","id_token":"I"}"#
            return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n\(String(body.utf8.count, radix: 16))\r\n\(body)\r\n0\r\n\r\n"
        }
        defer { upstream.channel.close(promise: nil) }
        let ca = try EgressCA.loadOrCreate(home: home)
        let tls = try EgressTLS(ca: ca, upstreamTrust: .certificates([try NIOSSLCertificate(bytes: Array(upstream.ca.certificatePEM.utf8), format: .pem)]))
        let store = Store(), lines = Mutex<[String]>([]), failing = Mutex(false)
        let port = upstream.port
        let proxy = EgressProxy(
            vm: "box",
            egress: { .init(allow: ["auth.openai.com", "chatgpt.com"], secrets: ["box.codex.access", "box.codex.refresh"], subscriptions: ["codex"]) },
            resolve: { _, _ in ["93.184.215.14"] }, localAddresses: { [] }, connect: { _, _ in EgressNet.connect("127.0.0.1", port) },
            log: { r in lines.withLock { $0.append(r.line) } }, tls: tls, secretDefinitions: { store.definitions },
            secretValue: { name in try store.values.withLock { try #require($0[name]).value } },
            storeSecret: { name, hosts, value in
                if failing.withLock({ $0 }) { throw CancellationError() }
                store.values.withLock { $0[name] = (hosts, value) }
            })
        func post(_ body: String, type: String) -> String {
            "POST /oauth/token HTTP/1.1\r\nHost: auth.openai.com\r\nContent-Type: \(type)\r\nAccept-Encoding: gzip\r\nContent-Length: \(body.utf8.count)\r\n\r\n\(body)"
        }

        // Sign-in: no secret exists yet, the code goes up as it is, and the tokens of the answer stay here.
        let signIn = try await SecretSubstitutionTests().exchange(
            proxy, host: "auth.openai.com", post("grant_type=authorization_code&code=c1", type: "application/x-www-form-urlencoded"), trust: ca)
        #expect(signIn.hasPrefix("HTTP/1.1 200 OK") && !signIn.contains("REAL"), "\(signIn)")
        #expect(signIn.contains(#""access_token":"glov-secret:box.codex.access""#) && signIn.contains(#""id_token":"I""#))
        #expect(store.values.withLock { $0["box.codex.access"]?.value } == "A1REAL")
        #expect(store.values.withLock { $0["box.codex.refresh"]?.hosts } == ["auth.openai.com"])
        let first = try #require(upstream.heads.list.withLock { $0.first })
        #expect(first.hasSuffix("Content-Length: 37\r\nConnection: close\r\n\r\ngrant_type=authorization_code&code=c1") && !first.contains("gzip"), "\(first)")

        // Refresh: the real refresh token goes up in the body, and the rotated one replaces it here.
        let refresh = try await SecretSubstitutionTests().exchange(
            proxy, host: "auth.openai.com", post(#"{"grant_type":"refresh_token","refresh_token":"glov-secret:box.codex.refresh"}"#, type: "application/json"),
            trust: ca)
        #expect(refresh.contains(#""refresh_token":"glov-secret:box.codex.refresh""#) && !refresh.contains("REAL"), "\(refresh)")
        #expect(upstream.heads.list.withLock { $0.last }?.contains(#""refresh_token":"R1REAL""#) == true)
        #expect(store.values.withLock { $0["box.codex.refresh"]?.value } == "R2REAL")

        // A refusal passes through untouched; a token that cannot be kept never reaches the guest.
        let refused = try await SecretSubstitutionTests().exchange(proxy, host: "auth.openai.com", post("code=bad", type: "application/x-www-form-urlencoded"), trust: ca)
        #expect(refused.hasPrefix("HTTP/1.1 401") && refused.hasSuffix("nope"))
        let echoed = try await SecretSubstitutionTests().exchange(
            proxy, host: "auth.openai.com", post("echo=1&refresh_token=glov-secret%3Abox.codex.refresh", type: "application/x-www-form-urlencoded"), trust: ca)
        #expect(echoed.hasPrefix("HTTP/1.1 400") && echoed.hasSuffix("refresh_token=glov-secret:box.codex.refresh") && !echoed.contains("REAL"), "\(echoed)")
        failing.withLock { $0 = true }
        let unkept = try await SecretSubstitutionTests().exchange(proxy, host: "auth.openai.com", post("code=c4", type: "application/x-www-form-urlencoded"), trust: ca)
        #expect(unkept.hasPrefix("HTTP/1.1 502") && !unkept.contains("REAL"), "\(unkept)")

        for _ in 0..<100 where lines.withLock({ $0.count }) < 5 { try await Task.sleep(for: .milliseconds(20)) }
        let log = lines.withLock { $0 }
        #expect(log.first?.contains("POST /oauth/token 200") == true && log.first?.hasSuffix("secrets=box.codex.access,box.codex.refresh") == true, "\(log)")
        #expect(log.last?.hasSuffix("(token not kept)") == true, "\(log)")
        #expect(!log.joined().contains("REAL"))
    }
}
