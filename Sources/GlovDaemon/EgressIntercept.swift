import Foundation
import GlovCore
import GlovDaemonAPI
import NIOCore
import NIOPosix
import NIOSSL
import Synchronization

/// TLS for the hosts a granted secret is bound to (docs/networking.md "Secrets"), shared by every VM's proxy: the egress
/// CA, a server context per host signed by it, and the client context upstream connections are verified with.
final class EgressTLS: Sendable {
    /// A host certificate is replaced once it is this old, well inside `EgressCA.leafLifetime`.
    static let leafRenewal: TimeInterval = 86400

    let ca: EgressCA
    let upstream: NIOSSLContext
    let group: any EventLoopGroup
    private let upstreamTrust: NIOSSLTrustRoots
    private let servers = Mutex<[String: (context: NIOSSLContext, made: Date)]>([:])
    /// Upstream contexts that also trust a VM's `glov vm trust` certificates, by their PEM text.
    private let extended = Mutex<[String: NIOSSLContext]>([:])

    /// `upstreamTrust` is the system trust store in glovd; tests pass their own roots.
    init(ca: EgressCA, upstreamTrust: NIOSSLTrustRoots = .default, group: any EventLoopGroup = MultiThreadedEventLoopGroup.singleton) throws {
        self.ca = ca
        self.group = group
        self.upstreamTrust = upstreamTrust
        upstream = try NIOSSLContext(configuration: Self.clientConfiguration(upstreamTrust, extra: []))
    }

    private static func clientConfiguration(_ trust: NIOSSLTrustRoots, extra: [NIOSSLCertificate]) -> TLSConfiguration {
        var client = TLSConfiguration.makeClientConfiguration()
        client.trustRoots = trust
        if !extra.isEmpty { client.additionalTrustRoots = [.certificates(extra)] }
        // One request per connection, framed by HTTP/1.1: never HTTP/2.
        client.applicationProtocols = ["http/1.1"]
        return client
    }

    /// The upstream context for a VM that trusts `certificates` (PEM) besides the Mac's roots; `upstream` without any.
    func upstream(trusting certificates: [String]) throws -> NIOSSLContext {
        guard !certificates.isEmpty else { return upstream }
        let key = certificates.joined()
        if let context = extended.withLock({ $0[key] }) { return context }
        let extra = try certificates.map { try NIOSSLCertificate(bytes: Array($0.utf8), format: .pem) }
        let context = try NIOSSLContext(configuration: Self.clientConfiguration(upstreamTrust, extra: extra))
        extended.withLock { $0[key] = context }
        return context
    }

    func serverContext(for host: String, now: Date = Date()) throws -> NIOSSLContext {
        if let cached = servers.withLock({ $0[host] }), now.timeIntervalSince(cached.made) < Self.leafRenewal { return cached.context }
        let leaf = try ca.leaf(for: host, now: now)
        var server = TLSConfiguration.makeServerConfiguration(
            certificateChain: [.certificate(try NIOSSLCertificate(bytes: Array(leaf.certificatePEM.utf8), format: .pem))],
            privateKey: .privateKey(try NIOSSLPrivateKey(bytes: Array(leaf.keyPEM.utf8), format: .pem)))
        server.applicationProtocols = ["http/1.1"]
        let context = try NIOSSLContext(configuration: server)
        servers.withLock { $0[host] = (context, now) }
        return context
    }
}

extension EgressProxy {
    /// The git secret granted to the VM that `host:port` belongs to, if any: its requests are held to the VM's repositories.
    static func forge(host: String, port: Int, granted: [String], definitions: [SecretDefinition]) -> (forge: Forge, secret: String)? {
        guard !EgressNet.isNumeric(host) else { return nil }
        for d in definitions where granted.contains(d.name) {
            if let f = d.forge, EgressPolicy(allow: d.hosts).allows(host: host, port: port) { return (f, d.name) }
        }
        return nil
    }

