import Foundation
import GlovCore

/// The token endpoints of a VM's subscriptions (`HarnessProfile.Subscription.tokenEndpoints`): the guest signs in and
/// refreshes there itself, the proxy puts the real values into the request body, and the tokens of the answer stay on
/// the Mac, the guest getting placeholders for them. docs/networking.md "Subscriptions".
enum TokenExchange {
    /// Largest request body and answer taken whole; token requests and answers are a few KiB.
    static let maxBody = 64 << 10
    static let maxAnswer = 1 << 20

    struct Endpoint: Equatable, Sendable {
        var host: String
        var path: String
        /// Answer field to the secret that keeps it, and the hosts that secret is bound to.
        var answer: [String: Kept]
        struct Kept: Equatable, Sendable {
            var name: String
            var hosts: [String]
        }
    }

    /// The endpoints of `vm`'s subscribed harnesses.
    static func endpoints(vm: String, subscriptions: [String], profiles: [HarnessProfile] = HarnessProfile.all) -> [Endpoint] {
        profiles.filter { subscriptions.contains($0.name) }.flatMap { p -> [Endpoint] in
            guard let sub = p.subscription else { return [] }
            return sub.tokenEndpoints.map { e in
                var answer: [String: Endpoint.Kept] = [:]
                for (field, secret) in e.answer ?? [:] {
                    answer[field] = .init(name: p.secretName(vm: vm, secret), hosts: sub.secrets.first { $0.name == secret }?.hosts ?? [])
                }
                return Endpoint(host: e.host, path: e.path, answer: answer)
            }
        }
    }

    enum Format { case form, json }

    static func format(_ contentType: String?) -> Format? {
        let type = (contentType ?? "").split(separator: ";").first.map { $0.trimmingCharacters(in: .whitespaces).lowercased() } ?? ""
        if type == "application/x-www-form-urlencoded" { return .form }
        if type == "application/json" || type.hasSuffix("+json") { return .json }
        return nil
    }

    /// The placeholder names in a request body: whole values only, after decoding.
    static func names(in body: Data, _ format: Format) -> [String] {
        values(body, format).compactMap(name)
    }

    /// The secret a whole value is the placeholder of.
    private static func name(_ value: String) -> String? {
        guard value.hasPrefix(SecretSubstitution.prefix) else { return nil }
        let name = String(value.dropFirst(SecretSubstitution.prefix.count))
        return (try? SecretName.validate(name)) != nil ? name : nil
    }

    /// `body` with each value that is exactly a placeholder of `values` replaced. Nil when the body does not parse.
    static func substitute(_ body: Data, _ format: Format, values: [String: String]) -> Data? {
        func swap(_ s: String) -> String? { name(s).flatMap { values[$0] } }
        switch format {
        case .form:
            guard let text = String(data: body, encoding: .utf8) else { return nil }
            let pairs = text.split(separator: "&", omittingEmptySubsequences: false).map { pair -> String in
                let parts = pair.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
                guard parts.count == 2, let value = formDecode(parts[1]), let real = swap(value) else { return String(pair) }
                return "\(parts[0])=\(formEncode(real))"
            }
            return Data(pairs.joined(separator: "&").utf8)
        case .json:
            guard let object = try? JSONSerialization.jsonObject(with: body) else { return nil }
            func walk(_ v: Any) -> Any {
                if let s = v as? String { return swap(s) ?? s }
                if let d = v as? [String: Any] { return d.mapValues(walk) }
                if let a = v as? [Any] { return a.map(walk) }
                return v
            }
            return try? JSONSerialization.data(withJSONObject: walk(object), options: [.withoutEscapingSlashes])
        }
    }

    private static func values(_ body: Data, _ format: Format) -> [String] {
        switch format {
        case .form:
            guard let text = String(data: body, encoding: .utf8) else { return [] }
            return text.split(separator: "&").compactMap { pair in
                let parts = pair.split(separator: "=", maxSplits: 1)
                return parts.count == 2 ? formDecode(parts[1]) : nil
            }
        case .json:
            guard let object = try? JSONSerialization.jsonObject(with: body) else { return [] }
            var out: [String] = []
            func walk(_ v: Any) {
                if let s = v as? String { out.append(s) } else if let d = v as? [String: Any] { d.values.forEach(walk) } else if let a = v as? [Any] { a.forEach(walk) }
            }
            walk(object)
            return out
        }
    }

