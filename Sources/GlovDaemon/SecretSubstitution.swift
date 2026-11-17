import Foundation
import GlovCore

/// Placeholder substitution in one decrypted request head (docs/networking.md "Secrets"): the guest writes
/// `glov-secret:<name>` where a credential goes, and the proxy puts the value there, in header values only, and only for a
/// secret granted to the VM and bound to the host the request goes to.
enum SecretSubstitution {
    static let prefix = "glov-secret:"

    struct Refusal: Error, Equatable {
        var status: Int
        var message: String
        /// For egress.log: names the secret, never its value.
        var reason: String
    }

    /// A request head checked against the VM's grants, waiting for the values of `secrets`.
    struct Plan: Equatable {
        var method: String
        /// The request target without its query, for the log.
        var path: String
        var requestLine: String
        var headers: [(name: String, value: String)]
        /// The secrets the head names, in order of first use.
        var secrets: [String]

        static func == (a: Plan, b: Plan) -> Bool {
            a.method == b.method && a.path == b.path && a.requestLine == b.requestLine && a.secrets == b.secrets
                && a.headers.elementsEqual(b.headers) { $0 == $1 }
        }
    }

    /// The placeholder names in `text`, in order, duplicates kept.
    static func names(in text: some StringProtocol) -> [String] {
        var found: [String] = []
        var rest = Substring(text)
        let allowed = Set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
        while let range = rest.range(of: prefix) {
            let name = rest[range.upperBound...].prefix { allowed.contains($0) }
            found.append(String(name))
            rest = rest[range.upperBound...]
        }
        return found
    }

    /// Whether `head` (an absolute-form plain-HTTP request) names a placeholder anywhere: such a request is refused,
    /// since a value is never sent unencrypted.
    static func mentionsPlaceholder(_ head: Data) -> Bool {
        let text = String(decoding: head, as: UTF8.self)
        if text.contains(prefix) { return true }
        return text.components(separatedBy: "\r\n").contains { line in
            basicCredentials(line).map { $0.contains(prefix) } ?? false
        }
    }

