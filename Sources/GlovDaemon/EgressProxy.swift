import Foundation
import GlovCore
import GlovDaemonAPI
import Synchronization

/// The host end of an isolated VM's egress proxy (docs/guest-protocol.md "Isolated VM egress proxy"): one guest
/// connection to vsock port 4024 carries one HTTP proxy request, which `serve` allows or denies by the VM's allowlist.
/// The guest is assumed hostile, so every decision is made here, on names and addresses the host checked itself.
struct EgressProxy: Sendable {
    static let maxHead = 16 << 10
    static let headTimeout: Duration = .seconds(10)

    let vm: String
    /// The VM's egress settings as vm.json has them now, so a change applies to the next connection.
    let egress: @Sendable () throws -> VMConfig.Egress
    /// The numeric addresses of a host name or literal, resolved on the Mac.
    let resolve: @Sendable (String, Int) -> [String]
    /// The numeric addresses of the Mac's own interfaces.
    let localAddresses: @Sendable () -> [String]
    /// A connected, blocking socket to a numeric address, or nil.
    let connect: @Sendable (String, Int) -> Int32?
    /// One metadata line per connection; never a body or a header value.
    let log: @Sendable (EgressRecord) -> Void
    /// TLS for hosts a granted secret is bound to; nil intercepts nothing.
    let tls: EgressTLS?
    /// Every secret defined on the Mac, and one secret's value (docs/networking.md "Secrets").
    let secretDefinitions: @Sendable () throws -> [SecretDefinition]
    let secretValue: @Sendable (String) async throws -> String
    /// Keeps a token from a subscription's token endpoint answer as a secret bound to the hosts given.
    let storeSecret: @Sendable (_ name: String, _ hosts: [String], _ value: String) async throws -> Void
    /// The repository (`owner/name`) of each GitHub node ID, nil for a node of no repository (a user); throws for one
    /// it cannot tell (`GitHubNodes`).
    let resolveNodes: @Sendable (_ host: String, _ token: String, _ ids: [String]) async throws -> [String: String?]

    init(
        vm: String, egress: @escaping @Sendable () throws -> VMConfig.Egress, resolve: @escaping @Sendable (String, Int) -> [String] = EgressNet.resolve,
        localAddresses: @escaping @Sendable () -> [String] = EgressNet.localAddresses, connect: @escaping @Sendable (String, Int) -> Int32? = EgressNet.connect,
        log: @escaping @Sendable (EgressRecord) -> Void, tls: EgressTLS? = nil,
        secretDefinitions: @escaping @Sendable () throws -> [SecretDefinition] = { [] },
        secretValue: @escaping @Sendable (String) async throws -> String = { throw RPCError(code: RPCError.failed, message: "no secret named \($0)") },
        storeSecret: @escaping @Sendable (String, [String], String) async throws -> Void = { name, _, _ in
            throw RPCError(code: RPCError.failed, message: "cannot keep secret \(name)")
        },
        resolveNodes: @escaping @Sendable (String, String, [String]) async throws -> [String: String?] = GitHubNodes.resolve
    ) {
        self.vm = vm
        self.egress = egress
        self.resolve = resolve
        self.localAddresses = localAddresses
        self.connect = connect
        self.log = log
        self.tls = tls
        self.secretDefinitions = secretDefinitions
        self.secretValue = secretValue
        self.storeSecret = storeSecret
        self.resolveNodes = resolveNodes
    }

    /// The proxy of `config`'s VM in `home`: its settings are vm.json's `egress`, re-read per connection; records go
    /// to the VM's `egress.log` and, as lines, to the daemon log.
    init(config: VMConfig, home: GlovHome, tls: EgressTLS?, secrets: SecretBroker) {
        let vm = config.name
        let records = EgressLog(path: config.egressLogPath)
        let daemon = DaemonLog.vm(vm)
        self.init(
            vm: vm, egress: { try home.loadConfig(vm).egress ?? .init() },
            log: { record in
                daemon(record.line)
                records.append(record)
            }, tls: tls,
            secretDefinitions: { try secrets.definitionsNow() }, secretValue: { try await secrets.value($0) },
            storeSecret: { try await secrets.set($0, hosts: $1, value: $2) })
    }

