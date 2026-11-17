import CryptoKit
import Foundation
import GlovCore

@testable import GlovCore
@testable import GlovDaemonAPI

/// A Guest Service in this process: listens on a unix socket the way QEMU's chardev does and speaks
/// docs/guest-protocol.md (length-prefixed JSON-RPC), with an in-memory file system and scripted processes.
final class FakeGuestService: @unchecked Sendable {
    enum Step {
        case out(String, String)  // stream, text
        case pause(Double)
        case exit(Int)
        case hold  // keep running until exec.kill
    }

    let path: String
    private let listener: UnixSocket.Listener
    private let lock = NSLock()
    private var connections: [Int32] = []
    private var _accepted = 0
    private var _requests: [(method: String, params: JSONValue)] = []
    private var _files: [String: Data] = [:]
    private var _dirs: Set<String> = []
    private var _stdin: [Int: Data] = [:]
    /// Processes replacing a file from their stdin (`sh -c '... cat > "$1.glov~"; mv ...' sh <path> <mode>`), by pid.
    private var _writers: [Int: String] = [:]
    private var nextPid = 100
    /// Every write goes out on this serial queue, in call order: tests call `notify` from async code, where a blocking
    /// write would hold a cooperative thread, and the Guest Service's own threads must not overtake those writes.
    private let writer = DispatchQueue(label: "FakeGuestService.writer")

    var os = "windows"
    var protocolVersion = 1
    var bootId = "boot-1"
    /// `secrets.sync` answers error 1.
    var secretsSyncFails = false
    var sendReadyOnConnect = true
    /// False makes the Guest Service hang: requests get no answer (notifications sent through `notify` still go out).
    var answering: Bool { get { lock.withLock { _answering } } set { lock.withLock { _answering = newValue } } }
    private var _answering = true
    /// Sends the process's first output chunk before the exec.start response, as a racing agent might.
    var outputBeforeStartResponse = false
    var script: @Sendable ([String]) -> [Step] = { _ in [.exit(0)] }

    var sep: String { os == "windows" ? "\\" : "/" }
    var accepted: Int { lock.withLock { _accepted } }
    var requests: [(method: String, params: JSONValue)] { lock.withLock { _requests } }
    func requests(_ method: String) -> [JSONValue] { requests.filter { $0.method == method }.map(\.params) }
    var files: [String: Data] { get { lock.withLock { _files } } set { lock.withLock { _files = newValue } } }
    var dirs: Set<String> { get { lock.withLock { _dirs } } set { lock.withLock { _dirs = newValue } } }
    func stdin(_ pid: Int) -> Data { lock.withLock { _stdin[pid] ?? Data() } }

    init(socketPath: String) throws {
        // SO_NOSIGPIPE cannot be set on an accepted socket whose peer already left, and a write to it would kill the tests.
        signal(SIGPIPE, SIG_IGN)
        path = socketPath
        var addr = UnixSocket.address(path)!
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        let rc = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        guard rc == 0, listen(fd, 4) == 0 else { close(fd); throw CocoaError(.fileWriteUnknown) }
        listener = UnixSocket.Listener(fd: fd)
        listener.start { [self] fd in
            var one: Int32 = 1
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
            lock.withLock { connections.append(fd); _accepted += 1 }
            Thread.detachNewThread { [self] in serve(fd) }
        }
    }

    func stop() {
        listener.stop()
        dropConnection()
        unlink(path)
    }

    /// Closes the current connection; the listener stays, so the host can reconnect.
    func dropConnection() {
        // Under the lock: serve() removes and closes its fd under it too, so a closed fd number is never touched.
        lock.withLock { for fd in connections { shutdown(fd, SHUT_RDWR) } }
    }

    var info: JSONValue {
        ["protocolVersion": .int(protocolVersion), "version": "0.0-fake", "os": .string(os), "arch": "arm64", "hostname": "FAKE",
         "user": user.map { .string($0) } ?? .null, "bootId": .string(bootId)]
    }
    var user: String? = "tester"

    func sendReady() { notify("guest.ready", info) }

    /// Writes bytes as they are, to break the framing on purpose.
    func rawWrite(_ data: Data) { write(nil, data) }

