import Foundation
import GlovCore
import GlovDaemonAPI
import Synchronization

/// The secrets the owner named on this Mac (`glov secret`), in `<home>/secrets.json`: each one's source and the hosts it
/// is bound to. Values never enter the file: a Keychain secret's value lives in `values`, a command secret's is the
/// command's stdout, fetched when needed and kept in memory for `ttl`.
actor SecretBroker {
    static let ttl: Duration = .seconds(60)
    static let commandTimeout: Duration = .seconds(10)

    /// Runs a source command; returns its stdout or throws a message naming nothing of its output.
    typealias Runner = @Sendable (_ argv: [String], _ timeout: Duration) async throws -> String

    private let path: String
    private let values: any ValueStore
    private let runner: Runner
    private let now: @Sendable () -> ContinuousClock.Instant
    private var cache: [String: (value: String, until: ContinuousClock.Instant)] = [:]

    init(home: GlovHome, values: any ValueStore, runner: @escaping Runner = SecretBroker.run, now: @escaping @Sendable () -> ContinuousClock.Instant = { .now }) {
        path = home.path + "/secrets.json"
        self.values = values
        self.runner = runner
        self.now = now
    }

    // MARK: Definitions

    func list() throws -> [SecretDefinition] { try load() }

    /// The definitions as the file has them now, for the egress proxy's blocking threads: the file is replaced whole,
    /// so this needs no actor hop (which could wait forever on a busy cooperative pool).
    nonisolated func definitionsNow() throws -> [SecretDefinition] { try Self.load(path) }

    func definition(_ name: String) throws -> SecretDefinition? { try load().first { $0.name == name } }

    /// Stores a Keychain secret, replacing any secret by that name.
    func set(_ name: String, hosts: [String], value: String, forge: Forge? = nil) async throws {
        var definition = try SecretDefinition(name: name, source: .keychain, hosts: hosts)
        definition.forge = forge
        definition.length = value.count
        definition.ending = SecretEntry.ending(of: value)
        guard !value.isEmpty else { throw RPCError(code: RPCError.invalidParams, message: "secret \(name): the value is empty") }
        let values = values
        try await Blocking.run { try values.set(name, value) }
        try save(definition)
    }

    /// Records a command secret, replacing any secret by that name (and its Keychain value).
    func source(_ name: String, hosts: [String], command: [String], forge: Forge? = nil) async throws {
        guard let program = command.first, program.hasPrefix("/") else {
            throw RPCError(code: RPCError.invalidParams, message: "secret \(name): give the command with an absolute path")
        }
        var definition = try SecretDefinition(name: name, source: .command(command), hosts: hosts)
        definition.forge = forge
        let values = values
        _ = try await Blocking.run { try values.remove(name) }
        try save(definition)
    }

    /// False if there was no secret by that name.
    func remove(_ name: String) async throws -> Bool {
        guard try definition(name) != nil else { return false }
        let values = values
        _ = try await Blocking.run { try values.remove(name) }
        // Loaded after the await: another call may have changed the file meanwhile.
        try write(try load().filter { $0.name != name })
        return true
    }

    // MARK: Values

    /// The value of `name`, for the egress proxy. Errors name the secret, never a value or a command's output.
    func value(_ name: String) async throws -> String {
        guard let definition = try definition(name) else { throw RPCError(code: RPCError.failed, message: "no secret named \(name)") }
        switch definition.source {
        case .keychain:
            let values = values
            guard let value = try await Blocking.run({ try values.get(name) }) else {
                throw RPCError(code: RPCError.failed, message: "secret \(name) has no stored value: store it again with glov secret set \(name)")
            }
            return value
        case .command(let argv):
            if let hit = cache[name], hit.until > now() { return hit.value }
            let value: String
            do { value = try await runner(argv, Self.commandTimeout) } catch {
                throw RPCError(code: RPCError.failed, message: "secret \(name): \(error)")
            }
            guard !value.isEmpty else { throw RPCError(code: RPCError.failed, message: "secret \(name): \(argv[0]) printed nothing") }
            cache[name] = (value, now() + Self.ttl)
            return value
        }
    }

    // MARK: File

    private func load() throws -> [SecretDefinition] { try Self.load(path) }

    private static func load(_ path: String) throws -> [SecretDefinition] {
        guard FileManager.default.fileExists(atPath: path) else { return [] }
        do { return try JSONDecoder().decode([SecretDefinition].self, from: Data(contentsOf: URL(fileURLWithPath: path))) } catch {
            throw RPCError(code: RPCError.failed, message: "cannot read \(path): \(error)")
        }
    }

    private func save(_ definition: SecretDefinition) throws {
        var all = try load().filter { $0.name != definition.name }
        all.append(definition)
        try write(all.sorted { $0.name < $1.name })
        cache[definition.name] = nil
    }

    private func write(_ all: [SecretDefinition]) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(all)
        let temp = path + ".tmp"
        guard FileManager.default.createFile(atPath: temp, contents: data, attributes: [.posixPermissions: 0o600]) else {
            throw RPCError(code: RPCError.failed, message: "cannot write \(temp)")
        }
        _ = try FileManager.default.replaceItemAt(URL(fileURLWithPath: path), withItemAt: URL(fileURLWithPath: temp))
        for name in Set(cache.keys).subtracting(all.map(\.name)) { cache[name] = nil }
    }

    // MARK: Command runner

    struct CommandFailure: Error, CustomStringConvertible {
        var description: String
    }

    /// Runs `argv` with stdin closed and stderr discarded, so neither the value nor any error text reaches a log.
    static func run(_ argv: [String], timeout: Duration) async throws -> String {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: argv[0])
        process.arguments = Array(argv.dropFirst())
        process.standardInput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        let out = Pipe()
        process.standardOutput = out
        // A termination handler rather than waitUntilExit, which was seen to miss an exit while other processes started.
        let (exited, exit) = AsyncStream<Void>.makeStream()
        process.terminationHandler = { _ in exit.yield(); exit.finish() }
        do { try process.run() } catch { throw CommandFailure(description: "cannot run \(argv[0])") }
        let timedOut = Atomic<Bool>(false)
        let watchdog = Task {
            try? await Task.sleep(for: timeout)
            if process.isRunning { timedOut.store(true, ordering: .relaxed); process.terminate() }
        }
        let data = await Blocking.run { out.fileHandleForReading.readDataToEndOfFile() }
        for await _ in exited {}
        watchdog.cancel()
        if timedOut.load(ordering: .relaxed) { throw CommandFailure(description: "\(argv[0]) did not finish within \(timeout)") }
        guard process.terminationStatus == 0 else { throw CommandFailure(description: "\(argv[0]) exited with status \(process.terminationStatus)") }
        var text = String(decoding: data, as: UTF8.self)
        while text.hasSuffix("\n") || text.hasSuffix("\r") { text.removeLast() }
        return text
    }
}