    struct Request: Equatable {
        /// Lowercased, without a trailing dot; an IPv6 literal without brackets.
        var host: String
        var port: Int
        /// Plain HTTP: the request head to send upstream (origin form, no `Proxy-*` headers, `Connection: close`).
        var upstreamHead: Data?

        var target: String { host.contains(":") ? "[\(host)]:\(port)" : "\(host):\(port)" }
    }

    /// Serves one guest connection and closes `fd`. Blocks until both directions end: call it on a thread of its own.
    @available(*, noasync, message: "blocks on sockets; run it on a thread of its own")
    func serve(_ fd: Int32) {
        defer { close(fd) }
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        guard let (head, rest) = Self.readHead(fd) else {
            reply(fd, "400 Bad Request", nil)
            log(EgressRecord(vm: vm, decision: .deny, host: nil, port: nil, reason: "malformed request"))
            return
        }
        guard let request = Self.parse(head) else {
            reply(fd, "400 Bad Request", nil)
            log(EgressRecord(vm: vm, decision: .deny, host: nil, port: nil, reason: "malformed request"))
            return
        }
        /// Answers without connecting: `deny` (403) for what the policy refuses, `fail` (502) for an allowed target out of reach.
        func refuse(_ reason: String, failed: Bool = false) {
            reply(fd, failed ? "502 Bad Gateway" : "403 Forbidden", "glov egress proxy: \(request.target): \(reason)\n")
            log(EgressRecord(vm: vm, decision: failed ? .fail : .deny, host: request.host, port: request.port, reason: reason))
        }
        guard let settings = try? egress() else { return refuse("not in the allowlist") }
        guard settings.permits(host: request.host, port: request.port) else {
            return refuse(settings.open == true ? "not a host and port" : "not in the allowlist")
        }
        let privateNetworks = settings.reachesPrivateNetworks(host: request.host, port: request.port)
        if let head = request.upstreamHead, SecretSubstitution.mentionsPlaceholder(head + rest) {
            return refuse("a secret placeholder goes only over HTTPS to a host the secret is bound to")
        }
        let granted = settings.secrets ?? []
        let endpoints = TokenExchange.endpoints(vm: vm, subscriptions: settings.subscriptions ?? [])
        var definitions: [SecretDefinition] = []
        if !granted.isEmpty || (tls != nil && request.upstreamHead == nil && !endpoints.isEmpty) {
            do { definitions = try secretDefinitions() } catch { return refuse("cannot read the secrets: \(error)", failed: true) }
        }
        let forge = Self.forge(host: request.host, port: request.port, granted: granted, definitions: definitions)
        let addresses = resolve(request.host, request.port)
        guard !addresses.isEmpty else { return refuse("unresolved", failed: true) }
        let local = localAddresses()
        let reachable = addresses.filter { EgressPolicy.isReachable($0, local: local, privateNetworks: privateNetworks) }
        guard !reachable.isEmpty else { return refuse(privateNetworks ? "no public or private-network address" : "no public address") }
        let tokenHosts = endpoints.filter { $0.host == request.host && request.port == 443 }
        if let tls, request.upstreamHead == nil,
           !tokenHosts.isEmpty || Self.intercepts(host: request.host, port: request.port, granted: granted, definitions: definitions) {
            // Connects only once the request and its secret values are in hand: a value can take seconds (a Keychain
            // prompt, a command source), and a provider closes a connection that sends nothing meanwhile.
            return intercept(
                fd, addresses: reachable, request, granted: granted, definitions: definitions, endpoints: tokenHosts, tls: tls,
                forge: forge.map { .init(forge: $0.forge, secret: $0.secret, repos: (settings.repos ?? []).compactMap(RepoPattern.init)) },
                trust: (settings.trust ?? []).map(\.certificate))
        }
        var connected: Int32?
        var failure: Int32 = 0
        for address in reachable where connected == nil {
            connected = connect(address, request.port)
            if connected == nil { failure = errno }
        }
        guard let upstream = connected else { return refuse(EgressNet.unreachable(reachable, local: local, errno: failure), failed: true) }
        defer { close(upstream) }
        setsockopt(upstream, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        var first = rest
        if let head = request.upstreamHead { first = head + rest } else {
            guard Self.writeAll(fd, Data("HTTP/1.1 200 Connection Established\r\n\r\n".utf8)) else { return }
        }
        if !first.isEmpty { guard Self.writeAll(upstream, first) else { return } }
        let (out, into, reset) = Self.bridge(fd, upstream)
        log(EgressRecord(vm: vm, decision: .allow, host: request.host, port: request.port, bytesIn: into, bytesOut: first.count + out,
            reason: reset ? "upstream reset" : nil))
    }

    static func statusLine(_ status: Int) -> String {
        let reasons = [400: "Bad Request", 403: "Forbidden", 408: "Request Timeout", 502: "Bad Gateway"]
        return "\(status) \(reasons[status] ?? "")"
    }

    func reply(_ fd: Int32, _ status: String, _ body: String?, type: String = "text/plain") {
        let body = body ?? ""
        let text = "HTTP/1.1 \(status)\r\nContent-Type: \(type)\r\nContent-Length: \(body.utf8.count)\r\nConnection: close\r\n\r\n\(body)"
        _ = Self.writeAll(fd, Data(text.utf8))
    }

    /// The request head (through the blank line) and whatever arrived after it, within `maxHead` bytes and `headTimeout`.
    static func readHead(_ fd: Int32) -> (Data, Data)? {
        let deadline = ContinuousClock.now + headTimeout
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while ContinuousClock.now < deadline {
            let left = deadline - ContinuousClock.now
            var tv = timeval(tv_sec: Int(left.components.seconds), tv_usec: min(Int32(left.components.attoseconds / 1_000_000_000_000) + 1, 999_999))
            setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
            let n = read(fd, &buffer, buffer.count)
            if n < 0 && errno == EINTR { continue }
            guard n > 0 else { break }
            data.append(contentsOf: buffer.prefix(n))
            if let end = data.firstRange(of: Data("\r\n\r\n".utf8)) {
                var none = timeval()
                setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &none, socklen_t(MemoryLayout<timeval>.size))
                guard end.upperBound <= maxHead else { return nil }
                return (data.subdata(in: data.startIndex..<end.upperBound), data.subdata(in: end.upperBound..<data.endIndex))
            }
            if data.count > maxHead { return nil }
        }
        return nil
    }