    /// Whether a CONNECT to `host:port` is intercepted: a DNS name some secret granted to the VM is bound to. An IP
    /// literal never is, since its certificate could not be checked by name upstream.
    static func intercepts(host: String, port: Int, granted: [String], definitions: [SecretDefinition]) -> Bool {
        guard !EgressNet.isNumeric(host) else { return false }
        return definitions.contains { granted.contains($0.name) && EgressPolicy(allow: $0.hosts).allows(host: host, port: port) }
    }

    /// Serves a CONNECT to a host a granted secret is bound to: answers 200, terminates the guest's TLS with a
    /// certificate from the egress CA, substitutes placeholders in the one request head, and sends the request over TLS
    /// verified against the system trust store, connecting to the first of `addresses` that answers. The caller closes `fd`.
    @available(*, noasync, message: "blocks on sockets; run it on a thread of its own")
    func intercept(
        _ fd: Int32, addresses: [String], _ request: Request, granted: [String], definitions: [SecretDefinition],
        endpoints: [TokenExchange.Endpoint] = [], tls: EgressTLS, forge: InterceptSetup.ForgeCheck? = nil, trust: [String] = []
    ) {
        let state = InterceptState()
        defer {
            let r = state.record.withLock { $0 }
            log(EgressRecord(
                vm: vm, decision: r.decision, host: request.host, port: request.port, bytesIn: r.bytesIn, bytesOut: r.bytesOut, reason: r.reason,
                method: r.method, path: r.path, status: r.status, secrets: r.secrets.isEmpty ? nil : r.secrets))
        }
        func fail(_ reason: String) { state.record.withLock { $0.decision = .fail; $0.reason = reason } }
        let server: NIOSSLContext, upstream: NIOSSLContext
        do { server = try tls.serverContext(for: request.host) } catch { return fail("no certificate for the host: \(error)") }
        do { upstream = try tls.upstream(trusting: trust) } catch { return fail("cannot load the VM's trusted certificates: \(error)") }
        guard Self.writeAll(fd, Data("HTTP/1.1 200 Connection Established\r\n\r\n".utf8)) else { return fail("guest closed") }
        // NIO runs the TLS on a socket pair, so the guest's own socket (vsock) stays with the plain bridge below.
        var pair: [Int32] = [0, 0]
        guard socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0 else { return fail("socketpair: \(String(cString: strerror(errno)))") }
        let bridged = DispatchSemaphore(value: 0)
        let near = pair[0]
        Thread.detachNewThread {
            let (out, into, _) = Self.bridge(fd, near)
            close(near)
            state.record.withLock { $0.bytesOut = out; $0.bytesIn = into }
            bridged.signal()
        }
        defer { bridged.wait() }
        let setup = InterceptSetup(
            proxy: self, request: request, granted: granted, definitions: definitions, endpoints: endpoints, tls: tls, upstream: upstream,
            addresses: addresses, forge: forge, state: state)
        let guest: any Channel
        do {
            guest = try ClientBootstrap(group: tls.group)
                .channelOption(.autoRead, value: true)
                .channelInitializer { channel in
                    channel.eventLoop.makeCompletedFuture {
                        try channel.pipeline.syncOperations.addHandler(NIOSSLServerHandler(context: server))
                        try channel.pipeline.syncOperations.addHandler(InterceptHeadHandler(setup))
                    }
                }
                .withConnectedSocket(pair[1]).wait()
        } catch {
            close(pair[1])
            return fail("cannot serve TLS: \(error)")
        }
        try? guest.closeFuture.wait()
        if let upstream = state.upstream.withLock({ $0 }) { try? upstream.closeFuture.wait() }
    }
}

/// What an intercepted connection did, for its egress.log line.
final class InterceptState: Sendable {
    struct Record {
        var decision: EgressRecord.Decision = .allow
        var reason: String?
        var method: String?
        var path: String?
        var status: Int?
        var secrets: [String] = []
        var bytesIn = 0
        var bytesOut = 0
    }

    let record = Mutex(Record())
    let upstream = Mutex<(any Channel)?>(nil)