    /// Writes to `fd`, or to the newest connection, unless it has gone. Under the lock: serve() removes and closes
    /// its fd under it too, so a closed fd number is never written.
    private func write(_ fd: Int32?, _ data: Data) {
        writer.async { [self] in
            lock.withLock {
                guard let fd = fd ?? connections.last, connections.contains(fd) else { return }
                _ = UnixSocket.write(fd: fd, data)
            }
        }
    }

    func notify(_ method: String, _ params: JSONValue) {
        rawWrite(frame(["jsonrpc": "2.0", "method": .string(method), "params": params]))
    }

    private func frame(_ message: JSONValue) -> Data {
        let payload = (try? JSONEncoder().encode(message)) ?? Data()
        var n = UInt32(payload.count).bigEndian
        return Data(bytes: &n, count: 4) + payload
    }

    private func respond(_ fd: Int32, _ id: JSONValue, result: JSONValue = [:], error: (Int, String)? = nil) {
        var m: [String: JSONValue] = ["jsonrpc": "2.0", "id": id]
        if let error { m["error"] = ["code": .int(error.0), "message": .string(error.1)] } else { m["result"] = result }
        write(fd, frame(.object(m)))
    }

    private func readExactly(_ fd: Int32, _ n: Int) -> [UInt8]? {
        var out = [UInt8](repeating: 0, count: n)
        var got = 0
        while got < n {
            let r = out.withUnsafeMutableBytes { read(fd, $0.baseAddress! + got, n - got) }
            if r < 0 && errno == EINTR { continue }
            if r <= 0 { return nil }
            got += r
        }
        return out
    }

    private func serve(_ fd: Int32) {
        if sendReadyOnConnect { sendReady() }
        while let header = readExactly(fd, 4) {
            let length = header.reduce(0) { Int($0) << 8 | Int($1) }
            guard let body = readExactly(fd, length), let msg = try? JSONDecoder().decode(JSONValue.self, from: Data(body)),
                case .string(let method)? = msg["method"], let id = msg["id"]
            else { break }
            let params = msg["params"] ?? [:]
            let answer = lock.withLock { _requests.append((method, params)); return _answering }
            if answer { handle(fd, id, method, params) }
        }
        lock.withLock { connections.removeAll { $0 == fd }; close(fd) }
    }

    private func str(_ v: JSONValue?) -> String { if case .string(let s)? = v { s } else { "" } }
    private func int(_ v: JSONValue?) -> Int { if case .int(let i)? = v { i } else { 0 } }

