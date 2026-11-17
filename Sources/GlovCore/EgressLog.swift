import Foundation
import Synchronization

/// One connection through an isolated VM's egress proxy: metadata only, never a body or a header value.
public struct EgressRecord: Codable, Sendable, Equatable {
    public enum Decision: String, Codable, Sendable {
        case allow
        /// Refused by the policy.
        case deny
        /// Allowed, but unreachable.
        case fail
    }

    public var time: Date
    public var vm: String
    public var decision: Decision
    /// Nil for a request too malformed to name one.
    public var host: String?
    public var port: Int?
    /// Bytes to the guest and from it.
    public var bytesIn: Int
    public var bytesOut: Int
    public var reason: String?
    /// A model-API or intercepted request: its method, path (without the query) and response status.
    public var method: String?
    public var path: String?
    public var status: Int?
    /// The secrets substituted into the request (docs/networking.md "Secrets"): names only, never a value.
    public var secrets: [String]?

    public init(
        time: Date = Date(), vm: String, decision: Decision, host: String?, port: Int?, bytesIn: Int = 0, bytesOut: Int = 0, reason: String? = nil,
        method: String? = nil, path: String? = nil, status: Int? = nil, secrets: [String]? = nil
    ) {
        self.time = time
        self.vm = vm
        self.decision = decision
        self.host = host
        self.port = port
        self.bytesIn = bytesIn
        self.bytesOut = bytesOut
        self.reason = reason
        self.method = method
        self.path = path
        self.status = status
        self.secrets = secrets
    }

    enum CodingKeys: String, CodingKey {
        case time, vm, decision, host, port, reason, method, path, status, secrets
        case bytesIn = "in", bytesOut = "out"
    }

    /// `time` is an ISO 8601 UTC string, readable in the log file and over RPC whatever date strategy the coder uses.
    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let text = try c.decode(String.self, forKey: .time)
        guard let time = try? Date(text, strategy: .iso8601) else {
            throw DecodingError.dataCorruptedError(forKey: .time, in: c, debugDescription: "not an ISO 8601 date: \(text)")
        }
        self.init(
            time: time, vm: try c.decode(String.self, forKey: .vm), decision: try c.decode(Decision.self, forKey: .decision),
            host: try c.decodeIfPresent(String.self, forKey: .host), port: try c.decodeIfPresent(Int.self, forKey: .port),
            bytesIn: try c.decode(Int.self, forKey: .bytesIn), bytesOut: try c.decode(Int.self, forKey: .bytesOut),
            reason: try c.decodeIfPresent(String.self, forKey: .reason), method: try c.decodeIfPresent(String.self, forKey: .method),
            path: try c.decodeIfPresent(String.self, forKey: .path), status: try c.decodeIfPresent(Int.self, forKey: .status),
            secrets: try c.decodeIfPresent([String].self, forKey: .secrets))
    }

    public func encode(to encoder: any Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(time.formatted(.iso8601), forKey: .time)
        try c.encode(vm, forKey: .vm)
        try c.encode(decision, forKey: .decision)
        try c.encodeIfPresent(host, forKey: .host)
        try c.encodeIfPresent(port, forKey: .port)
        try c.encode(bytesIn, forKey: .bytesIn)
        try c.encode(bytesOut, forKey: .bytesOut)
        try c.encodeIfPresent(reason, forKey: .reason)
        try c.encodeIfPresent(method, forKey: .method)
        try c.encodeIfPresent(path, forKey: .path)
        try c.encodeIfPresent(status, forKey: .status)
        try c.encodeIfPresent(secrets, forKey: .secrets)
    }

    /// `host:port`, `[v6]:port`, or `-`.
    public var target: String {
        guard let host, let port else { return "-" }
        return host.contains(":") ? "[\(host)]:\(port)" : "\(host):\(port)"
    }

    /// `<decision> <target>[ <method> <path>][ <status>] in=<bytes> out=<bytes>[ secrets=<a,b>][ (<reason>)]`.
    public var summary: String {
        var text = "\(decision.rawValue) \(target)"
        if let method, let path { text += " \(method) \(path)" }
        if let status { text += " \(status)" }
        text += " in=\(bytesIn) out=\(bytesOut)"
        if let secrets, !secrets.isEmpty { text += " secrets=\(secrets.joined(separator: ","))" }
        return text + (reason.map { " (\($0))" } ?? "")
    }

    /// The daemon-log form: `egress <summary>`.
    public var line: String { "egress " + summary }
}