    /// Records a TLS error as the connection's failure, unless it is only a close without close_notify after the
    /// response began: servers and clients commonly end a `Connection: close` exchange that way, and the response
    /// reached the guest as it came.
    func noteTLS(_ error: any Error, reason: String) {
        let unclean = (error as? NIOSSLError) == .uncleanShutdown
        record.withLock { r in
            guard r.reason == nil, !(unclean && r.status != nil) else { return }
            r.decision = .fail
            r.reason = reason
        }
    }
}

struct InterceptSetup: Sendable {
    let proxy: EgressProxy
    let request: EgressProxy.Request
    let granted: [String]
    let definitions: [SecretDefinition]
    /// The subscription token endpoints on this host.
    let endpoints: [TokenExchange.Endpoint]
    let tls: EgressTLS
    /// What the upstream certificate is verified with: the Mac's roots and the VM's trusted certificates.
    let upstream: NIOSSLContext
    /// The upstream host's reachable addresses, tried in order.
    let addresses: [String]
    /// A git host: the forge, the granted git secret (whose value resolves GraphQL node IDs) and the VM's repositories.
    struct ForgeCheck: Sendable {
        var forge: Forge
        var secret: String
        var repos: [RepoPattern]
    }
    let forge: ForgeCheck?
    let state: InterceptState
}

/// The guest side after TLS: reads one request head, refuses it or substitutes its placeholders, then connects
/// upstream and glues the two channels. Runs on the guest channel's event loop.
final class InterceptHeadHandler: ChannelInboundHandler, RemovableChannelHandler {
    typealias InboundIn = ByteBuffer
    typealias OutboundOut = ByteBuffer

    private enum Phase { case head, body, waiting, forwarding, answering, done }

    /// A request to a token endpoint, read whole before it goes upstream.
    private struct Exchange {
        var endpoint: TokenExchange.Endpoint
        var length: Int
        var format: TokenExchange.Format?
    }

    private let setup: InterceptSetup
    private var phase = Phase.head
    private var buffer = ByteBuffer()
    private var headTimeout: Scheduled<Void>?
    private var plan: SecretSubstitution.Plan?
    private var exchange: Exchange?
    /// A GraphQL request being read whole: its plan, the forge check, and the body's length.
    private var graphQL: (SecretSubstitution.Plan, InterceptSetup.ForgeCheck, Int)?
    /// The values put into a token request, which its answer must not carry back to the guest.
    private var sent: [String: String] = [:]

    init(_ setup: InterceptSetup) { self.setup = setup }

