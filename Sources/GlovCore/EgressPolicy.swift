import Foundation

/// The egress allowlist of an isolated VM (`VMConfig.Egress.allow`, docs/guest-protocol.md "Isolated VM egress proxy").
/// An entry is a hostname, `*.suffix` or an IP literal, optionally with `:port` (`[v6]:port` for IPv6); without a port
/// it allows 443 and 80 only. Nothing else is allowed.
public struct EgressPolicy: Sendable, Equatable {
    public static let defaultPorts = [443, 80]

    struct Entry: Equatable {
        enum Host: Equatable {
            case name(String)
            case suffix(String)
            case address([UInt8])
        }
        var host: Host
        var ports: [Int]
    }

    let entries: [Entry]

    public init(allow: [String]) {
        entries = allow.compactMap(Self.entry)
    }

    /// Whether the guest may connect to `host` (a name or an IP literal, as the request named it) on `port`.
    public func allows(host: String, port: Int) -> Bool {
        guard (1...65535).contains(port) else { return false }
        let target: Entry.Host
        if let address = Self.address(host) {
            target = .address(address)
        } else {
            let name = Self.name(host)
            guard !name.isEmpty else { return false }
            target = .name(name)
        }
        return entries.contains { entry in
            guard entry.ports.contains(port) else { return false }
            switch (entry.host, target) {
            case (.name(let a), .name(let b)): return a == b
            case (.suffix(let s), .name(let b)): return b.hasSuffix("." + s)
            case (.address(let a), .address(let b)): return a == b
            default: return false
            }
        }
    }

    /// Whether the proxy may connect to `address` (numeric): a public unicast address that is none of the Mac's own
    /// (`local`, numeric too). Loopback, private, CGNAT, link-local, multicast, unspecified and reserved ranges are not.
    /// `privateNetworks` (a `--private` entry) also admits RFC 1918, CGNAT and unique local addresses; the rest stay out.
    public static func isReachable(_ address: String, local: [String], privateNetworks: Bool = false) -> Bool {
        // An IPv4-mapped address is the IPv4 address it carries: compared as such with the Mac's own.
        guard let bytes = Self.address(address).map(unmapped) else { return false }
        if local.compactMap(Self.address).map(unmapped).contains(bytes) { return false }
        if bytes.count == 4 { return isPublicV4(bytes) || (privateNetworks && isPrivateV4(bytes)) }
        let b = bytes
        if b[0] == 0xff { return false }  // multicast
        if b[0] & 0xfe == 0xfc { return privateNetworks }  // fc00::/7 unique local
        if b[0] == 0xfe && b[1] & 0xc0 == 0x80 { return false }  // fe80::/10 link-local
        if b[0..<12].allSatisfy({ $0 == 0 }) { return false }  // ::, ::1 and IPv4-compatible
        // NAT64 and 6to4 never carry private IPv4 (RFC 6052, RFC 3056), not even for a --private entry.
        if b[0..<12] == [0x00, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0] { return isPublicV4(Array(b[12...])) }  // NAT64
        if b[0] & 0xe0 != 0x20 { return false }  // outside 2000::/3 global unicast: 64:ff9b:1::/48 local NAT64, fec0::/10, 100::/64, unassigned
        if b[0] == 0x20 && b[1] == 0x02 { return isPublicV4(Array(b[2..<6])) }  // 6to4
        if b[0..<4] == [0x20, 0x01, 0x0d, 0xb8] || b[0..<4] == [0x20, 0x01, 0, 0] { return false }  // documentation, Teredo
        if b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && (0x10...0x2f).contains(b[3]) { return false }  // 2001:10::/28 and 2001:20::/28 ORCHID
        if b[0] == 0x3f && b[1] == 0xff && b[2] & 0xf0 == 0 { return false }  // 3fff::/20 documentation
        return true
    }

