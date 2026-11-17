import Foundation

/// What a coding-agent harness needs to run in an isolated VM (`glov vm create --isolated --harness <name>`): the
/// secrets it uses and how each reaches it, the environment and home-directory files that turn off its optional
/// traffic, and the hosts it must reach. One file per harness in `Harnesses/`, compiled into the binary.
/// Each records the harness version it was last checked against and the vendor docs it relies on.
public struct HarnessProfile: Codable, Sendable, Equatable {
    public struct Verified: Codable, Sendable, Equatable {
        public var version: String
        /// `YYYY-MM-DD`.
        public var date: String
    }

    /// A file in the guest user's home, as `json` (written pretty-printed) or `text`.
    public struct File: Codable, Sendable, Equatable {
        /// Relative to the home directory.
        public var path: String
        public var json: JSONValue?
        public var text: String?

        public var content: String {
            if let text { return text }
            let encoder = JSONEncoder()
            encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
            return String(decoding: (try? encoder.encode(json ?? .object([:]))) ?? Data(), as: UTF8.self) + "\n"
        }
    }

    /// A secret the harness uses (`glov secret`), granted per VM. With `placeholder` the guest holds
    /// `glov-secret:<name>`, which the egress proxy swaps for the value on the way to `hosts`; with `tmpfs` the Guest
    /// Service writes the value to `/run/glov/secrets/<name>` (mode 0400, the guest user's), never to the VM's disk.
    public struct Secret: Codable, Sendable, Equatable {
        public enum Delivery: String, Codable, Sendable { case placeholder, tmpfs }
        public var name: String
        public var delivery: Delivery
        /// An environment variable (in /etc/environment) set to the placeholder, or for `tmpfs` to the file's path.
        public var env: String?
        /// A file in the guest user's home: for `placeholder` the profile's JSON file of that path (or a new one) with
        /// the placeholder at `json`; for `tmpfs` a symbolic link to the file in /run/glov/secrets.
        public var file: String?
        /// `placeholder` in a `file`: the keys that lead to the value inside the JSON document.
        public var json: [String]?
        /// Where the value is sent, for `glov secret set <name> --host`.
        public var hosts: [String]

        public var placeholder: String { "glov-secret:" + name }
        public var tmpfsPath: String { Self.tmpfsDirectory + "/" + name }
        public static let tmpfsDirectory = "/run/glov/secrets"
    }

    public var name: String
    public var title: String
    /// What the harness runs as, installed to /usr/local/bin.
    public var command: String
    public var secrets: [Secret]
    public var verified: Verified
    public var docs: [String]
    /// Set system-wide, in /etc/environment, with the `secrets`' variables (`guestEnvironment`).
    public var environment: [String: String]
    /// Written to the guest user's home, with the `secrets`' placeholders in them (`guestFiles`).
    public var files: [File]
    /// The egress allowlist it needs to run.
    public var runtime: [String]
    /// `runtime` plus where the harness installs from.
    public var install: [String]
    /// A shell script `glov vm harness install` runs as the guest user under the `install` allowlist. /bin/sh has no
    /// pipefail, so a vendor installer is downloaded to a file before it runs, never piped into a shell.
    public var installScript: String
    /// Its own sandbox needs unprivileged user namespaces, which Ubuntu restricts; a VM with such a harness lifts the
    /// restriction (it eases privilege escalation inside the guest, so only those VMs do).
    public var userNamespaces: Bool
    /// Traffic it cannot turn off, what is denied by design, and other behavior worth knowing.
    public var notes: [String]
    /// How a VM uses the owner's subscription instead of an API key (`glov secret login`); nil when it cannot.
    public var subscription: Subscription?
    /// The owner's config on the Mac (instructions, skills, agents) that `glov vm harness install` copies into the
    /// guest user's home, one way (`HostConfig`).
    public var hostConfig: [HostCopy]?
    /// Where the harness writes its conversation records in the guest user's home, kept on the Mac for review
    /// (`HarnessSessions`).
    public var sessions: [String]?

    /// One file or directory of the owner's home on the Mac, copied to the same kind of place in the guest's.
    public struct HostCopy: Codable, Sendable, Equatable {
        /// Relative to the Mac user's home.
        public var host: String
        /// Relative to the guest user's home.
        public var guest: String
    }