    func handlerAdded(context: ChannelHandlerContext) {
        let loop = context.eventLoop
        let bound = NIOLoopBound((self, context), eventLoop: loop)
        headTimeout = loop.scheduleTask(in: .seconds(Int64(EgressProxy.headTimeout.components.seconds))) {
            let (handler, context) = bound.value
            if handler.phase == .head { handler.refuse(context, .init(status: 408, message: "no request within \(EgressProxy.headTimeout)", reason: "request timeout")) }
        }
    }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        var bytes = unwrapInboundIn(data)
        switch phase {
        case .forwarding: context.fireChannelRead(data)
        case .done, .answering: break
        case .waiting: buffer.writeBuffer(&bytes)
        case .body:
            buffer.writeBuffer(&bytes)
            bodyArrived(context)
        case .head:
            buffer.writeBuffer(&bytes)
            let view = buffer.readableBytesView
            guard let end = view.firstRange(of: Array("\r\n\r\n".utf8)) else {
                if buffer.readableBytes > EgressProxy.maxHead { refuse(context, .init(status: 400, message: "request head over \(EgressProxy.maxHead) bytes", reason: "malformed request")) }
                return
            }
            let headLength = end.upperBound - view.startIndex
            guard headLength <= EgressProxy.maxHead else {
                return refuse(context, .init(status: 400, message: "request head over \(EgressProxy.maxHead) bytes", reason: "malformed request"))
            }
            let head = Data(buffer.readBytes(length: headLength)!)
            headTimeout?.cancel()
            let request = setup.request
            switch SecretSubstitution.plan(head, host: request.host, port: request.port, vm: setup.proxy.vm, granted: setup.granted, definitions: setup.definitions) {
            case .failure(let refusal): refuse(context, refusal)
            case .success(let plan):
                setup.state.record.withLock { $0.method = plan.method; $0.path = plan.path; $0.secrets = plan.secrets }
                if plan.method == "POST", let endpoint = setup.endpoints.first(where: { $0.path == plan.path }) {
                    return readBody(context, plan, endpoint)
                }
                // Only a request that signs in is held to the VM's repositories: an anonymous one (a public clone)
                // reaches what anyone on the internet can.
                if let check = setup.forge, !plan.secrets.isEmpty {
                    let target = RepoAccess.target(check.forge, host: request.host, path: plan.path)
                    if target == .graphQL {
                        guard plan.method == "POST" else { return refuseRepo(context, "a GraphQL request that is not a POST") }
                        return readGraphQL(context, plan, check)
                    }
                    if let reason = RepoAccess.refusal(target, method: plan.method, host: RepoAccess.repoHost(check.forge, request.host), allowed: check.repos) {
                        return refuseRepo(context, reason)
                    }
                }
                phase = .waiting
                fetch(context, plan)
            }
        }
    }

    func channelInactive(context: ChannelHandlerContext) {
        phase = .done
        context.fireChannelInactive()
    }

    func errorCaught(context: ChannelHandlerContext, error: any Error) {
        // A guest that does not trust the egress CA fails its handshake here.
        let reason = "\(error)".contains("UNKNOWN_CA")
            ? "guest TLS: the guest does not trust the egress CA; recreate a VM made before the CA" : "guest TLS: \(error)"
        setup.state.noteTLS(error, reason: reason)
        context.close(promise: nil)
    }

    /// A 403 for a request outside the VM's repositories, naming the command that allows one.
    private func refuseRepo(_ context: ChannelHandlerContext, _ reason: String) {
        let hint = "; allow a repository with glov secret repos \(setup.proxy.vm) add <host>/<owner>/<repo>"
        refuse(context, .init(status: 403, message: reason + hint, reason: reason))
    }

    /// Reads a GraphQL request's body whole, checks every repository it names (node IDs resolved through the forge
    /// with the VM's git secret), then sends it on as it came.
    private func readGraphQL(_ context: ChannelHandlerContext, _ plan: SecretSubstitution.Plan, _ check: InterceptSetup.ForgeCheck) {
        func header(_ name: String) -> String? { plan.headers.first { $0.name.lowercased() == name }?.value }
        if header("transfer-encoding") != nil { return refuseRepo(context, "a GraphQL request without a Content-Length") }
        let length = header("content-length").flatMap { Int($0) } ?? 0
        guard length >= 0, length <= Self.maxGraphQL else { return refuseRepo(context, "a GraphQL request over \(Self.maxGraphQL >> 10) KiB") }
        var plan = plan
        plan.headers.removeAll { $0.name.lowercased() == "content-length" }
        graphQL = (plan, check, length)
        phase = .body
        bodyArrived(context)
    }

    static let maxGraphQL = 256 << 10

    /// Decides a GraphQL body by `GraphQLRepos`, once its node IDs are resolved with the VM's own secret.
    private func checkGraphQL(_ context: ChannelHandlerContext, _ body: Data) {
        guard let (plan, check, _) = graphQL else { return }
        let host = RepoAccess.repoHost(check.forge, setup.request.host)
        let request: GraphQLRepos.Request
        switch GraphQLRepos.parse(body) {
        case .failure(let refusal): return refuseRepo(context, refusal.reason)
        case .success(let r): request = r
        }
        phase = .waiting
        let value = setup.proxy.secretValue, resolve = setup.proxy.resolveNodes, requestHost = setup.request.host
        context.eventLoop.makeFutureWithTask { () -> String? in
            var nodes: [String: String?] = [:]
            if !request.nodeIDs.isEmpty {
                do {
                    let token = try await value(check.secret)
                    nodes = try await resolve(requestHost, token, request.nodeIDs.sorted())
                } catch {
                    return "cannot tell the repositories of the request's node IDs: \(error)"
                }
            }
            return GraphQLRepos.refusal(request, host: host, nodes: nodes) { repo in
                check.repos.contains { $0.matches(host: host, repo: repo) }
            }
        }.assumeIsolated().whenComplete { result in
            guard self.phase == .waiting else { return }
            switch result {
            case .failure(let error): self.refuseRepo(context, "\(error)")
            case .success(let reason?): self.refuseRepo(context, reason)
            case .success(nil): self.fetch(context, plan) { _ in body }
            }
        }
    }

    /// Starts reading the body of a token endpoint request, which must have a length: the proxy rewrites it whole.
    private func readBody(_ context: ChannelHandlerContext, _ plan: SecretSubstitution.Plan, _ endpoint: TokenExchange.Endpoint) {
        func header(_ name: String) -> String? { plan.headers.first { $0.name.lowercased() == name }?.value }
        if header("transfer-encoding") != nil {
            return refuse(context, .init(status: 411, message: "a token request needs a Content-Length", reason: "token request without length"))
        }
        let length = header("content-length").flatMap { Int($0) } ?? 0
        guard length >= 0, length <= TokenExchange.maxBody else {
            return refuse(context, .init(status: 413, message: "token request body over \(TokenExchange.maxBody) bytes", reason: "token request too large"))
        }
        var plan = plan
        // The body changes length, and the answer is read whole, so it must come unencoded.
        plan.headers.removeAll { ["content-length", "accept-encoding"].contains($0.name.lowercased()) }
        self.plan = plan
        exchange = Exchange(endpoint: endpoint, length: length, format: TokenExchange.format(header("content-type")))
        phase = .body
        bodyArrived(context)
    }

    private func bodyArrived(_ context: ChannelHandlerContext) {
        if let (_, _, length) = graphQL {
            guard phase == .body, buffer.readableBytes >= length else { return }
            let body = Data(buffer.readBytes(length: length)!)
            return checkGraphQL(context, body)
        }
        guard let exchange, var plan, buffer.readableBytes >= exchange.length else { return }
        let body = Data(buffer.readBytes(length: exchange.length)!)
        buffer.clear()
        let request = setup.request
        let names = exchange.format.map { TokenExchange.names(in: body, $0) } ?? []
        if let refusal = SecretSubstitution.check(names, host: request.host, port: request.port, vm: setup.proxy.vm, granted: setup.granted, definitions: setup.definitions) {
            return refuse(context, refusal)
        }
        for name in names where !plan.secrets.contains(name) { plan.secrets.append(name) }
        setup.state.record.withLock { $0.secrets = plan.secrets }
        phase = .waiting
        fetch(context, plan) { values in
            guard let format = exchange.format, !names.isEmpty else { return body }
            return TokenExchange.substitute(body, format, values: values)
        }
    }

    private func refuse(_ context: ChannelHandlerContext, _ refusal: SecretSubstitution.Refusal) {
        phase = .done
        headTimeout?.cancel()
        setup.state.record.withLock {
            $0.decision = refusal.status >= 500 ? .fail : .deny
            $0.reason = refusal.reason
            $0.status = refusal.status
        }
        let body = "glov egress proxy: \(setup.request.target): \(refusal.message)\n"
        let text = "HTTP/1.1 \(EgressProxy.statusLine(refusal.status))\r\nContent-Type: text/plain\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)"
        context.writeAndFlush(wrapOutboundOut(ByteBuffer(string: text))).assumeIsolated().whenComplete { _ in context.close(promise: nil) }
    }

    /// Looks up the values of `plan.secrets`, builds the request, then connects upstream and sends it: every refusal
    /// comes before the connection. `body`, for a token endpoint request, gives its body with the values in it; nil
    /// when it cannot.
    private func fetch(
        _ context: ChannelHandlerContext, _ plan: SecretSubstitution.Plan, body: (([String: String]) -> Data?)? = nil
    ) {
        let value = setup.proxy.secretValue
        let names = plan.secrets
        context.eventLoop.makeFutureWithTask { () -> Result<[String: String], SecretSubstitution.Refusal> in
            var values: [String: String] = [:]
            for name in names {
                do { values[name] = try await value(name) } catch {
                    return .failure(.init(status: 502, message: "\(error)", reason: "secret \(name) unavailable"))
                }
            }
            return .success(values)
        }.assumeIsolated().whenComplete { result in
            guard self.phase == .waiting else { return }
            switch result {
            case .failure(let error): self.refuse(context, .init(status: 502, message: "\(error)", reason: "secret lookup failed"))
            case .success(.failure(let refusal)): self.refuse(context, refusal)
            case .success(.success(let values)):
                guard let body else {
                    switch SecretSubstitution.apply(plan, values: values) {
                    case .failure(let refusal): self.refuse(context, refusal)
                    case .success(let head): self.connect(context, head)
                    }
                    return
                }
                guard let content = body(values) else {
                    return self.refuse(context, .init(status: 400, message: "the token request body does not parse", reason: "malformed token request"))
                }
                var framed = plan
                framed.headers.insert(("Content-Length", "\(content.count)"), at: framed.headers.count - 1)
                switch SecretSubstitution.apply(framed, values: values) {
                case .failure(let refusal): self.refuse(context, refusal)
                case .success(let head):
                    self.sent = values
                    self.connect(context, head + content)
                }
            }
        }
    }

    /// Connects to the first of the host's reachable addresses that answers, off the event loop, then sends `head`.
    private func connect(_ context: ChannelHandlerContext, _ head: Data) {
        let connect = setup.proxy.connect, addresses = setup.addresses, port = setup.request.port
        context.eventLoop.makeFutureWithTask {
            await Blocking.run { () -> (fd: Int32?, errno: Int32) in
                var failure: Int32 = 0
                for address in addresses {
                    if let fd = connect(address, port) { return (fd, 0) }
                    failure = errno
                }
                return (nil, failure)
            }
        }.assumeIsolated().whenComplete { result in
            // The guest went away meanwhile.
            guard self.phase == .waiting else {
                if case .success(let r) = result, let fd = r.fd { close(fd) }
                return
            }
            guard case .success(let r) = result, let fd = r.fd else {
                var reason = "unreachable"
                if case .success(let r) = result { reason = EgressNet.unreachable(self.setup.addresses, local: [], errno: r.errno) }
                return self.refuse(context, .init(status: 502, message: "unreachable", reason: reason))
            }
            self.send(context, head, fd: fd)
        }
    }

    /// Owns `fd` from here.
    private func send(_ context: ChannelHandlerContext, _ head: Data, fd: Int32) {
        let (guestGlue, upstreamGlue) = GlueHandler.matchedPair()
        let loop = context.eventLoop
        let carried = NIOLoopBound(upstreamGlue, eventLoop: loop)
        let upstream = setup.upstream, host = setup.request.host, state = setup.state
        let sent = self.sent
        let carriedAnswer = exchange.map { NIOLoopBound(TokenAnswerHandler(guest: context, endpoint: $0.endpoint, sent: sent, setup: setup), eventLoop: loop) }
        ClientBootstrap(group: loop)
            .channelInitializer { channel in
                channel.eventLoop.makeCompletedFuture {
                    try channel.pipeline.syncOperations.addHandler(NIOSSLClientHandler(context: upstream, serverHostname: host))
                    try channel.pipeline.syncOperations.addHandler(UpstreamStatusHandler(state))
                    if let answer = carriedAnswer?.value {
                        try channel.pipeline.syncOperations.addHandler(answer)
                    } else {
                        try channel.pipeline.syncOperations.addHandler(carried.value)
                    }
                }
            }
            .withConnectedSocket(fd)
            .assumeIsolated().whenComplete { result in
                switch result {
                case .failure(let error):
                    // Not closed here: NIO may already have closed it, and a second close could hit a reused descriptor.
                    self.refuse(context, .init(status: 502, message: "upstream: \(error)", reason: "upstream unusable"))
                case .success(let upstream):
                    state.upstream.withLock { $0 = upstream }
                    guard self.phase == .waiting else { return upstream.close(promise: nil) }
                    if self.exchange != nil {
                        self.phase = .answering
                        return upstream.writeAndFlush(ByteBuffer(bytes: head), promise: nil)
                    }
                    self.phase = .forwarding
                    do { try context.pipeline.syncOperations.addHandler(guestGlue, position: .after(self)) } catch {
                        upstream.close(promise: nil)
                        return self.refuse(context, .init(status: 502, message: "\(error)", reason: "proxy error"))
                    }
                    var first = ByteBuffer(bytes: head)
                    first.writeBuffer(&self.buffer)
                    upstream.writeAndFlush(first, promise: nil)
                }
            }
    }
}