    /// The decoded `user:password` of an `Authorization: Basic` header line, nil for any other line.
    static func basicCredentials(_ line: String) -> String? {
        guard let colon = line.firstIndex(of: ":"), line[..<colon].lowercased() == "authorization" else { return nil }
        return basicCredentials(value: line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces))
    }

    private static func basicCredentials(value: String) -> String? {
        let parts = value.split(separator: " ", maxSplits: 1)
        guard parts.count == 2, parts[0].lowercased() == "basic", let data = Data(base64Encoded: String(parts[1]).trimmingCharacters(in: .whitespaces)) else {
            return nil
        }
        return String(data: data, encoding: .utf8)
    }

    /// Checks an origin-form request head bound for `host:port` against the VM's grants. `granted` are the names in
    /// vm.json, `definitions` all secrets on the Mac.
    static func plan(
        _ head: Data, host: String, port: Int, vm: String, granted: [String], definitions: [SecretDefinition]
    ) -> Result<Plan, Refusal> {
        func bad(_ message: String) -> Result<Plan, Refusal> { .failure(Refusal(status: 400, message: message, reason: "malformed request")) }
        guard let text = String(data: head, encoding: .utf8) else { return bad("the request head is not UTF-8") }
        var lines = text.components(separatedBy: "\r\n")
        while lines.last == "" { lines.removeLast() }
        guard let first = lines.first else { return bad("empty request") }
        let parts = first.split(separator: " ", omittingEmptySubsequences: false)
        guard parts.count == 3, ["HTTP/1.0", "HTTP/1.1"].contains(parts[2]), parts[1].hasPrefix("/") else {
            return bad("expected an origin-form HTTP/1.x request")
        }
        let method = String(parts[0])
        guard !method.isEmpty, method.allSatisfy({ $0.isLetter && $0.isASCII }) else { return bad("malformed method") }
        let token = Set("abcdefghijklmnopqrstuvwxyz0123456789!#$%&'*+-.^_`|~")
        var headers: [(name: String, value: String)] = []
        var hosts = 0
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":") else { return bad("malformed header") }
            let name = String(line[..<colon])
            let lower = name.lowercased()
            guard !name.isEmpty, lower.allSatisfy(token.contains) else { return bad("malformed header") }
            let value = line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)
            if lower.hasPrefix("proxy-") || ["connection", "keep-alive", "upgrade"].contains(lower) { continue }
            if lower == "host" {
                // The upstream serves the site the header names: on a shared front end, another name could reach a site
                // the secret is not bound to.
                hosts += 1
                guard let (named, namedPort) = EgressProxy.authority(value), named == host, (namedPort ?? 443) == port else {
                    return .failure(Refusal(status: 403, message: "the Host header must name \(host)", reason: "Host header mismatch"))
                }
            }
            headers.append((name, value))
        }
        guard hosts == 1 else { return bad("send exactly one Host header") }

        var secrets: [String] = []
        for (_, value) in headers {
            for name in names(in: value) + names(in: basicCredentials(value: value) ?? "") where !secrets.contains(name) { secrets.append(name) }
        }
        if let refusal = check(secrets, host: host, port: port, vm: vm, granted: granted, definitions: definitions) { return .failure(refusal) }
        headers.append(("Connection", "close"))
        return .success(Plan(method: method, path: String(parts[1].prefix { $0 != "?" }), requestLine: first, headers: headers, secrets: secrets))
    }

    /// Why the placeholders `names` may not go to `host:port`, or nil when each is defined, granted and bound to it.
    static func check(_ names: [String], host: String, port: Int, vm: String, granted: [String], definitions: [SecretDefinition]) -> Refusal? {
        for name in names {
            guard let definition = definitions.first(where: { $0.name == name }) else {
                return Refusal(
                    status: 403, message: "no secret named \(name): define it on the Mac with glov secret set or glov secret source, then glov secret grant \(vm) \(name)",
                    reason: "secret \(name) undefined")
            }
            guard granted.contains(name) else {
                return Refusal(status: 403, message: "VM \(vm) was not granted secret \(name): run glov secret grant \(vm) \(name) on the Mac", reason: "secret \(name) not granted")
            }
            guard EgressPolicy(allow: definition.hosts).allows(host: host, port: port) else {
                return Refusal(
                    status: 403, message: "secret \(name) is not bound to \(host): it may go to \(definition.hosts.joined(separator: ", ")) only",
                    reason: "secret \(name) not bound to host")
            }
        }
        return nil
    }

    /// The head to send upstream, with every placeholder of `plan.secrets` replaced by its value.
    static func apply(_ plan: Plan, values: [String: String]) -> Result<Data, Refusal> {
        for name in plan.secrets {
            guard let value = values[name] else { return .failure(Refusal(status: 502, message: "secret \(name) has no value", reason: "secret \(name) unavailable")) }
            // Scalars, not Characters: "\r\n" is one Character, equal to neither.
            guard !value.unicodeScalars.contains(where: { $0 == "\r" || $0 == "\n" || $0 == "\0" }) else {
                return .failure(Refusal(status: 502, message: "secret \(name): the value contains a line break or NUL", reason: "secret \(name) unusable"))
            }
        }
        func replace(_ text: String) -> String {
            var text = text
            // Longest names first, so `a` never rewrites part of `glov-secret:ab`.
            for name in plan.secrets.sorted(by: { $0.count > $1.count }) {
                text = replacingWhole(prefix + name, with: values[name]!, in: text)
            }
            return text
        }
        var lines = [plan.requestLine]
        for (name, value) in plan.headers {
            var value = value
            if let credentials = basicCredentials(value: value), credentials.contains(prefix) {
                value = "Basic " + Data(replace(credentials).utf8).base64EncodedString()
            } else if value.contains(prefix) {
                value = replace(value)
            }
            lines.append("\(name): \(value)")
        }
        return .success(Data((lines.joined(separator: "\r\n") + "\r\n\r\n").utf8))
    }

    /// `text` with each `placeholder` not followed by another name character replaced.
    private static func replacingWhole(_ placeholder: String, with value: String, in text: String) -> String {
        let allowed = Set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
        var out = ""
        var rest = Substring(text)
        while let range = rest.range(of: placeholder) {
            out += rest[..<range.lowerBound]
            if let next = rest[range.upperBound...].first, allowed.contains(next) { out += rest[range] } else { out += value }
            rest = rest[range.upperBound...]
        }
        return out + rest
    }
}
