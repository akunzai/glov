import Foundation
import GlovCore
import GlovDaemonAPI

/// Which host paths `copy.toVM` / `copy.fromVM` may touch: those that, after resolving symbolic links,
/// lie inside one of the allowlisted directories.
struct HostPathPolicy {
    let roots: [String]

    /// Creates missing allowlist directories (they are the defaults on a fresh VM) and resolves them. `shares` are the
    /// host folders of shares, which are never created: a missing one is simply not allowed.
    init(allowlist: [String], shares: [String] = []) {
        roots = allowlist.compactMap { entry in
            let path = (entry as NSString).expandingTildeInPath
            guard path.hasPrefix("/") else { return nil }
            try? FileManager.default.createDirectory(atPath: path, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            return HostPath.realpath(path)
        } + shares.compactMap { $0.hasPrefix("/") ? HostPath.realpath($0) : nil }
    }

    /// The directories a copy may touch on `config`'s host: every share's folder for reads, `rw` ones for writes.
    init(_ config: VMConfig, hostWrites: Bool) {
        let a = config.copyAllowlist(hostWrites: hostWrites)
        self.init(allowlist: a.configured, shares: a.shares)
    }

    /// The canonical form of `path` (which need not exist yet), or an error naming the allowlist.
    func resolve(_ raw: String) throws -> String {
        let path = (raw as NSString).expandingTildeInPath
        guard path.hasPrefix("/") else { throw refusal("host path \(String(reflecting: raw)) must be absolute") }
        var existing = path
        var tail: [String] = []
        while HostPath.realpath(existing) == nil, existing != "/" {
            tail.insert((existing as NSString).lastPathComponent, at: 0)
            existing = (existing as NSString).deletingLastPathComponent
        }
        // A ".." after a missing directory cannot be resolved without trusting the text.
        guard !tail.contains("..") else { throw refusal("host path \(String(reflecting: raw)) contains '..' below a directory that does not exist") }
        let resolved = ([HostPath.realpath(existing) ?? "/"] + tail).joined(separator: "/").replacingOccurrences(of: "//", with: "/")
        guard roots.contains(where: { resolved == $0 || resolved.hasPrefix($0 + "/") }) else {
            throw refusal("host path \(String(reflecting: raw)) is outside the allowlist (\(roots.joined(separator: ", "))); put the file in one of those directories")
        }
        return resolved
    }

    private func refusal(_ m: String) -> RPCError { RPCError(code: RPCError.failed, message: m) }
}

extension VMGuestService {
    static let chunk = 512 * 1024
    static let maxReadFile = 256 * 1024
    private static let maxDepth = 32

    private var separator: String { get async { await channel.info?.os == "windows" ? "\\" : "/" } }

    private func join(_ base: String, _ name: String) async -> String {
        let sep = await separator
        return base.hasSuffix(sep) || base.hasSuffix("/") ? base + name : base + sep + name
    }

    private func notFound(_ error: any Error) -> Bool {
        if case GuestServiceError.remote(let code, _) = error { return code == 2 }
        return false
    }

    private func stat(_ path: String) async throws -> (exists: Bool, isDir: Bool, size: Int) {
        do {
            let r = try await channel.request("file.stat", ["path": .string(path)])
            guard case .bool(true)? = r["exists"] else { return (false, false, 0) }
            var size = 0
            if case .int(let n)? = r["size"] { size = n }
            return (true, r["isDir"] == .bool(true), size)
        } catch { if notFound(error) { return (false, false, 0) }; throw error }
    }

    private func readChunk(_ path: String, offset: Int) async throws -> (Data, eof: Bool) {
        var retried = false
        while true {
            do {
                let r = try await channel.request("file.read", ["path": .string(path), "offset": .int(offset), "length": .int(Self.chunk)])
                guard case .string(let b64)? = r["data"], let data = Data(base64Encoded: b64) else {
                    throw GuestServiceError.protocolViolation("file.read returned no data")
                }
                return (data, r["eof"] == .bool(true) || data.isEmpty)
            } catch {
                // Reads are safe to repeat at the same offset; never retry a write whose reply was lost.
                if case GuestServiceError.timeout = error, !retried {
                    retried = true
                    continue
                }
                throw RPCError(code: RPCError.failed, message: "cannot read \(path) at byte offset \(offset): \(error)")
            }
        }
    }

    private func writeChunk(_ path: String, offset: Int, _ data: Data) async throws {
        var params: [String: JSONValue] = ["path": .string(path), "offset": .int(offset), "data": .string(data.base64EncodedString())]
        if offset == 0 { params["truncate"] = true; params["mkdirs"] = true }
        _ = try await channel.request("file.write", .object(params))
    }

    // MARK: text files

    func fileRead(_ p: Params.FileRead) async throws -> Results.FileRead {
        let limit = min(p.maxBytes ?? Self.maxReadFile, Self.maxReadFile)
        let info = try await stat(p.path)
        guard info.exists else { throw RPCError(code: RPCError.failed, message: "no such file in the guest: \(p.path)") }
        guard !info.isDir else { throw RPCError(code: RPCError.failed, message: "\(p.path) is a directory in the guest; use copy_from_vm for directories") }
        guard info.size <= limit else {
            throw RPCError(code: RPCError.failed, message: "\(p.path) is \(info.size) bytes, over the \(limit) byte limit for file_read; use copy_from_vm to fetch it to the host")
        }
        var data = Data()
        while true {
            let (part, eof) = try await readChunk(p.path, offset: data.count)
            data += part
            if eof { break }
        }
        guard let text = String(data: data, encoding: .utf8) else {
            throw RPCError(code: RPCError.failed, message: "\(p.path) is not valid UTF-8 text; use copy_from_vm to fetch it to the host")
        }
        return .init(text: text, size: data.count)
    }