/// The upstream side of a token endpoint request: reads the whole answer, keeps the tokens the endpoint declares as
/// secrets on the Mac, and gives the guest the answer with placeholders in their place. A token never reaches the guest:
/// an answer that cannot be read or kept is a 502.
final class TokenAnswerHandler: ChannelInboundHandler {
    typealias InboundIn = ByteBuffer

    private let guest: ChannelHandlerContext
    private let endpoint: TokenExchange.Endpoint
    private let sent: [String: String]
    private let setup: InterceptSetup
    private var answer = Data()
    private var over = false
    private var finished = false

    init(guest: ChannelHandlerContext, endpoint: TokenExchange.Endpoint, sent: [String: String], setup: InterceptSetup) {
        self.guest = guest
        self.endpoint = endpoint
        self.sent = sent
        self.setup = setup
    }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        var bytes = unwrapInboundIn(data)
        guard !over else { return }
        answer += bytes.readBytes(length: bytes.readableBytes) ?? []
        if answer.count > TokenExchange.maxAnswer {
            over = true
            answer = Data()
            context.close(promise: nil)
        }
    }

    func errorCaught(context: ChannelHandlerContext, error: any Error) {
        // A close without close_notify ends the answer as a close would; anything else is reported as the guest's failure.
        if (error as? NIOSSLError) != .uncleanShutdown { setup.state.noteTLS(error, reason: "upstream TLS: \(error)") }
        context.close(promise: nil)
    }

    func channelInactive(context: ChannelHandlerContext) {
        guard !finished else { return }
        finished = true
        guard !over, var response = TokenExchange.parse(answer) else {
            return reply(status: 502, reason: over ? "token answer too large" : "token answer unreadable", message: "the token endpoint's answer could not be read")
        }
        // An error that echoes the request would hand the guest what the proxy put in it.
        response.body = TokenExchange.masking(sent, in: response.body)
        guard (200..<300).contains(response.status), !endpoint.answer.isEmpty else { return send(response.framed) }
        guard let (body, kept) = TokenExchange.keep(response.body, answer: endpoint.answer) else {
            return reply(status: 502, reason: "token answer not JSON", message: "the token endpoint's answer is not a JSON object")
        }
        var rewritten = response
        rewritten.body = body
        let store = setup.proxy.storeSecret
        let names = kept.map(\.0.name)
        setup.state.record.withLock { r in for name in names where !r.secrets.contains(name) { r.secrets.append(name) } }
        let bound = NIOLoopBound(self, eventLoop: guest.eventLoop)
        guest.eventLoop.makeFutureWithTask {
            for (secret, value) in kept { try await store(secret.name, secret.hosts, value) }
        }.assumeIsolated().whenComplete { result in
            switch result {
            case .success: bound.value.send(rewritten.framed)
            case .failure(let error): bound.value.reply(status: 502, reason: "token not kept", message: "cannot keep the tokens: \(error)")
            }
        }
    }

    private func reply(status: Int, reason: String, message: String) {
        setup.state.record.withLock { $0.decision = .fail; $0.reason = reason; $0.status = status }
        let body = "glov egress proxy: \(setup.request.target): \(message)\n"
        send(Data("HTTP/1.1 \(EgressProxy.statusLine(status))\r\nContent-Type: text/plain\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)".utf8))
    }

    private func send(_ data: Data) {
        let guest = self.guest
        guest.writeAndFlush(NIOAny(ByteBuffer(bytes: data))).assumeIsolated().whenComplete { _ in guest.close(promise: nil) }
    }
}