/// One named secret: where its value comes from and the hosts it may be sent to.
struct SecretDefinition: Codable, Sendable, Equatable {
    enum Source: Codable, Sendable, Equatable {
        case keychain
        /// The value is the command's stdout, without its line end.
        case command([String])
    }

    var name: String
    var source: Source
    /// Egress entries in canonical form (`EgressPolicy`): a hostname, `*.suffix` or an address, optionally with `:port`.
    var hosts: [String]
    /// A Keychain value's length and, when it has at least 16 characters, its last 4: what may be shown of it, kept here
    /// so `glov secret list` reads no value. Nil for a command secret and for one stored before they were kept.
    var length: Int?
    var ending: String?
    /// A git sign-in: requests to `hosts` from a VM granted it are held to the VM's repositories (`RepoAccess`).
    var forge: Forge?

    init(name: String, source: Source, hosts: [String]) throws {
        try SecretName.validate(name)
        guard !hosts.isEmpty else { throw RPCError(code: RPCError.invalidParams, message: "secret \(name): bind it to at least one host with --host") }
        let canonical = hosts.map { ($0, EgressPolicy.canonical($0)) }
        let invalid = canonical.filter { $0.1 == nil }.map(\.0)
        guard invalid.isEmpty else { throw RPCError(code: RPCError.invalidParams, message: "secret \(name): \(EgressError.invalidEntries(invalid))") }
        var unique: [String] = []
        for form in canonical.compactMap(\.1) where !unique.contains(form) { unique.append(form) }
        self.name = name
        self.source = source
        self.hosts = unique
    }

    var summary: Results.Secret {
        switch source {
        case .keychain: Results.Secret(name: name, source: "keychain", hosts: hosts, length: length, ending: ending, forge: forge?.rawValue)
        case .command(let argv): Results.Secret(name: name, source: "command", command: argv, hosts: hosts, forge: forge?.rawValue)
        }
    }
}

enum SecretName {
    /// Letters, digits, `.`, `_` and `-`, at most 64.
    static func validate(_ name: String) throws {
        let allowed = Set("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-")
        guard (1...64).contains(name.count), name.allSatisfy(allowed.contains) else {
            throw RPCError(code: RPCError.invalidParams, message: "secret name \(name): use letters, digits, '.', '_' or '-', at most 64")
        }
    }
}