    /// A VM signed in to a subscription: its own credentials, kept on the Mac, and the guest set up for them in place of
    /// the API key. Each secret is the VM's own, named `<vm>.<harness>.<name>` (`secretName`).
    public struct Subscription: Codable, Sendable, Equatable {
        /// Run in the VM as the guest user, in a terminal, to sign in (the harness's own device sign-in); nil when the
        /// owner pastes a token made elsewhere instead, as `pasteHint` says.
        public var signIn: [String]?
        public var pasteHint: String?
        /// Run in the VM as the guest user at `glov secret logout`, while the proxy still holds the credentials.
        public var signOut: [String]?
        /// Replace the API-key `secrets` while signed in. One without `env` or `file` reaches the guest in a token
        /// endpoint's answer.
        public var secrets: [Secret]
        /// Where the harness gets and refreshes its tokens. The proxy puts the real values into the request body and keeps
        /// the tokens of the answer (`answer`: JSON field to secret name) on the Mac, so the guest only ever sees placeholders.
        public var tokenEndpoints: [TokenEndpoint]
        /// Added to `environment` while signed in.
        public var environment: [String: String]
        /// Single settings that differ while signed in; everything else in those files stays as the owner left it.
        public var settings: [Setting]
        /// Added to the allowlist while signed in.
        public var runtime: [String]
    }

    /// One setting in a home file: a key path in a JSON file, or a top-level key of a TOML file (`path` ends in `.toml`).
    /// While signed in it is `value`, or absent when `value` is nil; otherwise it is what the profile's own file says.
    public struct Setting: Codable, Sendable, Equatable {
        public var path: String
        public var key: [String]
        public var value: JSONValue?
        public var isTOML: Bool { path.hasSuffix(".toml") }
    }

    public struct TokenEndpoint: Codable, Sendable, Equatable {
        public var host: String
        /// Matched against the request path without its query.
        public var path: String
        public var answer: [String: String]?
    }

    /// The name of a subscription secret of VM `vm`.
    public func secretName(vm: String, _ name: String) -> String { "\(vm).\(self.name).\(name)" }

    /// This profile as a VM signed in to its subscription sees it: the subscription's secrets (named for `vm`) in place of
    /// the API key and its environment and allowlist added (its `settings` are applied by `HarnessMode`). Itself when
    /// there is no subscription.
    public func signedIn(vm: String) -> HarnessProfile {
        guard let sub = subscription else { return self }
        var p = self
        p.secrets = sub.secrets.map { var s = $0; s.name = secretName(vm: vm, s.name); return s }
        p.environment.merge(sub.environment) { $1 }
        p.runtime += sub.runtime.filter { !p.runtime.contains($0) }
        return p
    }

    /// `environment` plus each secret's variable.
    public var guestEnvironment: [String: String] {
        var env = environment
        for s in secrets {
            if let key = s.env { env[key] = s.delivery == .tmpfs ? s.tmpfsPath : s.placeholder }
        }
        return env
    }

    /// `files` with each `placeholder` secret written into its JSON file (a new file when the profile has none).
    public var guestFiles: [File] {
        var all = files
        for s in secrets where s.delivery == .placeholder {
            guard let path = s.file, let keys = s.json else { continue }
            let i = all.firstIndex { $0.path == path } ?? { all.append(File(path: path, json: .object([:]))); return all.count - 1 }()
            all[i].json = Self.setting(all[i].json ?? .object([:]), keys[...], to: .string(s.placeholder))
        }
        return all
    }

    /// Home-relative paths that link to a `tmpfs` secret's file.
    public var secretLinks: [(path: String, target: String)] {
        secrets.compactMap { s in s.delivery == .tmpfs ? s.file.map { ($0, s.tmpfsPath) } : nil }
    }

    private static func setting(_ value: JSONValue, _ keys: ArraySlice<String>, to new: JSONValue) -> JSONValue {
        guard let key = keys.first else { return new }
        var object: [String: JSONValue] = [:]
        if case .object(let o) = value { object = o }
        object[key] = setting(object[key] ?? .object([:]), keys.dropFirst(), to: new)
        return .object(object)
    }

