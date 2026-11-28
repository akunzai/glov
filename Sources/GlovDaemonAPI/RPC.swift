import Foundation
import GlovCore

/// JSON-RPC 2.0 over a unix socket, one JSON object per line.
public struct RPCRequest: Codable, Sendable, Equatable {
    public var jsonrpc = "2.0"
    public var id: JSONValue?
    public var method: String
    public var params: JSONValue?

    public init(id: JSONValue? = nil, method: String, params: JSONValue? = nil) {
        self.id = id
        self.method = method
        self.params = params
    }
}

public struct RPCError: Codable, Error, Sendable, Equatable, CustomStringConvertible {
    public static let parseError = -32700
    public static let invalidRequest = -32600
    public static let methodNotFound = -32601
    public static let invalidParams = -32602
    /// Anything the method itself reports (unknown VM, VM not running, QMP failure, ...).
    public static let failed = -32000
    /// `exec` with `as: "user"` while nobody is signed in to the guest console (Guest Service error 4); the message is unchanged.
    public static let noUser = -32001
    /// The change would let a `tmpfs` secret's real value reach any host (an open VM): repeat it with
    /// `allowPlainSecrets` once the owner confirms.
    public static let plainSecrets = -32002

    public var code: Int
    public var message: String
    public init(code: Int, message: String) { self.code = code; self.message = message }
    public var description: String { message }
}

/// Runs `attempt` again every `interval` while it fails with `RPCError.noUser` (nobody is signed in to the guest console
/// yet), calling `waiting` before the first retry; after `timeout` that error is thrown. Any other error is thrown at once.
public func retryingWhileNoUser<T>(
    timeout: Duration, interval: Duration = .seconds(2), waiting: () -> Void = {}, _ attempt: () async throws -> T
) async throws -> T {
    let deadline = ContinuousClock.now + timeout
    var first = true
    while true {
        do { return try await attempt() } catch let e as RPCError where e.code == RPCError.noUser && ContinuousClock.now < deadline {
            if first { waiting(); first = false }
            try await Task.sleep(for: interval)
        }
    }
}

public struct RPCResponse: Codable, Sendable, Equatable {
    public var jsonrpc = "2.0"
    public var id: JSONValue
    public var result: JSONValue?
    public var error: RPCError?

    public init(id: JSONValue, result: JSONValue) { self.id = id; self.result = result }
    public init(id: JSONValue, error: RPCError) { self.id = id; self.error = error }
}

public enum Method {
    public static let daemonInfo = "daemon.info"
    public static let guestServiceCheck = "guest.check"
    public static let guestServiceUpdate = "guest.update"
    public static let vmList = "vm.list"
    public static let vmStatus = "vm.status"
    public static let vmConfigure = "vm.configure"
    public static let vmDNS = "vm.dns"
    public static let vmStart = "vm.start"
    public static let vmStop = "vm.stop"
    public static let vmReboot = "vm.reboot"
    public static let vmWait = "vm.wait"
    /// Takes `Params.Balloon`, returns `Results.Balloon`.
    public static let vmBalloon = "vm.balloon"
    public static let screenshot = "screen.screenshot"
    public static let click = "input.click"
    public static let move = "input.move"
    public static let drag = "input.drag"
    public static let type = "input.type"
    public static let key = "input.key"
    public static let scroll = "input.scroll"
    public static let execRun = "exec.run"
    public static let execStart = "exec.start"
    public static let execRead = "exec.read"
    public static let execKill = "exec.kill"
    public static let fileRead = "file.read"
    public static let fileWrite = "file.write"
    public static let copyToVM = "copy.toVM"
    public static let copyFromVM = "copy.fromVM"
    public static let snapshotList = "snapshot.list"
    public static let snapshotCreate = "snapshot.create"
    public static let snapshotRevert = "snapshot.revert"
    public static let snapshotDelete = "snapshot.delete"
    public static let shareSync = "share.sync"
    public static let shareStatus = "share.status"
    public static let vmStats = "vm.stats"
    public static let egressList = "egress.list"
    public static let egressAllow = "egress.allow"
    public static let egressRemove = "egress.remove"
    public static let egressLog = "egress.log"
    public static let egressMCP = "egress.mcp"
    public static let egressMode = "egress.mode"
    public static let trustList = "trust.list"
    public static let trustAdd = "trust.add"
    public static let trustRemove = "trust.remove"
    public static let secretSet = "secret.set"
    public static let secretSource = "secret.source"
    public static let secretList = "secret.list"
    public static let secretRemove = "secret.remove"
    public static let secretGrant = "secret.grant"
    public static let secretRevoke = "secret.revoke"
    public static let secretGrants = "secret.grants"
    public static let secretRepos = "secret.repos"
    public static let secretLogin = "secret.login"
    public static let secretLogout = "secret.logout"
}