    /// A CONNECT or an absolute-form `http://` request; nil for anything else.
    static func parse(_ head: Data) -> Request? {
        guard let text = String(data: head, encoding: .utf8) else { return nil }
        var lines = text.components(separatedBy: "\r\n")
        while lines.last == "" { lines.removeLast() }
        guard let first = lines.first else { return nil }
        let parts = first.split(separator: " ", omittingEmptySubsequences: false)
        guard parts.count == 3, ["HTTP/1.0", "HTTP/1.1"].contains(parts[2]) else { return nil }
        let method = String(parts[0]), target = String(parts[1])
        guard !method.isEmpty, method.allSatisfy({ $0.isLetter && $0.isASCII }) else { return nil }
        if method == "CONNECT" {
            guard let (host, port) = authority(target), let port else { return nil }
            return Request(host: host, port: port, upstreamHead: nil)
        }
        guard target.lowercased().hasPrefix("http://") else { return nil }
        let afterScheme = target.dropFirst("http://".count)
        let end = afterScheme.firstIndex { $0 == "/" || $0 == "?" } ?? afterScheme.endIndex
        guard let (host, port) = authority(String(afterScheme[..<end])) else { return nil }
        var path = String(afterScheme[end...])
        if !path.hasPrefix("/") { path = "/" + path }
        let request = Request(host: host, port: port ?? 80, upstreamHead: nil)
        var headers: [String] = []
        var hosts = 0
        let token = Set("abcdefghijklmnopqrstuvwxyz0123456789!#$%&'*+-.^_`|~")
        for line in lines.dropFirst() {
            // No folded lines and no space before the colon, so every header name is the one compared below.
            guard let colon = line.firstIndex(of: ":") else { return nil }
            let name = line[..<colon].lowercased()
            guard !name.isEmpty, name.allSatisfy(token.contains) else { return nil }
            if name.hasPrefix("proxy-") || name == "connection" { continue }
            if name == "host" {
                // The upstream serves the site the header names: it must be the target, or an allowed address would front for any other.
                hosts += 1
                let value = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
                guard let (named, namedPort) = authority(value), named == host, (namedPort ?? 80) == request.port else { return nil }
            }
            headers.append(line)
        }
        guard hosts <= 1 else { return nil }
        headers.append("Connection: close")
        let upstream = ([method + " " + path + " " + parts[2]] + headers).joined(separator: "\r\n") + "\r\n\r\n"
        return Request(host: host, port: request.port, upstreamHead: Data(upstream.utf8))
    }