    /// The IPv4 address an IPv4-mapped one (`::ffff:a.b.c.d`) carries; any other address unchanged.
    static func unmapped(_ b: [UInt8]) -> [UInt8] {
        b.count == 16 && b[0..<10].allSatisfy({ $0 == 0 }) && b[10] == 0xff && b[11] == 0xff ? Array(b[12...]) : b
    }

    /// RFC 1918 and CGNAT (100.64.0.0/10, which tailnets use).
    static func isPrivateV4(_ b: [UInt8]) -> Bool {
        switch (b[0], b[1]) {
        case (10, _), (172, 16...31), (192, 168), (100, 64...127): true
        default: false
        }
    }

    static func isPublicV4(_ b: [UInt8]) -> Bool {
        switch (b[0], b[1], b[2]) {
        case (0, _, _), (10, _, _), (127, _, _): false
        case (100, 64...127, _): false
        case (169, 254, _): false
        case (172, 16...31, _): false
        case (192, 0, 0), (192, 0, 2), (192, 88, 99), (192, 168, _): false
        case (198, 18...19, _), (198, 51, 100), (203, 0, 113): false
        case (224..., _, _): false  // multicast, reserved and broadcast
        default: true
        }
    }

    /// The bytes of a numeric IPv4 or IPv6 address (brackets allowed), or nil for anything else.
    static func address(_ text: String) -> [UInt8]? {
        var s = text
        if s.hasPrefix("[") && s.hasSuffix("]") { s = String(s.dropFirst().dropLast()) }
        var v4 = in_addr()
        if inet_pton(AF_INET, s, &v4) == 1 { return withUnsafeBytes(of: &v4) { Array($0) } }
        var v6 = in6_addr()
        if inet_pton(AF_INET6, s, &v6) == 1 { return withUnsafeBytes(of: &v6) { Array($0) } }
        return nil
    }

    static func name(_ host: String) -> String {
        var name = host.lowercased()
        if name.hasSuffix(".") { name.removeLast() }
        return name
    }

    static func entry(_ text: String) -> Entry? {
        var host = text.trimmingCharacters(in: .whitespaces)
        var ports = defaultPorts
        if host.hasPrefix("["), let close = host.firstIndex(of: "]") {
            let rest = host[host.index(after: close)...]
            if rest.hasPrefix(":") {
                guard let port = Int(rest.dropFirst()), (1...65535).contains(port) else { return nil }
                ports = [port]
            } else if !rest.isEmpty { return nil }
            host = String(host[host.index(after: host.startIndex)..<close])
        } else if host.filter({ $0 == ":" }).count == 1, let colon = host.lastIndex(of: ":") {
            guard let port = Int(host[host.index(after: colon)...]), (1...65535).contains(port) else { return nil }
            ports = [port]
            host = String(host[..<colon])
        }
        if let address = address(host) { return Entry(host: .address(address), ports: ports) }
        let name = name(host)
        if name.hasPrefix("*.") {
            let suffix = String(name.dropFirst(2))
            return isName(suffix) ? Entry(host: .suffix(suffix), ports: ports) : nil
        }
        return isName(name) ? Entry(host: .name(name), ports: ports) : nil
    }

    /// A lowercased DNS name: dot-separated, non-empty labels of letters, digits, `-` and `_`.
    static func isName(_ name: String) -> Bool {
        let allowed = Set("abcdefghijklmnopqrstuvwxyz0123456789-_")
        return !name.isEmpty && name.count <= 253
            && name.split(separator: ".", omittingEmptySubsequences: false).allSatisfy { !$0.isEmpty && $0.allSatisfy(allowed.contains) }
    }