/// Parameter and result shapes. Every method that targets a VM takes `vm`: a name or a unique prefix.
public enum Params {
    public struct GuestService: Codable, Sendable {
        public var vm: String
        public var binary: String?
        public var force: Bool?
        public var timeout: Double?
        public init(vm: String, binary: String? = nil, force: Bool? = nil, timeout: Double? = nil) {
            self.vm = vm; self.binary = binary; self.force = force; self.timeout = timeout
        }
    }

    public struct VM: Codable, Sendable { public var vm: String; public init(vm: String) { self.vm = vm } }

    /// `egress.list` (no hosts), `egress.allow` and `egress.remove` of an isolated VM.
    public struct Egress: Codable, Sendable {
        public var vm: String
        public var hosts: [String]?
        /// Set by `glov mcp` on every change it relays: refused unless the VM opted in (`egress.mcp`).
        public var mcp: Bool?
        /// `egress.allow`: the hosts may also resolve to private-network addresses (`--private`); refused with `mcp`.
        public var `private`: Bool?
        public init(vm: String, hosts: [String]? = nil, mcp: Bool? = nil, private: Bool? = nil) {
            self.vm = vm; self.hosts = hosts; self.mcp = mcp; self.private = `private`
        }
    }

    /// `trust.list {vm}`; `trust.add {vm, name, certificate}` (PEM, one certificate) adds or replaces; `trust.remove {vm, name}`.
    /// CLI only: `glov mcp` relays none of them.
    public struct Trust: Codable, Sendable {
        public var vm: String
        public var name: String?
        public var certificate: String?
        public init(vm: String, name: String? = nil, certificate: String? = nil) { self.vm = vm; self.name = name; self.certificate = certificate }
    }

    /// `egress.mode`: open (any public host, through the proxy) or restricted (the allowlist). CLI only: `glov mcp`
    /// relays no tool for it. Opening a VM that holds `tmpfs` secrets needs `allowPlainSecrets`.
    public struct EgressMode: Codable, Sendable {
        public var vm: String
        public var open: Bool
        public var allowPlainSecrets: Bool?
        public init(vm: String, open: Bool, allowPlainSecrets: Bool? = nil) { self.vm = vm; self.open = open; self.allowPlainSecrets = allowPlainSecrets }
    }

    /// `egress.mcp`: whether MCP clients may change the VM's allowlist. `glov mcp` relays no tool for it.
    public struct EgressMCP: Codable, Sendable {
        public var vm: String
        public var enabled: Bool
        public init(vm: String, enabled: Bool) { self.vm = vm; self.enabled = enabled }
    }

    /// `secret.set {name, hosts, value}` stores a Keychain secret; `secret.source {name, hosts, command}` records a command
    /// (absolute path first) whose stdout is the value; `secret.remove {name}`; `secret.list {}`. No method returns a value.
    public struct Secret: Codable, Sendable {
        public var name: String?
        public var hosts: [String]?
        public var value: String?
        public var command: [String]?
        /// `github` or `gitlab`: a git sign-in, whose hosts are held to each VM's repositories (`secret.repos`).
        public var forge: String?
        public init(name: String? = nil, hosts: [String]? = nil, value: String? = nil, command: [String]? = nil, forge: String? = nil) {
            self.name = name; self.hosts = hosts; self.value = value; self.command = command; self.forge = forge
        }
    }

    /// `secret.repos {vm, add?, remove?}`: changes and returns the repositories (`<host>/<owner>/<repo>`, `<host>/<group>/*`)
    /// the VM may reach on the hosts of its git secrets.
    public struct SecretRepos: Codable, Sendable {
        public var vm: String
        public var add: [String]?
        public var remove: [String]?
        public init(vm: String, add: [String]? = nil, remove: [String]? = nil) { self.vm = vm; self.add = add; self.remove = remove }
    }

    /// `secret.grant` / `secret.revoke {vm, names}`, `secret.grants {vm}`.
    public struct SecretGrant: Codable, Sendable {
        public var vm: String
        public var names: [String]?
        /// Granting a `tmpfs` secret to an open VM needs this.
        public var allowPlainSecrets: Bool?
        public init(vm: String, names: [String]? = nil, allowPlainSecrets: Bool? = nil) {
            self.vm = vm; self.names = names; self.allowPlainSecrets = allowPlainSecrets
        }
    }