    /// `host[:port]` or `[v6][:port]`; the host must be a DNS name or an IP literal.
    static func authority(_ text: String) -> (String, Int?)? {
        var host: Substring, port: Substring?
        if text.hasPrefix("[") {
            guard let close = text.firstIndex(of: "]") else { return nil }
            host = text[text.index(after: text.startIndex)..<close]
            let rest = text[text.index(after: close)...]
            if rest.hasPrefix(":") { port = rest.dropFirst() } else if !rest.isEmpty { return nil }
            guard EgressNet.isNumeric(String(host)) else { return nil }
        } else {
            let pieces = text.split(separator: ":", omittingEmptySubsequences: false)
            guard pieces.count <= 2 else { return nil }
            host = pieces[0]
            port = pieces.count == 2 ? pieces[1] : nil
            let allowed = Set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_")
            guard !host.isEmpty, host.count <= 253, host.allSatisfy(allowed.contains) else { return nil }
        }
        var name = host.lowercased()
        if name.hasSuffix(".") { name.removeLast() }
        guard !name.isEmpty else { return nil }
        guard let port else { return (name, nil) }
        guard port.allSatisfy(\.isASCII), let number = Int(port), (1...65535).contains(number) else { return nil }
        return (name, number)
    }

    static func writeAll(_ fd: Int32, _ data: Data) -> Bool {
        var offset = 0
        while offset < data.count {
            let n = data.withUnsafeBytes { write(fd, $0.baseAddress!.advanced(by: offset), data.count - offset) }
            if n < 0 && errno == EINTR { continue }
            guard n > 0 else { return false }
            offset += n
        }
        return true
    }

    /// Copies both ways until both directions end; returns the bytes copied guest to upstream (out) and back (in), and
    /// whether upstream reset the connection.
    static func bridge(_ guest: Int32, _ upstream: Int32) -> (out: Int, into: Int, upstreamReset: Bool) {
        /// The bytes copied, and whether reading `source` ended in a reset.
        @Sendable func copy(_ source: Int32, _ destination: Int32) -> (Int, reset: Bool) {
            var total = 0, reset = false
            var buffer = [UInt8](repeating: 0, count: 65536)
            while true {
                let n = buffer.withUnsafeMutableBytes { UnixSocket.readWhenReadable(source, $0) }
                if n < 0 && errno == EINTR { continue }
                if n < 0 && errno == ECONNRESET { reset = true }
                if n <= 0 { break }
                guard writeAll(destination, Data(buffer.prefix(n))) else { break }
                total += n
            }
            shutdown(destination, SHUT_WR)
            return (total, reset)
        }
        // Written before the signal and read after the wait, so the semaphore orders the two.
        final class Count: @unchecked Sendable { var value = 0, reset = false }
        let into = Count()
        let done = DispatchSemaphore(value: 0)
        Thread.detachNewThread { (into.value, into.reset) = copy(upstream, guest); done.signal() }
        let (out, _) = copy(guest, upstream)
        done.wait()
        return (out, into.value, into.reset)
    }
}

/// Serves each guest connection on a thread of its own, at most `limit` at once; past that a connection is closed.
final class EgressGate: Sendable {
    let proxy: EgressProxy
    let limit: Int
    private let active = Mutex(0)

    init(proxy: EgressProxy, limit: Int = 256) {
        self.proxy = proxy
        self.limit = limit
    }

    @Sendable func accept(_ fd: Int32) {
        let admitted = active.withLock { n in
            if n >= limit { return false }
            n += 1
            return true
        }
        guard admitted else {
            close(fd)
            proxy.log(EgressRecord(vm: proxy.vm, decision: .deny, host: nil, port: nil, reason: "more than \(limit) connections"))
            return
        }
        Thread.detachNewThread { [self] in
            proxy.serve(fd)
            active.withLock { $0 -= 1 }
        }
    }
}

/// The proxy's real network: name resolution, the Mac's addresses and outbound connections.
enum EgressNet {
    static func isNumeric(_ host: String) -> Bool {
        var v4 = in_addr(), v6 = in6_addr()
        return inet_pton(AF_INET, host, &v4) == 1 || inet_pton(AF_INET6, host, &v6) == 1
    }

