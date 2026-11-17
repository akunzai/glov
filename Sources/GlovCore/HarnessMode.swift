import Foundation

/// How a harness VM's guest is set up for each harness: with the API key, or signed in to the owner's subscription
/// (`HarnessProfile.signedIn`). The host decides; these render what the guest must hold for that choice.
public enum HarnessMode {
    /// The harnesses the guest is set up as signed in, one name per line. The host compares it with vm.json at each
    /// Guest Service session, so a guest reverted to a snapshot taken in the other mode is set up again.
    public static let markerPath = "/etc/glov/harness-subscriptions"

    public static func marker(_ subscribed: Set<String>) -> String { subscribed.sorted().map { $0 + "\n" }.joined() }

    public static func parseMarker(_ text: String) -> Set<String> {
        Set(text.split(whereSeparator: \.isNewline).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty })
    }

    /// `/etc/environment` (`text`) with the variables of `profiles` as each one's mode sets them: every variable either
    /// mode sets is dropped, then the chosen mode's are appended. Other lines stay as they are.
    public static func environment(_ text: String, profiles: [HarnessProfile], subscribed: Set<String>, vm: String) -> String {
        var managed = Set<String>()
        var wanted: [String: String] = [:]
        for p in profiles {
            let signedIn = p.signedIn(vm: vm)
            managed.formUnion(p.guestEnvironment.keys)
            managed.formUnion(signedIn.guestEnvironment.keys)
            wanted.merge((subscribed.contains(p.name) ? signedIn : p).guestEnvironment) { $1 }
        }
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        if lines.last == "" { lines.removeLast() }
        lines.removeAll { line in
            guard let eq = line.firstIndex(of: "=") else { return false }
            return managed.contains(line[..<eq].trimmingCharacters(in: .whitespaces))
        }
        lines += wanted.sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }
        return lines.map { $0 + "\n" }.joined()
    }

    /// One setting in a home file as the chosen mode wants it (nil: absent).
    public struct Edit: Equatable, Sendable {
        public var path: String
        public var key: [String]
        public var value: JSONValue?
        public var isTOML: Bool { path.hasSuffix(".toml") }
    }

    /// The settings of `profiles` that differ between the two modes, as each one's mode wants them. Only these change
    /// in the guest's files, so whatever else the owner set there stays.
    public static func edits(profiles: [HarnessProfile], subscribed: Set<String>, vm: String) -> [Edit] {
        var out: [Edit] = []
        for p in profiles {
            guard let sub = p.subscription else { continue }
            let on = subscribed.contains(p.name)
            for setting in sub.settings {
                out.append(.init(path: setting.path, key: setting.key, value: on ? setting.value : apiValue(p, setting)))
            }
            // Placeholders that go into JSON files: the chosen mode's in, the other's out.
            let api = p.secrets.filter { $0.delivery == .placeholder && $0.file != nil && $0.json != nil }
            let signedIn = p.signedIn(vm: vm).secrets.filter { $0.delivery == .placeholder && $0.file != nil && $0.json != nil }
            for s in on ? api : signedIn { out.append(.init(path: s.file!, key: s.json!, value: nil)) }
            for s in on ? signedIn : api { out.append(.init(path: s.file!, key: s.json!, value: .string(s.placeholder))) }
        }
        return out
    }

    /// What the profile's own file says for `setting`.
    private static func apiValue(_ p: HarnessProfile, _ setting: HarnessProfile.Setting) -> JSONValue? {
        guard let file = p.guestFiles.first(where: { $0.path == setting.path }) else { return nil }
        if setting.isTOML { return file.text.flatMap { tomlValue($0, setting.key[0]) }.map { .string($0) } }
        var value = file.json
        for k in setting.key { value = value?[k] }
        return value
    }

    /// `text` (a file of `path`) with `edits` for that path applied. A JSON file that does not parse is left alone
    /// (nil), so a hand-broken file is not replaced.
    public static func apply(_ edits: [Edit], to text: String, path: String) -> String? {
        let mine = edits.filter { $0.path == path }
        guard !mine.isEmpty else { return text }
        if path.hasSuffix(".toml") {
            var text = text
            for e in mine { text = settingTOML(text, e.key[0], e.value.flatMap { if case .string(let s) = $0 { s } else { nil } }) }
            return text
        }
        let data = Data((text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty ? "{}" : text).utf8)
        guard var json = try? JSONDecoder().decode(JSONValue.self, from: data) else { return nil }
        for e in mine { json = setting(json, e.key[...], to: e.value) ?? .object([:]) }
        return HarnessProfile.File(path: path, json: json).content
    }

    private static func setting(_ value: JSONValue, _ keys: ArraySlice<String>, to new: JSONValue?) -> JSONValue? {
        guard let key = keys.first else { return new }
        var object: [String: JSONValue] = [:]
        if case .object(let o) = value { object = o }
        object[key] = setting(object[key] ?? .object([:]), keys.dropFirst(), to: new)
        return .object(object)
    }

    /// The string value of top-level `key` in TOML `text`, if it has one.
    static func tomlValue(_ text: String, _ key: String) -> String? {
        for line in topLevel(text) {
            guard let (k, v) = keyValue(line), k == key, v.hasPrefix("\""), v.hasSuffix("\""), v.count >= 2 else { continue }
            return String(v.dropFirst().dropLast())
        }
        return nil
    }

    /// `text` with top-level `key` set to the string `value`, or removed when nil; a new key goes before the first table.
    static func settingTOML(_ text: String, _ key: String, _ value: String?) -> String {
        var lines = text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        let firstTable = lines.firstIndex { $0.trimmingCharacters(in: .whitespaces).hasPrefix("[") } ?? lines.count
        let new = value.map { "\(key) = \"" + $0.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\"" }
        if let i = lines[..<firstTable].firstIndex(where: { keyValue($0)?.key == key }) {
            if let new { lines[i] = new } else { lines.remove(at: i) }
        } else if let new {
            var at = firstTable
            while at > 0, lines[at - 1].trimmingCharacters(in: .whitespaces).isEmpty { at -= 1 }
            lines.insert(new, at: at)
        }
        return lines.joined(separator: "\n")
    }

    private static func topLevel(_ text: String) -> [String] {
        Array(text.split(separator: "\n", omittingEmptySubsequences: false).map(String.init).prefix { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("[") })
    }

    private static func keyValue(_ line: String) -> (key: String, value: String)? {
        let t = line.trimmingCharacters(in: .whitespaces)
        guard !t.hasPrefix("#"), let eq = t.firstIndex(of: "=") else { return nil }
        return (t[..<eq].trimmingCharacters(in: .whitespaces), t[t.index(after: eq)...].trimmingCharacters(in: .whitespaces))
    }
}