    /// `secret.login {vm, harness, value?}` (`value` for a harness whose token is pasted), `secret.logout {vm, harness}`.
    public struct SecretLogin: Codable, Sendable {
        public var vm: String
        public var harness: String
        public var value: String?
        public init(vm: String, harness: String, value: String? = nil) { self.vm = vm; self.harness = harness; self.value = value }
    }

    /// `egress.log`: with `cursor` (from an earlier answer) only what came after it, else the last `limit` records.
    public struct EgressLogRead: Codable, Sendable {
        public var vm: String
        /// Only `deny` and `fail` records.
        public var denied: Bool?
        public var cursor: String?
        public var limit: Int?
        public static let defaultLimit = 100
        /// The most records one answer without a cursor carries.
        public static let maxLimit = 10_000
        public init(vm: String, denied: Bool? = nil, cursor: String? = nil, limit: Int? = nil) {
            self.vm = vm; self.denied = denied; self.cursor = cursor; self.limit = limit
        }
    }

    /// `vm.stats`: one VM, or every running one when nil.
    public struct Stats: Codable, Sendable { public var vm: String?; public init(vm: String? = nil) { self.vm = vm } }

    public struct Configure: Codable, Sendable {
        public var vm: String
        public var cpus: Int?
        public var memory: String?
        public var diskSize: String?
        /// QEMU: turn the virtio-balloon device on or off (`VMConfig.balloon`).
        public var balloon: Bool?
        public init(vm: String, cpus: Int? = nil, memory: String? = nil, diskSize: String? = nil, balloon: Bool? = nil) {
            self.vm = vm; self.cpus = cpus; self.memory = memory; self.diskSize = diskSize; self.balloon = balloon
        }
    }

    /// `vm.dns`: set (`servers` non-empty) or reset to DHCP (`servers` empty) a QEMU (Windows) VM's guest DNS servers.
    public struct DNS: Codable, Sendable {
        public var vm: String
        public var servers: [String]
        public init(vm: String, servers: [String]) { self.vm = vm; self.servers = servers }
    }

    /// `vm.balloon`: with `size` (QEMU -m syntax), fixes how much memory the guest keeps; with `auto`, lets glovd choose
    /// from the guest's use; with neither, only reports. Both are saved in vm.json and applied whenever the VM starts.
    public struct Balloon: Codable, Sendable {
        public var vm: String
        public var size: String?
        public var auto: Bool?
        public init(vm: String, size: String? = nil, auto: Bool? = nil) { self.vm = vm; self.size = size; self.auto = auto }
    }

    public struct Stop: Codable, Sendable {
        public var vm: String
        public var force: Bool?
        public init(vm: String, force: Bool? = nil) { self.vm = vm; self.force = force }
    }

    public struct Reboot: Codable, Sendable {
        public var vm: String
        /// Seconds to wait for the new boot's Guest Service; the daemon defaults to `defaultTimeout`.
        public var timeout: Double?
        /// Interrupt running jobs, share syncs and share mounts instead of refusing.
        public var force: Bool?
        /// Long enough for a Windows restart that installs updates.
        public static let defaultTimeout: Double = 600
        public init(vm: String, timeout: Double? = nil, force: Bool? = nil) { self.vm = vm; self.timeout = timeout; self.force = force }
    }

    public struct Wait: Codable, Sendable {
        public var vm: String
        public var state: String
        /// Seconds; the daemon defaults to 60.
        public var timeout: Double?
        /// "running" or "guest-ready".
        public init(vm: String, state: String = "running", timeout: Double? = nil) {
            self.vm = vm; self.state = state; self.timeout = timeout
        }
    }

    public struct Region: Codable, Sendable, Equatable {
        public var x: Int, y: Int, width: Int, height: Int
        public init(x: Int, y: Int, width: Int, height: Int) { self.x = x; self.y = y; self.width = width; self.height = height }
    }

    public struct Screenshot: Codable, Sendable {
        public var vm: String
        public var maxWidth: Int?
        public var region: Region?
        public init(vm: String, maxWidth: Int? = nil, region: Region? = nil) { self.vm = vm; self.maxWidth = maxWidth; self.region = region }
    }

    public struct Click: Codable, Sendable {
        public var vm: String
        public var x: Int, y: Int
        public var button: String?
        public var count: Int?
        public init(vm: String, x: Int, y: Int, button: String? = nil, count: Int? = nil) {
            self.vm = vm; self.x = x; self.y = y; self.button = button; self.count = count
        }
    }