    /// What is wrong with the profile's `secrets`, `subscription` and `hostConfig`, if anything; the profiles' test
    /// checks each.
    public var secretProblems: [String] {
        var problems = Self.problems(secrets, files: files, environment: environment) + HostConfig.problems(hostConfig ?? [], files: files)
        guard let sub = subscription else { return problems }
        let answered = Set(sub.tokenEndpoints.flatMap { ($0.answer ?? [:]).values })
        for s in sub.secrets where s.hosts.isEmpty { problems.append("subscription secret \(s.name): name the hosts it is sent to") }
        for s in sub.secrets where s.env == nil && s.file == nil && !answered.contains(s.name) {
            problems.append("subscription secret \(s.name): name a target, env or file, or a token endpoint answer field for it")
        }
        for name in answered where !sub.secrets.contains(where: { $0.name == name }) {
            problems.append("subscription: a token endpoint answers into \(name), which is not one of its secrets")
        }
        if (sub.signIn == nil) == (sub.pasteHint == nil) { problems.append("subscription: give exactly one of signIn and pasteHint") }
        if sub.signIn == nil, sub.secrets.count != 1 { problems.append("subscription: a pasted sign-in stores exactly one secret") }
        let signedIn = signedIn(vm: "vm")
        problems += Self.problems(signedIn.secrets.filter { $0.env != nil || $0.file != nil }, files: signedIn.files, environment: signedIn.environment)
            .map { "subscription: " + $0 }
        for setting in sub.settings {
            guard let file = files.first(where: { $0.path == setting.path }) else {
                problems.append("subscription setting \(setting.path): not one of the profile's files"); continue
            }
            if setting.isTOML {
                if setting.key.count != 1 || file.text == nil { problems.append("subscription setting \(setting.path): a TOML setting is one top-level key of a text file") }
                if let v = setting.value, case .string = v {} else if setting.value != nil { problems.append("subscription setting \(setting.path): a TOML value is a string") }
            } else if file.json == nil || setting.key.isEmpty {
                problems.append("subscription setting \(setting.path): not a JSON key path")
            }
        }
        return problems
    }

    private static func problems(_ secrets: [Secret], files: [File], environment: [String: String]) -> [String] {
        var problems: [String] = []
        for s in secrets {
            let targets = (s.env == nil ? 0 : 1) + (s.file == nil ? 0 : 1)
            if targets != 1 { problems.append("\(s.name): name one target, env or file") }
            switch s.delivery {
            case .placeholder:
                if s.file != nil, s.json?.isEmpty != false { problems.append("\(s.name): a placeholder in a file goes at a json path") }
                if let path = s.file, let f = files.first(where: { $0.path == path }), f.json == nil { problems.append("\(s.name): \(path) is not JSON") }
            case .tmpfs:
                if s.json != nil { problems.append("\(s.name): a tmpfs secret is the whole file; it takes no json path") }
                if let path = s.file, files.contains(where: { $0.path == path }) { problems.append("\(s.name): \(path) is also one of the profile's files") }
            }
            if s.hosts.isEmpty { problems.append("\(s.name): name the hosts it is sent to") }
            if let key = s.env, environment[key] != nil { problems.append("\(s.name): \(key) is also in environment") }
        }
        let names = secrets.map(\.name)
        if Set(names).count != names.count { problems.append("a secret is declared twice") }
        return problems
    }

    /// What `glov vm create` says about each secret of a new VM, none of them granted yet. `defined` holds the names
    /// `glov secret list` knows, nil when the daemon could not say.
    public func secretNeeds(vm: String, defined: Set<String>?) -> [String] {
        secrets.map { s in
            let target = s.env.map { "\($0)" } ?? "~/\(s.file ?? "")" + (s.json.map { " at " + $0.joined(separator: ".") } ?? "")
            let how = s.delivery == .tmpfs ? "the value in \(s.tmpfsPath), linked from \(target)" : "a placeholder in \(target)"
            let set = "glov secret set \(s.name) --host \(s.hosts.joined(separator: " --host "))"
            let grant = "glov secret grant \(vm) \(s.name)"
            let todo = switch defined?.contains(s.name) {
            case true?: "defined, not granted: \(grant)"
            case false?: "not defined or granted: \(set), then \(grant)"
            case nil: "not granted: \(grant) (define it first if glov secret list lacks it: \(set))"
            }
            return "harness \(name): secret \(s.name) (\(how), sent to \(s.hosts.joined(separator: ", "))) is \(todo)"
        }
    }

    public struct Unknown: Error, Equatable, CustomStringConvertible {
        public var name: String
        public var description: String { "unknown harness \(name): use \(HarnessProfile.names)" }
    }

    /// Sorted by name.
    public static let all: [HarnessProfile] = [
        PackageResources.antigravity_json, PackageResources.claude_json, PackageResources.codex_json,
    ].map { try! JSONDecoder().decode(HarnessProfile.self, from: Data($0)) }.sorted { $0.name < $1.name }

    /// "antigravity, claude or codex".
    public static var names: String {
        let all = all.map(\.name)
        return all.dropLast().joined(separator: ", ") + " or " + all.last!
    }

    public static func named(_ name: String) throws(Unknown) -> HarnessProfile {
        guard let p = all.first(where: { $0.name == name }) else { throw Unknown(name: name) }
        return p
    }
}