/// Records the upstream response's status code from its first line, then steps aside.
final class UpstreamStatusHandler: ChannelInboundHandler, RemovableChannelHandler {
    typealias InboundIn = ByteBuffer
    private let state: InterceptState
    private var seen = false

    init(_ state: InterceptState) { self.state = state }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        if !seen {
            seen = true
            let bytes = unwrapInboundIn(data)
            let line = bytes.getString(at: bytes.readerIndex, length: min(bytes.readableBytes, 16)) ?? ""
            let parts = line.split(separator: " ")
            if parts.count >= 2, let status = Int(parts[1]) { state.record.withLock { $0.status = status } }
        }
        context.fireChannelRead(data)
    }

    func errorCaught(context: ChannelHandlerContext, error: any Error) {
        // An upstream certificate the Mac does not trust fails here, as it would for a direct connection.
        state.noteTLS(error, reason: "upstream TLS: \(error)")
        context.fireErrorCaught(error)
    }
}

/// Copies between two channels on one event loop, reading from one only while the other can take writes. Closing
/// either side closes the other once what it was sent is written.
final class GlueHandler: ChannelDuplexHandler {
    typealias InboundIn = NIOAny
    typealias OutboundIn = NIOAny
    typealias OutboundOut = NIOAny

    private var partner: GlueHandler?
    private var context: ChannelHandlerContext?
    private var pendingRead = false
    private var lastWrite: EventLoopFuture<Void>?