    /// Why no address answered, given the last connect's `errno`. macOS Local Network privacy blocks a launchd agent's
    /// connections to the Mac's own Wi-Fi or Ethernet networks (not VPN ones) with "no route to host" until the user
    /// allows it; a closed port or a timeout is just unreachable.
    static func unreachable(_ addresses: [String], local: [String], errno failure: Int32) -> String {
        guard failure == EHOSTUNREACH, addresses.contains(where: { !EgressPolicy.isReachable($0, local: local) }) else { return "unreachable" }
        return "unreachable; a host on the Mac's own LAN needs Local Network access for glovd (System Settings > Privacy & Security > Local Network)"
    }

    /// Numeric addresses of `host` (a name or a literal), IPv4 first.
    @Sendable static func resolve(_ host: String, _ port: Int) -> [String] {
        var hints = addrinfo()
        hints.ai_family = AF_UNSPEC
        hints.ai_socktype = SOCK_STREAM
        var list: UnsafeMutablePointer<addrinfo>?
        guard getaddrinfo(host, String(port), &hints, &list) == 0, let list else { return [] }
        defer { freeaddrinfo(list) }
        var v4: [String] = [], v6: [String] = []
        for info in sequence(first: list, next: { $0.pointee.ai_next }) {
            guard let name = numeric(info.pointee.ai_addr, info.pointee.ai_addrlen) else { continue }
            if info.pointee.ai_family == AF_INET { v4.append(name) } else if info.pointee.ai_family == AF_INET6 { v6.append(name) }
        }
        var seen = Set<String>()
        return (v4 + v6).filter { seen.insert($0).inserted }
    }

    /// Every address of the Mac's interfaces, IPv6 without its zone.
    @Sendable static func localAddresses() -> [String] {
        var list: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&list) == 0, let list else { return [] }
        defer { freeifaddrs(list) }
        return sequence(first: list, next: { $0.pointee.ifa_next }).compactMap { entry in
            guard let address = entry.pointee.ifa_addr else { return nil }
            let family = Int32(address.pointee.sa_family)
            guard family == AF_INET || family == AF_INET6 else { return nil }
            let length = socklen_t(family == AF_INET ? MemoryLayout<sockaddr_in>.size : MemoryLayout<sockaddr_in6>.size)
            return numeric(address, length)
        }
    }

    static func numeric(_ address: UnsafePointer<sockaddr>?, _ length: socklen_t) -> String? {
        guard let address else { return nil }
        var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
        guard getnameinfo(address, length, &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 else { return nil }
        let name = String(decoding: host.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
        return name.split(separator: "%").first.map(String.init)
    }

    /// A blocking TCP connection to a numeric address, within 5 seconds.
    @Sendable static func connect(_ address: String, _ port: Int) -> Int32? {
        var hints = addrinfo()
        hints.ai_flags = AI_NUMERICHOST
        hints.ai_socktype = SOCK_STREAM
        var list: UnsafeMutablePointer<addrinfo>?
        guard getaddrinfo(address, String(port), &hints, &list) == 0, let info = list else { return nil }
        defer { freeaddrinfo(list) }
        let fd = socket(info.pointee.ai_family, SOCK_STREAM, IPPROTO_TCP)
        guard fd >= 0 else { return nil }
        /// Closes `fd` keeping `failure` in errno, which callers read to tell why the connect failed.
        func fail(_ failure: Int32) -> Int32? { close(fd); errno = failure; return nil }
        let flags = fcntl(fd, F_GETFL)
        _ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)
        if Darwin.connect(fd, info.pointee.ai_addr, info.pointee.ai_addrlen) != 0 {
            guard errno == EINPROGRESS else { return fail(errno) }
            var p = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0)
            var error: Int32 = 0
            var size = socklen_t(MemoryLayout<Int32>.size)
            guard poll(&p, 1, 5000) == 1 else { return fail(ETIMEDOUT) }
            guard getsockopt(fd, SOL_SOCKET, SO_ERROR, &error, &size) == 0 else { return fail(errno) }
            guard error == 0 else { return fail(error) }
        }
        _ = fcntl(fd, F_SETFL, flags & ~O_NONBLOCK)
        return fd
    }
}