    /// `entry` as vm.json stores it (lowercase, no trailing dot, compressed IPv6, a port only when it is not the default),
    /// or nil when it is no valid entry.
    public static func canonical(_ entry: String) -> String? {
        guard let e = Self.entry(entry) else { return nil }
        let port = e.ports == defaultPorts ? nil : e.ports.first
        switch e.host {
        case .name(let n): return n + (port.map { ":\($0)" } ?? "")
        case .suffix(let s): return "*." + s + (port.map { ":\($0)" } ?? "")
        case .address(let bytes):
            var text = [CChar](repeating: 0, count: Int(INET6_ADDRSTRLEN))
            let family = bytes.count == 4 ? AF_INET : AF_INET6
            guard inet_ntop(family, bytes, &text, socklen_t(text.count)) != nil else { return nil }
            let a = String(decoding: text.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
            guard let port else { return a }
            return family == AF_INET6 ? "[\(a)]:\(port)" : "\(a):\(port)"
        }
    }
}

public enum EgressError: Error, Equatable, CustomStringConvertible {
    case invalidEntries([String])
    case notListed([String])
    case privateSuffix([String])

    public var description: String {
        switch self {
        case .invalidEntries(let e):
            "not a valid egress entry: \(e.map { String(reflecting: $0) }.joined(separator: ", ")) (use a hostname, *.suffix or an IP address, optionally with :port)"
        case .notListed(let e): "not in the allowlist: \(e.joined(separator: ", "))"
        case .privateSuffix(let e): "--private takes an exact hostname or IP address, not \(e.joined(separator: ", "))"
        }
    }
}

extension VMConfig.Egress {
    /// This allowlist with `entries` added in canonical form, each once; fails on any invalid entry, changing nothing.
    /// `privateNetworks` also lets these entries resolve to private addresses (`privateAllow`); an entry already listed
    /// keeps its own setting unless `privateNetworks` adds it.
    public func allowing(_ entries: [String], privateNetworks: Bool = false) throws(EgressError) -> Self {
        let canonical = entries.map { (entry: $0, form: EgressPolicy.canonical($0)) }
        let invalid = canonical.filter { $0.form == nil }.map(\.entry)
        guard invalid.isEmpty else { throw .invalidEntries(invalid) }
        let forms = canonical.compactMap(\.form)
        if privateNetworks {
            let suffixes = forms.filter { $0.hasPrefix("*.") }
            guard suffixes.isEmpty else { throw .privateSuffix(suffixes) }
        }
        var result = self
        for form in forms where !result.allow.contains(form) { result.allow.append(form) }
        if privateNetworks {
            var marked = result.privateAllow ?? []
            for form in forms where !marked.contains(form) { marked.append(form) }
            result.privateAllow = marked
        }
        return result
    }

    /// This allowlist without `entries` (compared in canonical form); fails, changing nothing, when one is not listed.
    public func removing(_ entries: [String]) throws(EgressError) -> Self {
        func form(_ entry: String) -> String { EgressPolicy.canonical(entry) ?? entry }
        let listed = allow.map(form)
        let missing = entries.filter { !listed.contains(form($0)) }
        guard missing.isEmpty else { throw .notListed(missing) }
        let forms = Set(entries.map(form))
        var result = self
        result.allow = zip(allow, listed).filter { !forms.contains($0.1) }.map(\.0)
        result.privateAllow = privateAllow?.filter { !forms.contains(form($0)) }
        if result.privateAllow?.isEmpty == true { result.privateAllow = nil }
        return result
    }
}

extension VMConfig.Egress {
    /// Whether the guest may connect to `host` on `port` at all: any host when open, else an `allow` entry's.
    public func permits(host: String, port: Int) -> Bool {
        if open == true { return (1...65535).contains(port) && (EgressPolicy.address(host) != nil || !EgressPolicy.name(host).isEmpty) }
        return EgressPolicy(allow: allow).allows(host: host, port: port)
    }

    /// Whether the entry that lets the guest reach `host` on `port` was allowed with `--private`.
    public func reachesPrivateNetworks(host: String, port: Int) -> Bool {
        guard let privateAllow, !privateAllow.isEmpty else { return false }
        return EgressPolicy(allow: privateAllow).allows(host: host, port: port)
    }
}