/// An isolated VM's egress records, `<vm>/egress.log`: JSON lines, renamed to `egress.log.1` (replacing it) past `cap`.
public final class EgressLog: Sendable {
    public static let defaultCap = 1 << 20

    public let path: String
    let cap: Int
    private let lock = Mutex(())

    public init(path: String, cap: Int = EgressLog.defaultCap) {
        self.path = path
        self.cap = cap
    }

    static let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.outputFormatting = .sortedKeys
        return e
    }()

    static let decoder = JSONDecoder()

    /// Appends one line. A failure to write is dropped: the daemon log has the same line.
    public func append(_ record: EgressRecord) {
        guard var data = try? Self.encoder.encode(record) else { return }
        data.append(0x0a)
        lock.withLock { _ in
            var st = stat()
            if stat(path, &st) == 0, Int(st.st_size) >= cap { _ = rename(path, path + ".1") }
            let fd = open(path, O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0o600)
            guard fd >= 0 else { return }
            defer { close(fd) }
            // O_APPEND puts each write at the end; a short one is finished, so lines never interleave.
            var offset = 0
            while offset < data.count {
                let n = data.withUnsafeBytes { write(fd, $0.baseAddress!.advanced(by: offset), data.count - offset) }
                if n < 0 && errno == EINTR { continue }
                guard n > 0 else { return }
                offset += n
            }
        }
    }

    public struct Page: Sendable, Equatable {
        public var records: [EgressRecord]
        /// Pass it back to read only what comes after.
        public var cursor: String
    }

    /// Records after `cursor` (all of them), or without one the last `limit`; `deniedOnly` keeps `deny` and `fail`.
    /// The cursor names the file and offset read up to, so it survives one rotation.
    public static func read(path: String, cursor: String?, deniedOnly: Bool, limit: Int) -> Page {
        var records: [EgressRecord] = []
        let current: Chunk
        if let cursor, let (inode, offset) = parse(cursor) {
            let same = lines(path, from: offset, ifInode: inode)
            if same.inode == inode {
                current = same
            } else {
                // Rotated since: finish the file the cursor was in, then read the new one from its start.
                records = lines(path + ".1", from: offset, ifInode: inode).records
                current = lines(path, from: 0)
            }
        } else {
            records = lines(path + ".1", from: 0).records
            current = lines(path, from: 0)
        }
        records += current.records
        if deniedOnly { records = records.filter { $0.decision != .allow } }
        if cursor == nil { records = Array(records.suffix(limit)) }
        return Page(records: records, cursor: "\(current.inode):\(current.end)")
    }

    struct Chunk {
        var records: [EgressRecord] = []
        /// Of the file actually read (0: none), so a rotation during the read cannot pair it with another file's offset.
        var inode: UInt64 = 0
        var end = 0
    }

    private static func parse(_ cursor: String) -> (UInt64, Int)? {
        let parts = cursor.split(separator: ":")
        guard parts.count == 2, let inode = UInt64(parts[0]), let offset = Int(parts[1]), offset >= 0 else { return nil }
        return (inode, offset)
    }

    /// The complete lines from `offset` on, and the offset after the last one (a line still being written waits). With
    /// `ifInode`, only when the open file is that one (else just its inode, offset 0, nothing read).
    private static func lines(_ path: String, from offset: Int, ifInode: UInt64? = nil) -> Chunk {
        guard let h = FileHandle(forReadingAtPath: path) else { return Chunk() }
        defer { try? h.close() }
        var st = stat()
        guard fstat(h.fileDescriptor, &st) == 0 else { return Chunk() }
        var chunk = Chunk(inode: st.st_ino, end: offset)
        guard ifInode.map({ $0 == st.st_ino }) ?? true, Int(st.st_size) >= offset else { chunk.end = 0; return chunk }
        guard (try? h.seek(toOffset: UInt64(offset))) != nil, let data = try? h.readToEnd(), let last = data.lastIndex(of: 0x0a) else { return chunk }
        let complete = data[data.startIndex...last]
        chunk.records = complete.split(separator: 0x0a).compactMap { try? decoder.decode(EgressRecord.self, from: Data($0)) }
        chunk.end = offset + complete.count
        return chunk
    }
}