    public struct Move: Codable, Sendable {
        public var vm: String
        public var x: Int, y: Int
        public init(vm: String, x: Int, y: Int) { self.vm = vm; self.x = x; self.y = y }
    }

    public struct Drag: Codable, Sendable {
        public var vm: String
        public var fromX: Int, fromY: Int, toX: Int, toY: Int
        public var button: String?
        public init(vm: String, fromX: Int, fromY: Int, toX: Int, toY: Int, button: String? = nil) {
            self.vm = vm; self.fromX = fromX; self.fromY = fromY; self.toX = toX; self.toY = toY; self.button = button
        }
    }

    public struct TypeText: Codable, Sendable {
        public var vm: String
        public var text: String
        public init(vm: String, text: String) { self.vm = vm; self.text = text }
    }

    public struct Key: Codable, Sendable {
        public var vm: String
        public var key: String
        public init(vm: String, key: String) { self.vm = vm; self.key = key }
    }

    /// `exec.run` and `exec.start`; `timeout` (seconds, default 30, at most 50) only applies to `exec.run`.
    public struct Exec: Codable, Sendable {
        public var vm: String
        public var argv: [String]
        public var cwd: String?
        public var env: [String: String]?
        /// "system" (default) or "user".
        public var `as`: String?
        /// Text written to the process's stdin, then end-of-file.
        public var stdin: String?
        public var timeout: Double?
        public init(
            vm: String, argv: [String], cwd: String? = nil, env: [String: String]? = nil, as runAs: String? = nil,
            stdin: String? = nil, timeout: Double? = nil
        ) {
            self.vm = vm; self.argv = argv; self.cwd = cwd; self.env = env; self.as = runAs; self.stdin = stdin; self.timeout = timeout
        }
    }

    public struct ExecRead: Codable, Sendable {
        public var vm: String
        public var jobId: String
        /// Byte offset into the job's total output (the `cursor` of the previous read); default 0.
        public var cursor: Int?
        public var maxBytes: Int?
        public init(vm: String, jobId: String, cursor: Int? = nil, maxBytes: Int? = nil) {
            self.vm = vm; self.jobId = jobId; self.cursor = cursor; self.maxBytes = maxBytes
        }
    }

    public struct ExecKill: Codable, Sendable {
        public var vm: String
        public var jobId: String
        public init(vm: String, jobId: String) { self.vm = vm; self.jobId = jobId }
    }

    public struct FileRead: Codable, Sendable {
        public var vm: String
        public var path: String
        public var maxBytes: Int?
        public init(vm: String, path: String, maxBytes: Int? = nil) { self.vm = vm; self.path = path; self.maxBytes = maxBytes }
    }

    public struct FileWrite: Codable, Sendable {
        public var vm: String
        public var path: String
        public var text: String
        public init(vm: String, path: String, text: String) { self.vm = vm; self.path = path; self.text = text }
    }

    public struct CopyToVM: Codable, Sendable {
        public var vm: String
        public var hostPath: String
        public var guestPath: String
        public init(vm: String, hostPath: String, guestPath: String) { self.vm = vm; self.hostPath = hostPath; self.guestPath = guestPath }
    }

    public struct CopyFromVM: Codable, Sendable {
        public var vm: String
        public var guestPath: String
        public var hostPath: String
        public init(vm: String, guestPath: String, hostPath: String) { self.vm = vm; self.guestPath = guestPath; self.hostPath = hostPath }
    }

    /// `snapshot.create`, `snapshot.revert` and `snapshot.delete`; `name` is 1 to 64 characters of `A-Za-z0-9._-`.
    public struct Snapshot: Codable, Sendable {
        public var vm: String
        public var name: String
        public init(vm: String, name: String) { self.vm = vm; self.name = name }
    }

    /// `share` is a share name, optionally followed by `/<subdir>` to sync one directory of it.
    public struct ShareSync: Codable, Sendable {
        public var vm: String
        public var share: String
        /// Rehash every file on both sides instead of trusting unchanged stat keys.
        public var verify: Bool?
        public init(vm: String, share: String, verify: Bool? = nil) { self.vm = vm; self.share = share; self.verify = verify }
    }

    public struct ShareStatus: Codable, Sendable {
        public var vm: String
        /// Only this share; all of the VM's syncs when nil.
        public var share: String?
        public init(vm: String, share: String? = nil) { self.vm = vm; self.share = share }
    }

