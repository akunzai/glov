import Foundation
import Synchronization
import Testing
import GlovCore
@testable import GlovDaemon

@Suite struct EgressProxyTests {
    /// A proxy whose upstream is the far end of a socket pair, and what it did.
    final class Harness: Sendable {
        let allow = Mutex<[String]>(["api.example.com", "*.ubuntu.com"])
        let privateAllow = Mutex<[String]?>(nil)
        let openMode = Mutex<Bool?>(nil)
        let addresses = Mutex<[String]>(["93.184.215.14"])
        let lines = Mutex<[String]>([])
        let resolved = Mutex<[String]>([])
        let connected = Mutex<[String]>([])
        let upstream = Mutex<Int32?>(nil)
        /// Upstream over TCP loopback instead of a socket pair, for a test that resets it.
        let tcp = Mutex<Bool>(false)

        var proxy: EgressProxy {
            EgressProxy(
                vm: "box", egress: { .init(allow: self.allow.withLock { $0 }, privateAllow: self.privateAllow.withLock { $0 }, open: self.openMode.withLock { $0 }) },
                resolve: { host, _ in self.resolved.withLock { $0.append(host) }; return self.addresses.withLock { $0 } },
                localAddresses: { ["192.0.2.10"] },
                connect: { address, port in
                    self.connected.withLock { $0.append("\(address):\(port)") }
                    var pair: [Int32] = [0, 0]
                    if self.tcp.withLock({ $0 }) { pair = EgressProxyTests.tcpPair() } else {
                        guard socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0 else { return nil }
                    }
                    self.upstream.withLock { $0 = pair[1] }
                    return pair[0]
                },
                log: { record in self.lines.withLock { $0.append(record.line) } })
        }

        /// Serves one guest connection on a thread; returns the guest's end.
        func open() -> (guest: Int32, done: DispatchSemaphore) {
            var pair: [Int32] = [0, 0]
            precondition(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
            let done = DispatchSemaphore(value: 0)
            let proxy = self.proxy, fd = pair[1]
            Thread.detachNewThread { proxy.serve(fd); done.signal() }
            return (pair[0], done)
        }

        func waitForUpstream() -> Int32 {
            for _ in 0..<200 {
                if let fd = upstream.withLock({ $0 }) { return fd }
                usleep(10_000)
            }
            Issue.record("the proxy never connected upstream")
            return -1
        }
    }

    static func send(_ fd: Int32, _ text: String) {
        let data = Array(text.utf8)
        #expect(write(fd, data, data.count) == data.count)
    }

    /// Reads until `count` bytes arrived or the peer closed.
    static func receive(_ fd: Int32, atLeast count: Int = .max) -> String {
        var out: [UInt8] = []
        var buffer = [UInt8](repeating: 0, count: 4096)
        while out.count < count {
            let n = buffer.withUnsafeMutableBytes { UnixSocket.readWhenReadable(fd, $0) }
            if n <= 0 { break }
            out += buffer.prefix(n)
        }
        return String(decoding: out, as: UTF8.self)
    }

    @Test func anAllowedConnectTunnelsBytesAndLogsTheirCount() {
        let h = Harness()
        let (guest, done) = h.open()
        Self.send(guest, "CONNECT API.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nUser-Agent: curl\r\n\r\n")
        #expect(Self.receive(guest, atLeast: 39) == "HTTP/1.1 200 Connection Established\r\n\r\n")
        let up = h.waitForUpstream()
        Self.send(guest, "hello")
        #expect(Self.receive(up, atLeast: 5) == "hello")
        Self.send(up, "world!")
        #expect(Self.receive(guest, atLeast: 6) == "world!")
        close(up)
        #expect(Self.receive(guest) == "")
        close(guest)
        done.wait()
        #expect(h.connected.withLock { $0 } == ["93.184.215.14:443"])
        #expect(h.lines.withLock { $0 } == ["egress allow api.example.com:443 in=6 out=5"])
    }