    static func matchedPair() -> (GlueHandler, GlueHandler) {
        let a = GlueHandler(), b = GlueHandler()
        a.partner = b
        b.partner = a
        return (a, b)
    }

    func handlerAdded(context: ChannelHandlerContext) {
        self.context = context
        // The partner's channel may have asked to read before this side existed to take the bytes.
        partner?.partnerBecameWritable()
    }

    func handlerRemoved(context: ChannelHandlerContext) {
        self.context = nil
        partner = nil
    }

    private func partnerWrite(_ data: NIOAny) {
        guard let context else { return }
        let promise = context.eventLoop.makePromise(of: Void.self)
        context.write(data, promise: promise)
        lastWrite = promise.futureResult
    }

    private func partnerFlush() { context?.flush() }

    private func partnerClose() {
        guard let context else { return }
        context.flush()
        if let lastWrite {
            lastWrite.assumeIsolated().whenComplete { _ in context.close(promise: nil) }
        } else {
            context.close(promise: nil)
        }
    }

    private func partnerBecameWritable() {
        if pendingRead {
            pendingRead = false
            context?.read()
        }
    }

    private var partnerWritable: Bool { context?.channel.isWritable ?? false }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) { partner?.partnerWrite(data) }
    func channelReadComplete(context: ChannelHandlerContext) { partner?.partnerFlush() }

    func channelInactive(context: ChannelHandlerContext) {
        partner?.partnerClose()
        context.fireChannelInactive()
    }

    func errorCaught(context: ChannelHandlerContext, error: any Error) {
        partner?.partnerClose()
        context.close(promise: nil)
    }

    func channelWritabilityChanged(context: ChannelHandlerContext) {
        if context.channel.isWritable { partner?.partnerBecameWritable() }
        context.fireChannelWritabilityChanged()
    }

    func read(context: ChannelHandlerContext) {
        if let partner, partner.partnerWritable { context.read() } else { pendingRead = true }
    }
}