    public struct Scroll: Codable, Sendable {
        public var vm: String
        public var x: Int, y: Int
        public var dx: Int?, dy: Int?
        public init(vm: String, x: Int, y: Int, dx: Int? = nil, dy: Int? = nil) {
            self.vm = vm; self.x = x; self.y = y; self.dx = dx; self.dy = dy
        }
    }
}

public enum Results {
    /// `vm.dns`: the saved DNS servers (empty when reset to DHCP) and whether the running guest updated now.
    public struct DNS: Codable, Sendable, Equatable {
        public var vm: String
        public var servers: [String]
        public var applied: Bool
        public init(vm: String, servers: [String], applied: Bool) { self.vm = vm; self.servers = servers; self.applied = applied }
    }
    /// `vm.balloon`: the guest's memory now, and the target when one was just set. The guest gives memory up gradually,
    /// and the Windows driver stops early when the guest runs short, so `memory` can stay above `target`.
    public struct Balloon: Codable, Sendable, Equatable {
        public var vm: String
        /// Bytes the guest has (QMP `query-balloon` `actual`); nil while the VM is stopped.
        public var memory: UInt64?
        /// Bytes the VM was given (`-m`).
        public var configured: UInt64
        /// What the guest is asked to keep; nil keeps all of `configured`.
        public var target: UInt64?
        /// "auto" (glovd picks the target) or "fixed".
        public var mode: String?
        /// Auto: why the target is what it is.
        public var reason: String?
        /// Auto: the guest's own use, without the balloon's memory.
        public var used: UInt64?
        /// Auto: the Mac's memory pressure at the last decision.
        public var pressure: String?
        public init(
            vm: String, memory: UInt64?, configured: UInt64, target: UInt64? = nil, mode: String? = nil, reason: String? = nil,
            used: UInt64? = nil, pressure: String? = nil
        ) {
            self.vm = vm; self.memory = memory; self.configured = configured; self.target = target; self.mode = mode
            self.reason = reason; self.used = used; self.pressure = pressure
        }
    }

    public struct Egress: Codable, Sendable, Equatable {
        public var vm: String
        public var allow: [String]
        public var mcpEdits: Bool
        /// The `allow` entries that may resolve to private-network addresses.
        public var `private`: [String]?
        /// True when the VM may reach any public host (`egress.mode`); `allow` then waits for a switch back.
        public var open: Bool?
        public init(vm: String, allow: [String], mcpEdits: Bool = false, private: [String]? = nil, open: Bool? = nil) {
            self.vm = vm; self.allow = allow; self.mcpEdits = mcpEdits; self.private = `private`; self.open = open
        }
    }

    /// The `trust.*` answer: the VM's CA certificates, in the order added.
    public struct Trust: Codable, Sendable, Equatable {
        public var vm: String
        public var certificates: [TrustedCertificate]
        public init(vm: String, certificates: [TrustedCertificate]) { self.vm = vm; self.certificates = certificates }
    }

    public struct TrustedCertificate: Codable, Sendable, Equatable {
        public var name: String
        public var subject: String
        public var notAfter: Date?
        /// Of the DER certificate, lowercase hex.
        public var sha256: String
        /// Whether it says it is a CA (basic constraints); a self-signed server certificate works as trust too.
        public var authority: Bool
        public init(name: String, subject: String, notAfter: Date?, sha256: String, authority: Bool) {
            self.name = name; self.subject = subject; self.notAfter = notAfter; self.sha256 = sha256; self.authority = authority
        }
    }

    /// A secret as `secret.list` shows it: never its value.
    public struct Secret: Codable, Sendable, Equatable {
        public var name: String
        /// `keychain` or `command`.
        public var source: String
        public var command: [String]?
        public var hosts: [String]
        /// A Keychain value's length, and its last 4 characters when it has at least 16; nil if not recorded.
        public var length: Int?
        public var ending: String?
        public var forge: String?
        public init(
            name: String, source: String, command: [String]? = nil, hosts: [String], length: Int? = nil, ending: String? = nil, forge: String? = nil
        ) {
            self.name = name; self.source = source; self.command = command; self.hosts = hosts; self.length = length; self.ending = ending
            self.forge = forge
        }
    }

    public struct Secrets: Codable, Sendable, Equatable {
        public var secrets: [Secret]
        public init(secrets: [Secret]) { self.secrets = secrets }
    }