    private func handle(_ fd: Int32, _ id: JSONValue, _ method: String, _ p: JSONValue) {
        switch method {
        case "guest.info": respond(fd, id, result: info)
        case "test.fail": respond(fd, id, error: (3, "denied"))
        case "test.silent": break
        case "test.delay":
            let ms = int(p["ms"]), value = p["value"] ?? .null
            Thread.detachNewThread { [self] in
                Thread.sleep(forTimeInterval: Double(ms) / 1000)
                respond(fd, id, result: ["value": value])
            }
        case "time.sync": respond(fd, id)
        case "secrets.sync" where os == "linux" && secretsSyncFails: respond(fd, id, error: (1, "mount a tmpfs on /run/glov/secrets: denied"))
        case "secrets.sync" where os == "linux":
            var names: [JSONValue] = []
            if case .array(let list)? = p["secrets"] { names = list.compactMap { $0["name"] } }
            respond(fd, id, result: ["names": .array(names)])
        case "power.reboot" where os == "windows":
            respond(fd, id)
            if let reboot = onReboot { Thread.detachNewThread { [self] in reboot(self) } }
        case "guest.stats" where statsError != nil: respond(fd, id, error: (1, statsError ?? ""))
        case "guest.stats" where supportsStats: respond(fd, id, result: Self.stats)
        case "sync.manifest" where supportsSync && holdManifest: break
        case "sync.manifest" where supportsSync: syncManifest(fd, id, p)
        case "sync.cancel" where supportsSync: respond(fd, id)
        case "sync.status" where supportsSync && syncStatus != nil: respond(fd, id, result: syncStatus!)
        case "sync.apply" where supportsSync: syncApply(fd, id, p)
        case "exec.start": startProcess(fd, id, p)
        case "exec.stdin":
            let pid = int(p["pid"])
            let path: String? = lock.withLock {
                _stdin[pid, default: Data()].append(Data(base64Encoded: str(p["data"])) ?? Data())
                guard p["eof"] == .bool(true), let path = _writers.removeValue(forKey: pid) else { return nil }
                _files[path] = _stdin[pid]
                return path
            }
            respond(fd, id)
            if path != nil { notify("exec.exit", ["pid": .int(pid), "code": 0]) }
        case "exec.kill":
            let pid = int(p["pid"])
            respond(fd, id)
            notify("exec.exit", ["pid": .int(pid), "code": 137])
        case "file.stat":
            let path = str(p["path"])
            let (file, isDir) = lock.withLock { (_files[path], _dirs.contains(path)) }
            respond(fd, id, result: ["exists": .bool(file != nil || isDir), "isDir": .bool(isDir), "size": .int(file?.count ?? 0), "mtime": "2026-01-01T00:00:00Z"])
        case "file.list":
            let path = str(p["path"])
            let prefix = path.hasSuffix(sep) ? path : path + sep
            let (files, dirs) = lock.withLock { (_files, _dirs) }
            var entries: [JSONValue] = []
            for (k, v) in files.sorted(by: { $0.key < $1.key }) where k.hasPrefix(prefix) && !k.dropFirst(prefix.count).contains(sep) {
                entries.append(["name": .string(String(k.dropFirst(prefix.count))), "isDir": false, "size": .int(v.count), "mtime": "2026-01-01T00:00:00Z"])
            }
            for k in dirs.sorted() where k.hasPrefix(prefix) && !k.dropFirst(prefix.count).contains(sep) {
                entries.append(["name": .string(String(k.dropFirst(prefix.count))), "isDir": true, "size": 0, "mtime": "2026-01-01T00:00:00Z"])
            }
            // A hostile guest can name entries anything.
            entries += hostileEntries
            respond(fd, id, result: ["entries": .array(entries)])
        case "file.read":
            if int(p["offset"]) == droppedFileReadOffset, droppedFileReads > 0 { droppedFileReads -= 1; break }
            if int(p["offset"]) == failedFileReadOffset {
                respond(fd, id, error: (3, "read denied"))
                break
            }
            let path = str(p["path"]), offset = int(p["offset"]), length = int(p["length"])
            guard length <= 524_288 else { return respond(fd, id, error: (-32602, "length over 524288")) }
            guard let data = lock.withLock({ _files[path] }) else { return respond(fd, id, error: (2, "not found")) }
            let slice = data.dropFirst(offset).prefix(length)
            respond(fd, id, result: ["data": .string(Data(slice).base64EncodedString()), "eof": .bool(offset + slice.count >= data.count)])
        case "file.write":
            let path = str(p["path"]), offset = int(p["offset"])
            let chunk = Data(base64Encoded: str(p["data"])) ?? Data()
            let size: Int = lock.withLock {
                var d = _files[path] ?? Data()
                if case .bool(true)? = p["truncate"] { d = Data() }
                d.replaceSubrange(offset..<min(d.count, offset + chunk.count), with: chunk)
                _files[path] = d
                return d.count
            }
            respond(fd, id, result: ["size": .int(size)])
        default: respond(fd, id, error: (-32601, "method not found"))
        }
    }

    /// What follows power.reboot's answer, on its own thread; nil leaves the guest running its old boot.
    var onReboot: (@Sendable (FakeGuestService) -> Void)?

    /// A Windows restart after power.reboot: the old boot keeps answering for `goingDown` seconds, then stops answering
    /// (shutting down) for `down` seconds, and the new boot's Guest Service announces itself with `bootId`.
    func rebootsAfterAnswering(to bootId: String, goingDown: Double = 0.3, down: Double = 0.3) {
        onReboot = { fake in
            Thread.sleep(forTimeInterval: goingDown)
            fake.answering = false
            Thread.sleep(forTimeInterval: down)
            fake.bootId = bootId
            fake.answering = true
            fake.sendReady()
        }
    }