    static func formDecode(_ s: Substring) -> String? { s.replacingOccurrences(of: "+", with: " ").removingPercentEncoding }

    static func formEncode(_ s: String) -> String {
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        return s.addingPercentEncoding(withAllowedCharacters: allowed) ?? s
    }

    /// `body` with each occurrence of a value of `sent` replaced by its placeholder, longest values first.
    static func masking(_ sent: [String: String], in body: Data) -> Data {
        var body = body
        for (name, value) in sent.sorted(by: { $0.value.count > $1.value.count }) where !value.isEmpty {
            let needle = Data(value.utf8), placeholder = Data((SecretSubstitution.prefix + name).utf8)
            while let range = body.range(of: needle) { body.replaceSubrange(range, with: placeholder) }
        }
        return body
    }

    /// The tokens of a JSON answer that `answer` keeps, and the answer with placeholders in their place. Nil when the
    /// answer is not a JSON object.
    static func keep(_ body: Data, answer: [String: Endpoint.Kept]) -> (body: Data, kept: [(Endpoint.Kept, String)])? {
        guard var object = (try? JSONSerialization.jsonObject(with: body)) as? [String: Any] else { return nil }
        var kept: [(Endpoint.Kept, String)] = []
        for (field, secret) in answer.sorted(by: { $0.key < $1.key }) {
            guard let value = object[field] as? String, !value.isEmpty else { continue }
            kept.append((secret, value))
            object[field] = SecretSubstitution.prefix + secret.name
        }
        guard let data = try? JSONSerialization.data(withJSONObject: object, options: [.withoutEscapingSlashes]) else { return nil }
        return (data, kept)
    }

    /// A whole HTTP/1.x response as read until the server closed: status line, headers and the decoded body.
    struct Response {
        var status: Int
        var statusLine: String
        var headers: [(name: String, value: String)]
        var body: Data

        /// The response as one message to the guest: the body is decoded, so its framing headers are its own again.
        var framed: Data {
            let drop: Set<String> = ["content-length", "transfer-encoding", "content-encoding", "connection"]
            var lines = [statusLine] + headers.filter { !drop.contains($0.name.lowercased()) }.map { "\($0.name): \($0.value)" }
            lines += ["Content-Length: \(body.count)", "Connection: close"]
            return Data((lines.joined(separator: "\r\n") + "\r\n\r\n").utf8) + body
        }
    }

    /// Nil when `data` is not a complete response: no head, a short body, a broken chunk, or an encoding other than identity.
    static func parse(_ data: Data) -> Response? {
        guard let end = data.range(of: Data("\r\n\r\n".utf8)), let head = String(data: data[..<end.lowerBound], encoding: .utf8) else { return nil }
        let lines = head.components(separatedBy: "\r\n")
        let parts = lines[0].split(separator: " ")
        guard parts.count >= 2, parts[0].hasPrefix("HTTP/1."), let status = Int(parts[1]) else { return nil }
        var headers: [(name: String, value: String)] = []
        for line in lines.dropFirst() {
            guard let colon = line.firstIndex(of: ":") else { return nil }
            headers.append((String(line[..<colon]), line[line.index(after: colon)...].trimmingCharacters(in: .whitespaces)))
        }
        func header(_ name: String) -> String? { headers.first { $0.name.lowercased() == name }?.value }
        if let encoding = header("content-encoding"), encoding.lowercased() != "identity" { return nil }
        var rest = Data(data[end.upperBound...])
        var body = Data()
        if header("transfer-encoding")?.lowercased().contains("chunked") == true {
            while true {
                guard let lineEnd = rest.range(of: Data("\r\n".utf8)),
                      let sizeText = String(data: rest[..<lineEnd.lowerBound], encoding: .utf8)?.split(separator: ";").first,
                      let size = Int(sizeText.trimmingCharacters(in: .whitespaces), radix: 16) else { return nil }
                let start = lineEnd.upperBound
                if size == 0 { break }
                guard rest.count >= start - rest.startIndex + size + 2 else { return nil }
                body += rest[start..<(start + size)]
                rest = Data(rest[(start + size + 2)...])
            }
        } else if let length = header("content-length").flatMap(Int.init) {
            guard rest.count >= length else { return nil }
            body = Data(rest.prefix(length))
        } else {
            body = rest
        }
        return Response(status: status, statusLine: lines[0], headers: headers, body: body)
    }
}