    public struct SecretRepos: Codable, Sendable, Equatable {
        public var vm: String
        public var repos: [String]
        public init(vm: String, repos: [String]) { self.vm = vm; self.repos = repos }
    }

    public struct SecretGrants: Codable, Sendable, Equatable {
        public var vm: String
        public var names: [String]
        /// Granted names no secret is defined for (removed since).
        public var missing: [String]
        public init(vm: String, names: [String], missing: [String] = []) { self.vm = vm; self.names = names; self.missing = missing }
    }

    /// `secret.login` / `secret.logout`.
    public struct SecretLogin: Codable, Sendable, Equatable {
        public var vm: String
        public var harness: String
        /// After `secret.login`: the command the owner runs in the VM, as user glov in a terminal, to sign in; nil when
        /// the token was pasted.
        public var command: [String]?
        /// The harness's subscription secrets not stored yet: a sign-in in the guest stores them.
        public var missing: [String]
        public init(vm: String, harness: String, command: [String]? = nil, missing: [String] = []) {
            self.vm = vm; self.harness = harness; self.command = command; self.missing = missing
        }
    }

    public struct EgressLogPage: Codable, Sendable, Equatable {
        public var records: [EgressRecord]
        public var cursor: String
        public init(records: [EgressRecord], cursor: String) { self.records = records; self.cursor = cursor }
    }

    public struct DaemonInfo: Codable, Sendable {
        public var protocolVersion: Int
        public var instanceID: String
        public init(protocolVersion: Int, instanceID: String) { self.protocolVersion = protocolVersion; self.instanceID = instanceID }
    }
    public struct GuestServiceCheck: Codable, Sendable, Equatable {
        /// "up-to-date", "update-available", or "unknown".
        public var state: String
        public var currentVersion: String?
        public var targetVersion: String?
        public var problem: String?
        public init(state: String, currentVersion: String? = nil, targetVersion: String? = nil, problem: String? = nil) {
            self.state = state; self.currentVersion = currentVersion; self.targetVersion = targetVersion; self.problem = problem
        }
        public var description: String {
            "Guest Service \(currentVersion ?? "unknown") → \(targetVersion ?? "unknown"): \(state)" + (problem.map { " (" + $0 + ")" } ?? "")
        }
    }

    public struct GuestServiceStatus: Codable, Sendable, Equatable {
        enum CodingKeys: String, CodingKey {
            case state, os, user, bootId, connectionID, problem, shares, update, busy
            case version
        }
        /// "ready", "connecting" (not answered yet; a Windows guest may need minutes), "unresponsive" (answered before, silent now:
        /// `user` and `bootId` are nil and `problem` says for how long), "incompatible" or "disabled" (vm.json `guestService` is false).
        public var state: String
        public var os: String?
        public var version: String?
        /// The signed-in console user, if any.
        public var user: String?
        public var bootId: String?
        /// Host connection generation; changes on Guest Service restart even within the same guest boot.
        public var connectionID: String?
        public var problem: String?
        /// A VZ (Linux) VM's shares as the daemon mounted them since the Guest Service became ready; nil for a VM without any.
        public var shares: [ShareMount]?
        public var update: GuestServiceCheck?
        /// glov-managed work a daemon restart would cut off ("job j3", "share sync proj"); empty when idle, nil from a
        /// daemon too old to report it.
        public var busy: [String]?
        public init(
            state: String, os: String? = nil, version: String? = nil, user: String? = nil, bootId: String? = nil, problem: String? = nil,
            shares: [ShareMount]? = nil, update: GuestServiceCheck? = nil, connectionID: String? = nil, busy: [String]? = nil
        ) {
            self.state = state; self.os = os; self.version = version; self.user = user; self.bootId = bootId; self.problem = problem
            self.shares = shares; self.update = update; self.connectionID = connectionID; self.busy = busy
        }
    }

    /// Whether the daemon has mounted one share in the guest.
    public struct ShareMount: Codable, Sendable, Equatable {
        public var name: String
        public var guestPath: String
        /// "mounting", "mounted" or "failed".
        public var state: String
        /// Why it failed.
        public var problem: String?
        public init(name: String, guestPath: String, state: String, problem: String? = nil) {
            self.name = name; self.guestPath = guestPath; self.state = state; self.problem = problem
        }
    }