    /// False answers guest.stats with "method not found", like a Guest Service from before it.
    var supportsStats = true
    /// Answers guest.stats with this error message instead.
    var statsError: String?
    static let stats: JSONValue = [
        "memoryTotal": .int(8 << 30), "memoryAvailable": .int(6 << 30), "diskPath": "C:\\", "diskTotal": .int(200 << 30),
        "diskUsed": .int(40 << 30), "diskAvailable": .int(160 << 30), "load": .null, "cpuBusy": .double(12.5),
    ]

    /// File read replies to discard, simulating a transient timeout with a live channel.
    var droppedFileReads = 0
    var droppedFileReadOffset = 0
    var failedFileReadOffset: Int?

    var hostileEntries: [JSONValue] = []

    // MARK: share sync, over `files` and `dirs` (full guest paths, `sep`-separated)

    /// False answers sync.* with "method not found", like a Guest Service from before share sync.
    var supportsSync = true
    /// True leaves sync.manifest unanswered, like a listing stuck on a file in the guest.
    var holdManifest: Bool { get { lock.withLock { _holdManifest } } set { lock.withLock { _holdManifest = newValue } } }
    private var _holdManifest = false
    /// The sync.status answer; nil answers "method not found", like a Guest Service from before sync.status.
    var syncStatus: JSONValue? { get { lock.withLock { _syncStatus } } set { lock.withLock { _syncStatus = newValue } } }
    private var _syncStatus: JSONValue?
    /// Full guest paths another guest process holds open: writes and deletes of them fail as locked.
    var lockedPaths: Set<String> = []
    /// Full guest paths whose writes and deletes fail with access denied; true also sets locked, as Windows reports a
    /// held file it cannot replace.
    var deniedPaths: [String: Bool] = [:]
    /// Hash recorded per full guest path by the last committed sync (the guest manifest).
    private var syncRecords: [String: String]?
    private var pendingSync: [String: Data] = [:]

    private func refusal(_ path: String, _ rel: String) -> JSONValue? {
        if lockedPaths.contains(path) { return ["path": .string(rel), "message": "in use", "locked": true] }
        if let alsoLocked = deniedPaths[path] { return ["path": .string(rel), "message": "denied", "locked": .bool(alsoLocked), "denied": true] }
        return nil
    }
    private var manifestPages: [JSONValue] = []

    static func sha(_ d: Data) -> String { SHA256.hash(data: d).map { String(format: "%02x", $0) }.joined() }

    private func guestPath(_ root: String, _ rel: String) -> String {
        rel.isEmpty ? root : root + sep + rel.replacingOccurrences(of: "/", with: sep)
    }

    private func syncManifest(_ fd: Int32, _ id: JSONValue, _ p: JSONValue) {
        let root = str(p["root"]), subdir = str(p["subdir"]), offset = int(p["offset"])
        var rules: [(NSRegularExpression, Bool, Bool)] = []
        if case .array(let list)? = p["exclude"] {
            for r in list { rules.append((try! NSRegularExpression(pattern: str(r["pattern"])), r["negate"] == true, r["dirOnly"] == true)) }
        }
        func excluded(_ rel: String, _ dir: Bool) -> Bool {
            var out = false
            for (re, neg, dirOnly) in rules where !dirOnly || dir {
                if re.firstMatch(in: rel, range: NSRange(rel.startIndex..., in: rel)) != nil { out = !neg }
            }
            return out
        }
        if offset > 0 {
            let page = Array(manifestPages.dropFirst(offset).prefix(2))
            return respond(fd, id, result: ["entries": .array(page), "total": .int(manifestPages.count), "hashed": 0, "fresh": false])
        }
        let prefix = guestPath(root, subdir) + sep
        let (files, dirs, records) = lock.withLock { (_files, _dirs, syncRecords) }
        func rel(_ full: String) -> String { String(full.dropFirst(root.count + sep.count)).replacingOccurrences(of: sep, with: "/") }
        func hidden(_ r: String, _ dir: Bool) -> Bool {
            let parts = r.split(separator: "/")
            return parts.indices.contains { i in excluded(parts[...i].joined(separator: "/"), i < parts.count - 1 || dir) }
        }
        var entries: [JSONValue] = []
        for d in dirs.sorted() where d.hasPrefix(prefix) && !hidden(rel(d), true) { entries.append(["path": .string(rel(d)), "dir": true, "size": 0, "hash": ""]) }
        for (f, data) in files.sorted(by: { $0.key < $1.key }) where f.hasPrefix(prefix) && !hidden(rel(f), false) {
            let h = Self.sha(data)
            let modified = records != nil && records![f] != h
            entries.append(["path": .string(rel(f)), "size": .int(data.count), "hash": .string(h), "modified": .bool(modified)])
        }
        manifestPages = entries
        // Pages of two entries, so the host's paging is exercised.
        respond(fd, id, result: ["entries": .array(Array(entries.prefix(2))), "total": .int(entries.count), "hashed": .int(files.count), "fresh": .bool(records == nil)])
    }

