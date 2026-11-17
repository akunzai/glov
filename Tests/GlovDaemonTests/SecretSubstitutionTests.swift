import Foundation
import GlovCore
import NIOCore
import NIOPosix
import NIOSSL
import Synchronization
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct SecretSubstitutionTests {
    static let definitions = [
        try! SecretDefinition(name: "gh", source: .keychain, hosts: ["api.example.com"]),
        try! SecretDefinition(name: "gh.user", source: .keychain, hosts: ["api.example.com"]),
        try! SecretDefinition(name: "npm", source: .keychain, hosts: ["registry.example.org"]),
    ]

    func plan(_ head: String, granted: [String] = ["gh", "gh.user", "npm"]) -> Result<SecretSubstitution.Plan, SecretSubstitution.Refusal> {
        SecretSubstitution.plan(Data(head.utf8), host: "api.example.com", port: 443, vm: "box", granted: granted, definitions: Self.definitions)
    }

    @Test func headersGetValuesBasicCredentialsIncludedAndTheConnectionCloses() throws {
        let basic = Data("x-access-token:glov-secret:gh".utf8).base64EncodedString()
        let head = "POST /repo.git/git-upload-pack?x=glov-secret:gh HTTP/1.1\r\nHost: API.example.com\r\nAuthorization: Basic \(basic)\r\n"
            + "X-Token: glov-secret:gh.user glov-secret:gh\r\nConnection: keep-alive\r\nProxy-Authorization: x\r\n\r\n"
        let p = try plan(head).get()
        #expect(p.secrets == ["gh", "gh.user"])
        #expect(p.path == "/repo.git/git-upload-pack")
        let out = String(decoding: try SecretSubstitution.apply(p, values: ["gh": "T1", "gh.user": "U2"]).get(), as: UTF8.self)
        #expect(out == """
            POST /repo.git/git-upload-pack?x=glov-secret:gh HTTP/1.1\r
            Host: API.example.com\r
            Authorization: Basic \(Data("x-access-token:T1".utf8).base64EncodedString())\r
            X-Token: U2 T1\r
            Connection: close\r
            \r

            """)
    }

    @Test func refusalsNameTheFixAndNeverAValue() {
        func refusal(_ head: String, granted: [String] = ["gh", "npm"]) -> SecretSubstitution.Refusal? {
            if case .failure(let r) = plan(head, granted: granted) { return r }
            return nil
        }
        let host = "Host: api.example.com\r\n"
        #expect(refusal("GET / HTTP/1.1\r\n\(host)A: glov-secret:gh\r\n\r\n", granted: []) == .init(
            status: 403, message: "VM box was not granted secret gh: run glov secret grant box gh on the Mac", reason: "secret gh not granted"))
        #expect(refusal("GET / HTTP/1.1\r\n\(host)A: glov-secret:nope\r\n\r\n")?.reason == "secret nope undefined")
        #expect(refusal("GET / HTTP/1.1\r\n\(host)A: glov-secret:npm\r\n\r\n") == .init(
            status: 403, message: "secret npm is not bound to api.example.com: it may go to registry.example.org only", reason: "secret npm not bound to host"))
        #expect(refusal("GET / HTTP/1.1\r\nHost: evil.example.net\r\nA: glov-secret:gh\r\n\r\n")?.reason == "Host header mismatch")
        #expect(refusal("GET / HTTP/1.1\r\nA: glov-secret:gh\r\n\r\n")?.status == 400)
        #expect(refusal("GET http://api.example.com/ HTTP/1.1\r\n\(host)\r\n")?.status == 400)

        let p = try! plan("GET / HTTP/1.1\r\n\(host)A: glov-secret:gh\r\n\r\n").get()
        #expect(SecretSubstitution.apply(p, values: ["gh": "a\r\nInjected: 1"]).failure?.reason == "secret gh unusable")
    }

    @Test func plainHTTPCarryingAPlaceholderIsRefusedBeforeConnecting() {
        let h = EgressProxyTests.Harness()
        let (guest, done) = h.open()
        let basic = Data("u:glov-secret:gh".utf8).base64EncodedString()
        EgressProxyTests.send(guest, "GET http://api.example.com/ HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Basic \(basic)\r\n\r\n")
        #expect(EgressProxyTests.receive(guest).hasPrefix("HTTP/1.1 403 Forbidden"))
        done.wait()
        close(guest)
        #expect(h.connected.withLock { $0 }.isEmpty)
        #expect(h.lines.withLock { $0 } == ["egress deny api.example.com:80 in=0 out=0 (a secret placeholder goes only over HTTPS to a host the secret is bound to)"])
    }

    // MARK: Over TLS

    /// An HTTPS server for `api.example.com` with a CA of its own; it answers each request with `ok` and records the head.
    final class Upstream: Sendable {
        final class Heads: Sendable { let list = Mutex<[String]>([]) }
        let heads = Heads()
        let ca: EgressCA
        let channel: any Channel
        var port: Int { channel.localAddress!.port! }

        /// Serves `host`, answering each request, once its body arrived, with what `response` gives for it.
        init(
            home: GlovHome, host: String = "api.example.com",
            response: @escaping @Sendable (String) -> String = { _ in "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok" }
        ) throws {
            ca = try EgressCA.loadOrCreate(home: home)
            let leaf = try ca.leaf(for: host)
            let context = try NIOSSLContext(configuration: .makeServerConfiguration(
                certificateChain: [.certificate(try NIOSSLCertificate(bytes: Array(leaf.certificatePEM.utf8), format: .pem))],
                privateKey: .privateKey(try NIOSSLPrivateKey(bytes: Array(leaf.keyPEM.utf8), format: .pem))))
            let heads = heads
            channel = try ServerBootstrap(group: MultiThreadedEventLoopGroup.singleton)
                .childChannelInitializer { child in
                    child.eventLoop.makeCompletedFuture {
                        try child.pipeline.syncOperations.addHandler(NIOSSLServerHandler(context: context))
                        try child.pipeline.syncOperations.addHandler(Answer(heads, response))
                    }
                }
                .bind(host: "127.0.0.1", port: 0).wait()
        }

        final class Answer: ChannelInboundHandler {
            typealias InboundIn = ByteBuffer
            typealias OutboundOut = ByteBuffer
            let heads: Heads
            let response: @Sendable (String) -> String
            var text = ""
            var answered = false
            init(_ heads: Heads, _ response: @escaping @Sendable (String) -> String) {
                self.heads = heads
                self.response = response
            }
            func channelRead(context: ChannelHandlerContext, data: NIOAny) {
                text += String(buffer: unwrapInboundIn(data))
                guard !answered, let end = text.range(of: "\r\n\r\n") else { return }
                let length = text[..<end.lowerBound].components(separatedBy: "\r\n")
                    .first { $0.lowercased().hasPrefix("content-length:") }.flatMap { Int($0.dropFirst(15).trimmingCharacters(in: .whitespaces)) } ?? 0
                guard text[end.upperBound...].utf8.count >= length else { return }
                answered = true
                heads.list.withLock { $0.append(text) }
                context.writeAndFlush(wrapOutboundOut(ByteBuffer(string: response(text))))
                    .assumeIsolated().whenComplete { _ in context.close(promise: nil) }
            }
        }
    }

    /// Reads a whole TLS exchange's plaintext from the guest's side.
    final class Collect: ChannelInboundHandler {
        typealias InboundIn = ByteBuffer
        var data = ""
        let done: EventLoopPromise<String>
        init(_ done: EventLoopPromise<String>) { self.done = done }
        func channelRead(context: ChannelHandlerContext, data: NIOAny) { self.data += String(buffer: unwrapInboundIn(data)) }
        func channelInactive(context: ChannelHandlerContext) { done.succeed(data) }
        func errorCaught(context: ChannelHandlerContext, error: any Error) { done.fail(error) }
    }

    /// Sends CONNECT through `proxy`, then `request` over TLS trusting `trust`; returns the plaintext response.
    func exchange(_ proxy: EgressProxy, host: String = "api.example.com", _ request: String, trust: EgressCA) async throws -> String {
        var pair: [Int32] = [0, 0]
        #expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
        let served = pair[1]
        Thread.detachNewThread { proxy.serve(served) }
        let guest = pair[0]
        // Blocking socket calls stay off the cooperative pool, which other tests may also be blocking.
        let answer = await Blocking.run {
            EgressProxyTests.send(guest, "CONNECT \(host):443 HTTP/1.1\r\nHost: \(host):443\r\n\r\n")
            return EgressProxyTests.receive(guest, atLeast: 39)
        }
        #expect(answer == "HTTP/1.1 200 Connection Established\r\n\r\n")
        var config = TLSConfiguration.makeClientConfiguration()
        config.trustRoots = .certificates([try NIOSSLCertificate(bytes: Array(trust.certificatePEM.utf8), format: .pem)])
        let context = try NIOSSLContext(configuration: config)
        let loop = MultiThreadedEventLoopGroup.singleton.next()
        let done = loop.makePromise(of: String.self)
        let channel = try await ClientBootstrap(group: loop)
            .channelInitializer { channel in
                channel.eventLoop.makeCompletedFuture {
                    try channel.pipeline.syncOperations.addHandler(NIOSSLClientHandler(context: context, serverHostname: host))
                    try channel.pipeline.syncOperations.addHandler(Collect(done))
                }
            }
            .withConnectedSocket(pair[0]).get()
        try await channel.writeAndFlush(ByteBuffer(string: request))
        return try await done.futureResult.get()
    }

    @Test(.timeLimit(.minutes(1)))
    func aBoundHostGetsTheValueOverTLSAndTheLogNamesOnlyTheSecret() async throws {
        let home = GlovHome(path: try shortTempDir()), upstreamHome = GlovHome(path: try shortTempDir())
        defer { for h in [home, upstreamHome] { try? FileManager.default.removeItem(atPath: h.path) } }
        let upstream = try Upstream(home: upstreamHome)
        defer { upstream.channel.close(promise: nil) }
        let ca = try EgressCA.loadOrCreate(home: home)
        let tls = try EgressTLS(ca: ca, upstreamTrust: .certificates([try NIOSSLCertificate(bytes: Array(upstream.ca.certificatePEM.utf8), format: .pem)]))
        let lines = Mutex<[String]>([])
        let port = upstream.port
        let proxy = EgressProxy(
            vm: "box", egress: { .init(allow: ["api.example.com", "other.example.com"], secrets: ["gh"]) }, resolve: { _, _ in ["93.184.215.14"] },
            localAddresses: { [] }, connect: { _, _ in EgressNet.connect("127.0.0.1", port) }, log: { r in lines.withLock { $0.append(r.line) } },
            tls: tls, secretDefinitions: { Self.definitions }, secretValue: { _ in "tok-REAL" })

        let ok = try await exchange(proxy, "GET /user HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", trust: ca)
        #expect(ok.hasPrefix("HTTP/1.1 200 OK") && ok.hasSuffix("ok"))
        let seen = upstream.heads.list.withLock { $0 }
        #expect(seen == ["GET /user HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer tok-REAL\r\nConnection: close\r\n\r\n"])

        let denied = try await exchange(proxy, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:npm\r\n\r\n", trust: ca)
        #expect(denied.hasPrefix("HTTP/1.1 403 Forbidden") && denied.contains("glov secret grant box npm"))
        #expect(upstream.heads.list.withLock { $0.count } == 1)

        // A guest that does not trust the egress CA gets no request through.
        await #expect(throws: (any Error).self) {
            _ = try await exchange(proxy, "GET / HTTP/1.1\r\nHost: api.example.com\r\n\r\n", trust: upstream.ca)
        }

        for _ in 0..<100 where lines.withLock({ $0.count }) < 3 { try await Task.sleep(for: .milliseconds(20)) }
        let log = lines.withLock { $0 }
        #expect(log.count == 3)
        #expect(log.first?.hasPrefix("egress allow api.example.com:443 GET /user 200 in=") == true && log.first?.hasSuffix(" secrets=gh") == true, "\(log)")
        #expect(log.dropFirst().first?.contains("deny api.example.com:443 403") == true && log.dropFirst().first?.hasSuffix("(secret npm not granted)") == true, "\(log)")
        #expect(log.last?.contains("fail api.example.com:443") == true && log.last?.hasSuffix("(guest TLS: the guest does not trust the egress CA; recreate a VM made before the CA)") == true, "\(log)")
        #expect(!log.joined().contains("REAL"))
    }

    @Test(.timeLimit(.minutes(1)))
    func aVMsTrustedCertificateVerifiesAnUpstreamTheMacDoesNotTrust() async throws {
        let home = GlovHome(path: try shortTempDir()), upstreamHome = GlovHome(path: try shortTempDir())
        defer { for h in [home, upstreamHome] { try? FileManager.default.removeItem(atPath: h.path) } }
        let upstream = try Upstream(home: upstreamHome)
        defer { upstream.channel.close(promise: nil) }
        let ca = try EgressCA.loadOrCreate(home: home)
        // The "Mac's roots" here trust only the egress CA, not the upstream's internal CA.
        let tls = try EgressTLS(ca: ca, upstreamTrust: .certificates([try NIOSSLCertificate(bytes: Array(ca.certificatePEM.utf8), format: .pem)]))
        let lines = Mutex<[String]>([])
        let port = upstream.port
        func proxy(_ trust: [VMConfig.Egress.Trust]?) -> EgressProxy {
            EgressProxy(
                vm: "box", egress: { .init(allow: ["api.example.com"], secrets: ["gh"], trust: trust) }, resolve: { _, _ in ["93.184.215.14"] },
                localAddresses: { [] }, connect: { _, _ in EgressNet.connect("127.0.0.1", port) }, log: { r in lines.withLock { $0.append(r.line) } },
                tls: tls, secretDefinitions: { Self.definitions }, secretValue: { _ in "tok-REAL" })
        }
        let request = "GET /user HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n"

        let refused = try? await exchange(proxy(nil), request, trust: ca)
        #expect(refused?.hasPrefix("HTTP/1.1 200 OK") != true)
        #expect(upstream.heads.list.withLock { $0 }.isEmpty)

        let trusted = try await exchange(proxy([.init(name: "corp", certificate: upstream.ca.certificatePEM)]), request, trust: ca)
        #expect(trusted.hasPrefix("HTTP/1.1 200 OK") && trusted.hasSuffix("ok"), "\(trusted)")
        #expect(upstream.heads.list.withLock { $0.count } == 1)
    }

    @Test(.timeLimit(.minutes(1)))
    func theUpstreamConnectsOnlyOnceTheSecretValuesAreRead() async throws {
        // A value can take seconds (a Keychain prompt, a command source); a provider closes a connection that sends
        // nothing that long, and the request then failed with "EOF during handshake".
        let home = GlovHome(path: try shortTempDir()), upstreamHome = GlovHome(path: try shortTempDir())
        defer { for h in [home, upstreamHome] { try? FileManager.default.removeItem(atPath: h.path) } }
        let upstream = try Upstream(home: upstreamHome)
        defer { upstream.channel.close(promise: nil) }
        let ca = try EgressCA.loadOrCreate(home: home)
        let tls = try EgressTLS(ca: ca, upstreamTrust: .certificates([try NIOSSLCertificate(bytes: Array(upstream.ca.certificatePEM.utf8), format: .pem)]))
        let events = Mutex<[String]>([])
        let lines = Mutex<[String]>([])
        let port = upstream.port
        let proxy = EgressProxy(
            vm: "box", egress: { .init(allow: ["api.example.com"], secrets: ["gh"]) }, resolve: { _, _ in ["93.184.215.14"] }, localAddresses: { [] },
            connect: { _, _ in events.withLock { $0.append("connect") }; return EgressNet.connect("127.0.0.1", port) },
            log: { r in lines.withLock { $0.append(r.line) } }, tls: tls, secretDefinitions: { Self.definitions },
            secretValue: { _ in
                try await Task.sleep(for: .milliseconds(300))
                events.withLock { $0.append("value") }
                return "tok-REAL"
            })

        let ok = try await exchange(proxy, "GET /user HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", trust: ca)
        #expect(ok.hasPrefix("HTTP/1.1 200 OK") && ok.hasSuffix("ok"))
        #expect(events.withLock { $0 } == ["value", "connect"])

        // A refused request never connects.
        let denied = try await exchange(proxy, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:npm\r\n\r\n", trust: ca)
        #expect(denied.hasPrefix("HTTP/1.1 403 Forbidden"))
        #expect(events.withLock { $0 } == ["value", "connect"])

        // An upstream out of reach once the values are in: 502, named in the log.
        let unreachable = EgressProxy(
            vm: "box", egress: { .init(allow: ["api.example.com"], secrets: ["gh"]) }, resolve: { _, _ in ["93.184.215.14"] }, localAddresses: { [] },
            connect: { _, _ in nil }, log: { r in lines.withLock { $0.append(r.line) } }, tls: tls, secretDefinitions: { Self.definitions },
            secretValue: { _ in "tok-REAL" })
        let failed = try await exchange(unreachable, "GET / HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer glov-secret:gh\r\n\r\n", trust: ca)
        #expect(failed.hasPrefix("HTTP/1.1 502 Bad Gateway"), "\(failed)")
        for _ in 0..<100 where lines.withLock({ $0.count }) < 3 { try await Task.sleep(for: .milliseconds(20)) }
        #expect(lines.withLock { $0 }.contains { $0.contains("fail api.example.com:443") && $0.hasSuffix("(unreachable)") }, "\(lines.withLock { $0 })")
        #expect(!lines.withLock { $0 }.joined().contains("REAL"))
    }

    @Test func aCloseWithoutCloseNotifyAfterTheResponseIsNoFailure() {
        let answered = InterceptState()
        answered.record.withLock { $0.status = 200 }
        answered.noteTLS(NIOSSLError.uncleanShutdown, reason: "upstream TLS: uncleanShutdown")
        #expect(answered.record.withLock { $0.decision == .allow && $0.reason == nil })

        // Before any response, the same close cut the exchange short.
        let cut = InterceptState()
        cut.noteTLS(NIOSSLError.uncleanShutdown, reason: "upstream TLS: uncleanShutdown")
        #expect(cut.record.withLock { $0.decision == .fail && $0.reason == "upstream TLS: uncleanShutdown" })

        let other = InterceptState()
        other.record.withLock { $0.status = 200 }
        other.noteTLS(NIOSSLError.handshakeFailed(.sslError([])), reason: "guest TLS: handshake")
        #expect(other.record.withLock { $0.decision == .fail })
    }

    @Test func onlyDNSNamesBoundToAGrantedSecretAreIntercepted() {
        let d = Self.definitions
        #expect(EgressProxy.intercepts(host: "api.example.com", port: 443, granted: ["gh"], definitions: d))
        #expect(!EgressProxy.intercepts(host: "api.example.com", port: 443, granted: ["npm"], definitions: d))
        #expect(!EgressProxy.intercepts(host: "api.example.com", port: 8443, granted: ["gh"], definitions: d))
        #expect(!EgressProxy.intercepts(host: "other.example.com", port: 443, granted: ["gh"], definitions: d))
        let ip = [try! SecretDefinition(name: "ip", source: .keychain, hosts: ["93.184.215.14"])]
        #expect(!EgressProxy.intercepts(host: "93.184.215.14", port: 443, granted: ["ip"], definitions: ip))
    }

    @Test func theCAIsMadeOnceAndItsKeyStaysPrivate() throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let a = try EgressCA.loadOrCreate(home: home), b = try EgressCA.loadOrCreate(home: home)
        #expect(a.certificatePEM == b.certificatePEM)
        let attributes = { try FileManager.default.attributesOfItem(atPath: $0)[.posixPermissions] as? Int }
        #expect(try attributes(EgressCA.directory(home)) == 0o700)
        #expect(try attributes(EgressCA.directory(home) + "/ca.key") == 0o600)
    }
}

extension Result {
    var failure: Failure? { if case .failure(let f) = self { f } else { nil } }
}