    public struct VMInfo: Codable, Sendable, Equatable {
        enum CodingKeys: String, CodingKey {
            case name, state, pid, engine, installFailure, install, warning
            case agent = "guestService"
        }
        public var name: String
        /// "running" or "stopped"; for a VM `vm create` is installing Windows on, "installing" (running) until the install
        /// is done, or "install-failed".
        public var state: String
        public var pid: Int32?
        /// Only while the VM runs.
        public var agent: GuestServiceStatus?
        /// "qemu" or "vz".
        public var engine: String?
        /// Why the install failed ("install-failed").
        public var installFailure: String?
        /// While a Windows install runs ("installing"): its phase and how long it has run, of at most `limitSeconds`.
        public var install: InstallProgress?
        /// `vm.start`: started, but the Mac may run short of memory (`MemoryBudget`).
        public var warning: String?
        public init(name: String, state: String, pid: Int32? = nil, agent: GuestServiceStatus? = nil, engine: String? = nil, installFailure: String? = nil) {
            self.name = name; self.state = state; self.pid = pid; self.agent = agent; self.engine = engine; self.installFailure = installFailure
        }
    }

    public struct InstallProgress: Codable, Sendable, Equatable {
        /// "booting the installer" and "Windows Setup" are told apart by the time since the first start alone, so a VM stuck
        /// before Setup also shows "Windows Setup"; "first sign-in and setup script" is when the Guest Service answers.
        public var phase: String
        public var elapsedSeconds: Int
        public var limitSeconds: Int
        /// Past the 15 minutes a full install takes; the screen shows whether it is stuck.
        public var overdue: Bool { elapsedSeconds > 15 * 60 }
        public init(phase: String, elapsedSeconds: Int, limitSeconds: Int) {
            self.phase = phase; self.elapsedSeconds = elapsedSeconds; self.limitSeconds = limitSeconds
        }
    }

    public struct JobStarted: Codable, Sendable, Equatable {
        public var jobId: String
        public init(jobId: String) { self.jobId = jobId }
    }

    public struct ExecRun: Codable, Sendable, Equatable {
        public var jobId: String
        /// False while the job still runs (the timeout hit) and when it was lost.
        public var exited: Bool
        public var code: Int?
        /// The Guest Service reconnected and never reported this job's exit; it may still run in the guest.
        public var lost: Bool
        /// Last 50 lines of output (at most 32 KiB), stdout and stderr interleaved.
        public var tail: String
        public var lineCount: Int
        /// Full output on the host.
        public var logPath: String
        public init(jobId: String, exited: Bool, code: Int?, lost: Bool, tail: String, lineCount: Int, logPath: String) {
            self.jobId = jobId; self.exited = exited; self.code = code; self.lost = lost; self.tail = tail
            self.lineCount = lineCount; self.logPath = logPath
        }
    }

    public struct ExecRead: Codable, Sendable, Equatable {
        public var data: String
        /// Pass back as `cursor` to continue.
        public var cursor: Int
        public var exited: Bool
        public var code: Int?
        public var lost: Bool
        /// Output before the in-memory window (1 MiB) was skipped; `logPath` has all of it.
        public var dropped: Bool
        public var logPath: String
        public init(data: String, cursor: Int, exited: Bool, code: Int?, lost: Bool, dropped: Bool, logPath: String) {
            self.data = data; self.cursor = cursor; self.exited = exited; self.code = code; self.lost = lost
            self.dropped = dropped; self.logPath = logPath
        }
    }

    public struct FileRead: Codable, Sendable, Equatable {
        public var text: String
        public var size: Int
        public init(text: String, size: Int) { self.text = text; self.size = size }
    }

    public struct FileWrite: Codable, Sendable, Equatable {
        public var size: Int
        public init(size: Int) { self.size = size }
    }

    public struct Copy: Codable, Sendable, Equatable {
        public var files: Int
        public var bytes: Int
        /// Entries left out (symbolic links and other special files).
        public var skipped: [String]
        public init(files: Int, bytes: Int, skipped: [String]) { self.files = files; self.bytes = bytes; self.skipped = skipped }
    }

    public struct List: Codable, Sendable, Equatable {
        public var vms: [VMInfo]
        public init(vms: [VMInfo]) { self.vms = vms }
    }

    public struct Screenshot: Codable, Sendable {
        /// Base64 PNG.
        public var png: String
        public var width: Int, height: Int
        /// Size of the whole screen in screenshot pixels; input coordinates are relative to it.
        public var frameWidth: Int, frameHeight: Int
        public init(png: String, width: Int, height: Int, frameWidth: Int, frameHeight: Int) {
            self.png = png; self.width = width; self.height = height; self.frameWidth = frameWidth; self.frameHeight = frameHeight
        }
    }