    private func syncApply(_ fd: Int32, _ id: JSONValue, _ p: JSONValue) {
        let root = str(p["root"])
        var failures: [JSONValue] = []
        lock.withLock {
            guard case .array(let ops)? = p["ops"] else { return }
            if syncRecords == nil { syncRecords = [:] }
            for op in ops {
                let rel = str(op["path"]), path = guestPath(root, rel)
                switch str(op["op"]) {
                case "delete":
                    if let f = refusal(path, rel) { failures.append(f); continue }
                    _files[path] = nil
                    syncRecords![path] = nil
                case "rmdir":
                    if !_files.keys.contains(where: { $0.hasPrefix(path + sep) }) && !_dirs.contains(where: { $0.hasPrefix(path + sep) }) { _dirs.remove(path) }
                case "mkdir":
                    let parts = rel.split(separator: "/").map(String.init)
                    for i in parts.indices { _dirs.insert(guestPath(root, parts[...i].joined(separator: "/"))) }
                case "write":
                    let chunk = Data(base64Encoded: str(op["data"])) ?? Data()
                    if int(op["offset"]) == 0 { pendingSync[path] = Data() }
                    pendingSync[path, default: Data()].append(chunk)
                    guard op["end"] == true, let whole = pendingSync.removeValue(forKey: path) else { continue }
                    if let f = refusal(path, rel) { failures.append(f); continue }
                    guard Self.sha(whole) == str(op["hash"]) else { failures.append(["path": .string(rel), "message": "hash", "locked": false]); continue }
                    _files[path] = whole
                    syncRecords![path] = Self.sha(whole)
                default: break
                }
            }
            if p["commit"] == true {
                for (f, d) in _files where f.hasPrefix(root + sep) { syncRecords![f] = Self.sha(d) }
            }
        }
        respond(fd, id, result: ["failures": .array(failures)])
    }

    private func startProcess(_ fd: Int32, _ id: JSONValue, _ p: JSONValue) {
        if p["as"] == "user" && user == nil { return respond(fd, id, error: (4, "no user is signed in on the console")) }
        var argv: [String] = []
        if case .array(let a)? = p["argv"] { argv = a.map { str($0) } }
        let pid = lock.withLock { let n = nextPid; nextPid += 1; return n }
        // `runuser -u <user> -- sh -c ...` writes as that user.
        let writer = argv.first == "runuser" && argv.count > 4 ? Array(argv.dropFirst(4)) : argv
        if writer.count >= 5, writer[0] == "sh", writer[2].contains(#"cat > "$1.glov~""#) {
            lock.withLock { _writers[pid] = writer[4] }
            return respond(fd, id, result: ["pid": .int(pid)])
        }
        let steps = script(argv)
        if outputBeforeStartResponse, case .out(let stream, let text)? = steps.first {
            notify("exec.output", ["pid": .int(pid), "stream": .string(stream), "data": .string(Data(text.utf8).base64EncodedString())])
        }
        respond(fd, id, result: ["pid": .int(pid)])
        Thread.detachNewThread { [self] in
            for (i, step) in steps.enumerated() {
                switch step {
                case .out(let stream, let text):
                    if outputBeforeStartResponse && i == 0 { continue }
                    notify("exec.output", ["pid": .int(pid), "stream": .string(stream), "data": .string(Data(text.utf8).base64EncodedString())])
                case .pause(let s): Thread.sleep(forTimeInterval: s)
                case .exit(let code): notify("exec.exit", ["pid": .int(pid), "code": .int(code)])
                case .hold: return
                }
            }
        }
    }
}