    @Test func aDeniedHostGetsA403AndNothingGoesUpstreamNotEvenDNS() {
        let h = Harness()
        for request in ["CONNECT evil.example.net:443 HTTP/1.1\r\n\r\n", "CONNECT api.example.com:22 HTTP/1.1\r\n\r\n",
                        "CONNECT 10.0.0.1:443 HTTP/1.1\r\n\r\n"] {
            let (guest, done) = h.open()
            Self.send(guest, request)
            #expect(Self.receive(guest).hasPrefix("HTTP/1.1 403 Forbidden\r\n"))
            close(guest)
            done.wait()
        }
        #expect(h.resolved.withLock { $0 }.isEmpty)
        #expect(h.connected.withLock { $0 }.isEmpty)
        #expect(h.lines.withLock { $0 } == [
            "egress deny evil.example.net:443 in=0 out=0 (not in the allowlist)",
            "egress deny api.example.com:22 in=0 out=0 (not in the allowlist)",
            "egress deny 10.0.0.1:443 in=0 out=0 (not in the allowlist)",
        ])
    }

    @Test func anAllowedNameThatResolvesToAPrivateOrMacAddressIsRefused() {
        let h = Harness()
        for addresses in [["10.0.0.5"], ["192.0.2.10"], ["169.254.169.254", "fe80::1"]] {
            h.addresses.withLock { $0 = addresses }
            let (guest, done) = h.open()
            Self.send(guest, "CONNECT archive.ubuntu.com:443 HTTP/1.1\r\n\r\n")
            #expect(Self.receive(guest).hasPrefix("HTTP/1.1 403 Forbidden\r\n"))
            close(guest)
            done.wait()
        }
        #expect(h.connected.withLock { $0 }.isEmpty)
        #expect(h.lines.withLock { $0 }.allSatisfy { $0 == "egress deny archive.ubuntu.com:443 in=0 out=0 (no public address)" })
    }

    @Test func aPrivateEntryReachesAPrivateAddressButNeverTheMacOrLinkLocal() {
        let h = Harness()
        h.allow.withLock { $0 = ["gitlab.corp.example", "api.example.com"] }
        h.privateAllow.withLock { $0 = ["gitlab.corp.example"] }
        h.addresses.withLock { $0 = ["169.254.1.1", "192.0.2.10", "10.0.0.5"] }
        let (guest, done) = h.open()
        Self.send(guest, "CONNECT gitlab.corp.example:443 HTTP/1.1\r\n\r\n")
        #expect(Self.receive(guest, atLeast: 39) == "HTTP/1.1 200 Connection Established\r\n\r\n")
        close(h.waitForUpstream())
        close(guest)
        done.wait()
        #expect(h.connected.withLock { $0 } == ["10.0.0.5:443"])

        for (host, addresses, reason) in [
            ("gitlab.corp.example", ["127.0.0.1", "192.0.2.10", "fe80::1"], "no public or private-network address"),
            ("api.example.com", ["10.0.0.5"], "no public address"),
        ] {
            h.addresses.withLock { $0 = addresses }
            h.lines.withLock { $0 = [] }
            let (guest, done) = h.open()
            Self.send(guest, "CONNECT \(host):443 HTTP/1.1\r\n\r\n")
            #expect(Self.receive(guest).hasPrefix("HTTP/1.1 403 Forbidden\r\n"))
            close(guest)
            done.wait()
            #expect(h.lines.withLock { $0 } == ["egress deny \(host):443 in=0 out=0 (\(reason))"])
        }
        #expect(h.connected.withLock { $0 } == ["10.0.0.5:443"])
    }

    @Test func anOpenVMTunnelsToAnUnlistedPublicHostAndStillRefusesPrivateAndPlaceholders() {
        let h = Harness()
        h.openMode.withLock { $0 = true }
        let (guest, done) = h.open()
        Self.send(guest, "CONNECT unlisted.example.org:443 HTTP/1.1\r\n\r\n")
        #expect(Self.receive(guest, atLeast: 39) == "HTTP/1.1 200 Connection Established\r\n\r\n")
        close(h.waitForUpstream())
        close(guest)
        done.wait()
        #expect(h.connected.withLock { $0 } == ["93.184.215.14:443"])

        h.lines.withLock { $0 = [] }
        h.addresses.withLock { $0 = ["10.0.0.5"] }
        for request in [
            "CONNECT intranet.example.org:443 HTTP/1.1\r\n\r\n",
            "GET http://unlisted.example.org/ HTTP/1.1\r\nHost: unlisted.example.org\r\nAuthorization: Bearer glov-secret:key\r\n\r\n",
        ] {
            let (guest, done) = h.open()
            Self.send(guest, request)
            #expect(Self.receive(guest).hasPrefix("HTTP/1.1 403 Forbidden\r\n"))
            close(guest)
            done.wait()
        }
        #expect(h.lines.withLock { $0 } == [
            "egress deny intranet.example.org:443 in=0 out=0 (no public address)",
            "egress deny unlisted.example.org:80 in=0 out=0 (a secret placeholder goes only over HTTPS to a host the secret is bound to)",
        ])
        #expect(h.connected.withLock { $0 } == ["93.184.215.14:443"])
    }

    @Test func anUnreachablePrivateAddressPointsAtLocalNetworkAccess() {
        #expect(EgressNet.unreachable(["93.184.215.14"], local: [], errno: EHOSTUNREACH) == "unreachable")
        #expect(EgressNet.unreachable(["10.0.0.5"], local: [], errno: EHOSTUNREACH).hasPrefix("unreachable; a host on the Mac's own LAN needs Local Network access for glovd"))
        // A closed port or a timeout on a private address is no permission problem.
        #expect(EgressNet.unreachable(["10.0.0.5"], local: [], errno: ECONNREFUSED) == "unreachable")
        #expect(EgressNet.unreachable(["10.0.0.5"], local: [], errno: ETIMEDOUT) == "unreachable")
    }

    @Test func aPlainHTTPRequestGoesUpstreamInOriginFormWithoutProxyHeaders() {
        let h = Harness()
        let (guest, done) = h.open()
        Self.send(guest, "GET http://archive.ubuntu.com/ubuntu/dists/x?y=1 HTTP/1.1\r\nHost: archive.ubuntu.com\r\n"
            + "Proxy-Connection: keep-alive\r\nProxy-Authorization: Basic c2VjcmV0\r\nConnection: keep-alive\r\nUser-Agent: apt\r\n\r\nBODY")
        let up = h.waitForUpstream()
        let expected = "GET /ubuntu/dists/x?y=1 HTTP/1.1\r\nHost: archive.ubuntu.com\r\nUser-Agent: apt\r\nConnection: close\r\n\r\nBODY"
        #expect(Self.receive(up, atLeast: expected.utf8.count) == expected)
        Self.send(up, "HTTP/1.1 200 OK\r\n\r\n")
        close(up)
        #expect(Self.receive(guest) == "HTTP/1.1 200 OK\r\n\r\n")
        close(guest)
        done.wait()
        #expect(h.connected.withLock { $0 } == ["93.184.215.14:80"])
        #expect(h.lines.withLock { $0 } == ["egress allow archive.ubuntu.com:80 in=19 out=\(expected.utf8.count)"])
    }

    /// The host reached resets the connection, which is upstream's doing and not the proxy's.
    @Test func anUpstreamResetIsNamedInTheLog() {
        let h = Harness()
        h.tcp.withLock { $0 = true }
        let (guest, done) = h.open()
        Self.send(guest, "GET http://archive.ubuntu.com/ubuntu/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n\r\n")
        let up = h.waitForUpstream()
        let expected = "GET /ubuntu/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\nConnection: close\r\n\r\n"
        #expect(Self.receive(up, atLeast: expected.utf8.count) == expected)
        var reset = linger(l_onoff: 1, l_linger: 0)  // close sends RST
        setsockopt(up, SOL_SOCKET, SO_LINGER, &reset, socklen_t(MemoryLayout<linger>.size))
        close(up)
        #expect(Self.receive(guest) == "")
        close(guest)
        done.wait()
        #expect(h.lines.withLock { $0 } == ["egress allow archive.ubuntu.com:80 in=0 out=\(expected.utf8.count) (upstream reset)"])
    }

    /// A connected TCP loopback pair.
    static func tcpPair() -> [Int32] {
        let listener = socket(AF_INET, SOCK_STREAM, 0)
        defer { close(listener) }
        var address = sockaddr_in(sin_len: UInt8(MemoryLayout<sockaddr_in>.size), sin_family: sa_family_t(AF_INET), sin_port: 0,
            sin_addr: in_addr(s_addr: inet_addr("127.0.0.1")), sin_zero: (0, 0, 0, 0, 0, 0, 0, 0))
        var size = socklen_t(MemoryLayout<sockaddr_in>.size)
        withUnsafeMutablePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                precondition(bind(listener, $0, size) == 0 && listen(listener, 1) == 0 && getsockname(listener, $0, &size) == 0)
            }
        }
        let near = socket(AF_INET, SOCK_STREAM, 0)
        withUnsafePointer(to: &address) { $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { precondition(connect(near, $0, size) == 0) } }
        return [near, accept(listener, nil, nil)]
    }

    @Test func aPlainHTTPRequestWhoseHostHeaderNamesAnotherSiteIsRefused() {
        let h = Harness()
        for request in [
            "GET http://archive.ubuntu.com/ HTTP/1.1\r\nHost: evil.example.net\r\n\r\n",
            "GET http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\nHost: evil.example.net\r\n\r\n",
            "GET http://archive.ubuntu.com/ HTTP/1.1\r\nHost: archive.ubuntu.com\r\n Proxy-Authorization: x\r\n\r\n",
        ] {
            let (guest, done) = h.open()
            Self.send(guest, request)
            #expect(Self.receive(guest).hasPrefix("HTTP/1.1 400 Bad Request\r\n"))
            close(guest)
            done.wait()
        }
        #expect(h.connected.withLock { $0 }.isEmpty)
        let (guest, done) = h.open()
        Self.send(guest, "GET http://archive.ubuntu.com:80/ HTTP/1.1\r\nhost:  Archive.Ubuntu.com:80 \r\n\r\n")
        let up = h.waitForUpstream()
        close(up)
        _ = Self.receive(guest)
        close(guest)
        done.wait()
    }

    @Test func aMalformedRequestGetsA400WithoutEchoingIt() {
        let h = Harness()
        for request in ["HELLO\r\n\r\n", "GET relative HTTP/1.1\r\nHost: api.example.com\r\n\r\n", "CONNECT api.example.com HTTP/1.1\r\n\r\n",
                        "CONNECT bad\u{1b}host:443 HTTP/1.1\r\n\r\n",
                        // There is no in-guest model API endpoint: a model API key is a secret on the provider's own host.
                        "POST /anthropic/v1/messages HTTP/1.1\r\nHost: 127.0.0.1:3128\r\nContent-Length: 2\r\n\r\n{}"] {
            let (guest, done) = h.open()
            Self.send(guest, request)
            let reply = Self.receive(guest)
            #expect(reply.hasPrefix("HTTP/1.1 400 Bad Request\r\n"))
            #expect(!reply.contains("api.example.com"))
            close(guest)
            done.wait()
        }
        #expect(h.connected.withLock { $0 }.isEmpty)
        #expect(h.lines.withLock { $0 } == Array(repeating: "egress deny - in=0 out=0 (malformed request)", count: 5))
    }

    @Test func anAllowlistChangeAppliesToTheNextConnection() {
        let h = Harness()
        h.allow.withLock { $0 = [] }
        var (guest, done) = h.open()
        Self.send(guest, "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
        #expect(Self.receive(guest).hasPrefix("HTTP/1.1 403"))
        close(guest)
        done.wait()
        h.allow.withLock { $0 = ["api.example.com"] }
        (guest, done) = h.open()
        Self.send(guest, "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
        #expect(Self.receive(guest, atLeast: 39).hasPrefix("HTTP/1.1 200"))
        close(h.waitForUpstream())
        close(guest)
        done.wait()
    }

    @Test func anUnreachableAllowedHostGetsA502() {
        let h = Harness()
        let proxy = EgressProxy(
            vm: "box", egress: { .init(allow: ["api.example.com"]) }, resolve: { _, _ in ["93.184.215.14"] },
            localAddresses: { [] }, connect: { _, _ in nil }, log: { record in h.lines.withLock { $0.append(record.line) } })
        var pair: [Int32] = [0, 0]
        #expect(socketpair(AF_UNIX, SOCK_STREAM, 0, &pair) == 0)
        Self.send(pair[0], "CONNECT api.example.com:443 HTTP/1.1\r\n\r\n")
        proxy.serve(pair[1])
        #expect(Self.receive(pair[0]).hasPrefix("HTTP/1.1 502 Bad Gateway\r\n"))
        close(pair[0])
        #expect(h.lines.withLock { $0 } == ["egress fail api.example.com:443 in=0 out=0 (unreachable)"])
    }
}