    public struct SnapshotList: Codable, Sendable, Equatable {
        public var snapshots: [SnapshotInfo]
        public init(snapshots: [SnapshotInfo]) { self.snapshots = snapshots }
    }

    /// What one `share.sync` did. Paths are relative to the share root, `/`-separated; the path lists hold at most
    /// `listLimit` entries each, the counts are complete.
    public struct ShareSync: Codable, Sendable, Equatable {
        public static let listLimit = 100
        public var share: String
        public var guestPath: String
        public var written: Int
        public var deleted: Int
        public var unchanged: Int
        public var bytes: Int
        /// Files hashed because their stat key changed (all of them with `verify`).
        public var hostHashed: Int
        public var guestHashed: Int
        public var writtenPaths: [String]
        public var deletedPaths: [String]
        /// Files changed in the guest since the last sync that this sync replaced or deleted.
        public var overwritten: [String]
        /// Host paths not mirrored and left alone in the guest (links, special files, names Windows cannot hold).
        public var skipped: [String]
        public var seconds: Double
        public init(
            share: String, guestPath: String, written: Int = 0, deleted: Int = 0, unchanged: Int = 0, bytes: Int = 0, hostHashed: Int = 0,
            guestHashed: Int = 0, writtenPaths: [String] = [], deletedPaths: [String] = [], overwritten: [String] = [], skipped: [String] = [],
            seconds: Double = 0
        ) {
            self.share = share; self.guestPath = guestPath; self.written = written; self.deleted = deleted; self.unchanged = unchanged
            self.bytes = bytes; self.hostHashed = hostHashed; self.guestHashed = guestHashed; self.writtenPaths = writtenPaths
            self.deletedPaths = deletedPaths; self.overwritten = overwritten; self.skipped = skipped; self.seconds = seconds
        }
    }

    public struct ShareStatus: Codable, Sendable, Equatable {
        public var syncs: [SyncProgress]
        public init(syncs: [SyncProgress]) { self.syncs = syncs }
    }

    /// A share sync in progress: the host's stage, and what the guest reports for its listing or batch.
    public struct SyncProgress: Codable, Sendable, Equatable {
        public var share: String
        /// "host scan", "guest listing" or "applying".
        public var stage: String
        public var seconds: Double
        /// While applying: file bytes sent so far, of the total to send.
        public var sentBytes: Int
        public var totalBytes: Int
        /// The guest's current call, nil when it has none or its Guest Service has no `sync.status`.
        public var guest: GuestSyncProgress?
        public init(share: String, stage: String, seconds: Double, sentBytes: Int = 0, totalBytes: Int = 0, guest: GuestSyncProgress? = nil) {
            self.share = share; self.stage = stage; self.seconds = seconds; self.sentBytes = sentBytes; self.totalBytes = totalBytes; self.guest = guest
        }
    }

    /// `sync.status` (docs/guest-protocol.md): the file being hashed or written and what this call did so far.
    public struct GuestSyncProgress: Codable, Sendable, Equatable {
        public var phase: String
        public var path: String
        public var files: Int
        public var bytes: Int
        public var seconds: Double
        public init(phase: String, path: String, files: Int, bytes: Int, seconds: Double) {
            self.phase = phase; self.path = path; self.files = files; self.bytes = bytes; self.seconds = seconds
        }
    }

    public struct OK: Codable, Sendable, Equatable { public var ok = true; public init() {} }
}

public enum DaemonProtocol {
    // Public status keys and guest management methods changed; guest wire protocol stays at 1.
    public static let version = 2
}

extension JSONValue {
    public init<T: Encodable>(encoding value: T) throws {
        self = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(value))
    }

    /// Decodes into `T`, reporting a missing or mistyped field in words an AI agent can act on.
    public func decode<T: Decodable>(_ type: T.Type) throws -> T {
        do { return try JSONDecoder().decode(T.self, from: JSONEncoder().encode(self)) } catch let e as DecodingError {
            switch e {
            case .keyNotFound(let k, _): throw RPCError(code: RPCError.invalidParams, message: "missing parameter '\(k.stringValue)'")
            case .typeMismatch(_, let c), .valueNotFound(_, let c), .dataCorrupted(let c):
                let path = c.codingPath.map(\.stringValue).joined(separator: ".")
                throw RPCError(code: RPCError.invalidParams, message: "invalid parameter '\(path)': \(c.debugDescription)")
            @unknown default: throw RPCError(code: RPCError.invalidParams, message: "invalid parameters")
            }
        }
    }
}