    func fileWrite(_ p: Params.FileWrite) async throws -> Results.FileWrite {
        let data = Data(p.text.utf8)
        try await push(data: data, to: p.path)
        return .init(size: data.count)
    }

    private func push(data: Data, to path: String) async throws {
        var offset = 0
        repeat {
            let part = data.dropFirst(offset).prefix(Self.chunk)
            try await writeChunk(path, offset: offset, Data(part))
            offset += part.count
        } while offset < data.count
    }

    // MARK: copy

    /// `current` is the VM's vm.json as it is now, so a share added while the VM runs counts at once.
    func copyToVM(_ p: Params.CopyToVM, current: VMConfig) async throws -> Results.Copy {
        let policy = HostPathPolicy(current, hostWrites: false)
        let root = try policy.resolve(p.hostPath)
        guard FileManager.default.fileExists(atPath: root) else {
            throw RPCError(code: RPCError.failed, message: "no such host path: \(p.hostPath)")
        }
        var summary = Results.Copy(files: 0, bytes: 0, skipped: [])
        // shortcut: empty directories are not recreated (the protocol has no mkdir), add one if it matters.
        func send(_ host: String, _ guest: String, depth: Int) async throws {
            guard depth <= Self.maxDepth else { throw RPCError(code: RPCError.failed, message: "directory tree deeper than \(Self.maxDepth) levels at \(host)") }
            let type = try FileManager.default.attributesOfItem(atPath: host)[.type] as? FileAttributeType
            switch type {
            case .typeDirectory:
                for name in try FileManager.default.contentsOfDirectory(atPath: host).sorted() {
                    try await send(host + "/" + name, await join(guest, name), depth: depth + 1)
                }
            case .typeRegular:
                // Re-resolve per file: the tree may have changed since the root was checked.
                _ = try policy.resolve(host)
                let handle = try FileHandle(forReadingFrom: URL(fileURLWithPath: host))
                defer { try? handle.close() }
                var offset = 0
                repeat {
                    let part = try handle.read(upToCount: Self.chunk) ?? Data()
                    try await writeChunk(guest, offset: offset, part)
                    offset += part.count
                    if part.isEmpty { break }
                } while true
                summary.files += 1
                summary.bytes += offset
            default:
                summary.skipped.append(host)
            }
        }
        try await send(root, p.guestPath, depth: 0)
        return summary
    }

    func copyFromVM(_ p: Params.CopyFromVM, current: VMConfig) async throws -> Results.Copy {
        let policy = HostPathPolicy(current, hostWrites: true)
        var dest = try policy.resolve(p.hostPath)
        let source = try await stat(p.guestPath)
        guard source.exists else { throw RPCError(code: RPCError.failed, message: "no such path in the guest: \(p.guestPath)") }
        var summary = Results.Copy(files: 0, bytes: 0, skipped: [])
        let fm = FileManager.default

        func fetch(_ guest: String, _ host: String) async throws {
            let target = try policy.resolve(host)
            var existing = ObjCBool(false)
            if fm.fileExists(atPath: target, isDirectory: &existing), existing.boolValue {
                throw RPCError(code: RPCError.failed, message: "\(host) is a directory on the host")
            }
            try fm.createDirectory(atPath: (target as NSString).deletingLastPathComponent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            // Keep the destination intact until every chunk arrives; stage on the same filesystem for rename.
            var template = Array(((target as NSString).deletingLastPathComponent + "/.glov-copy-XXXXXX").utf8CString)
            let fd = mkstemp(&template)
            guard fd >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
            let staging = String(decoding: template.dropLast().map { UInt8(bitPattern: $0) }, as: UTF8.self)
            let handle = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
            defer {
                try? handle.close()
                try? fm.removeItem(atPath: staging)
            }
            var offset = 0
            while true {
                let (part, eof) = try await readChunk(guest, offset: offset)
                try handle.write(contentsOf: part)
                offset += part.count
                if eof { break }
            }
            try handle.close()
            _ = try policy.resolve(host)
            guard rename(staging, target) == 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
            summary.files += 1
            summary.bytes += offset
        }

        func fetchTree(_ guest: String, _ host: String, depth: Int) async throws {
            guard depth <= Self.maxDepth else { throw RPCError(code: RPCError.failed, message: "directory tree deeper than \(Self.maxDepth) levels at \(guest)") }
            try fm.createDirectory(atPath: try policy.resolve(host), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let listing = try await channel.request("file.list", ["path": .string(guest)])
            guard case .array(let entries)? = listing["entries"] else { throw GuestServiceError.protocolViolation("file.list returned no entries") }
            for entry in entries {
                guard case .string(let name)? = entry["name"] else { continue }
                // The names come from the guest: one that is a path would write outside the destination.
                guard !name.isEmpty, name != ".", name != "..", !name.contains(where: { "/\\:\0".contains($0) }) else {
                    summary.skipped.append(guest + ": " + String(reflecting: name))
                    continue
                }
                if entry["isDir"] == .bool(true) {
                    try await fetchTree(await join(guest, name), host + "/" + name, depth: depth + 1)
                } else {
                    try await fetch(await join(guest, name), host + "/" + name)
                }
            }
        }

        if source.isDir {
            try await fetchTree(p.guestPath, dest, depth: 0)
        } else {
            var isDir = ObjCBool(false)
            if fm.fileExists(atPath: dest, isDirectory: &isDir), isDir.boolValue {
                dest += "/" + (p.guestPath.split(whereSeparator: { "/\\".contains($0) }).last.map(String.init) ?? "file")
            }
            try await fetch(p.guestPath, dest)
        }
        return summary
    }
}
