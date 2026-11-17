import CryptoKit
import Foundation
import Synchronization
import GlovCore

/// The host folder of a share as a sync sees it: its files with SHA-256, its directories, and what it leaves out.
/// Paths are relative to the share root, `/`-separated.
struct HostTree {
    var files: [String: SyncPlan.HostFile] = [:]
    var dirs: Set<String> = []
    /// Paths the sync does not mirror and leaves alone in the guest: symbolic links, special files and names Windows
    /// cannot hold.
    var skipped: [String] = []
    /// Files hashed by this scan; the others' hashes came from the cache.
    var hashed = 0

    /// Each file's stat key (size, mtime, ctime, inode) and SHA-256 from the last scan of a share.
    private struct Cache: Codable {
        struct Record: Codable { var key: String; var hash: String }
        var hostPath: String
        var files: [String: Record]
    }

    /// Scans `root/scope`, hashing only the files whose stat key differs from `cachePath`'s record (every file with
    /// `verify`), and rewrites the cache: records below `scope` are replaced by what the scan found. ctime cannot be set
    /// back by user tools and an atomic replace changes the inode, so a rewrite that keeps size and mtime is still seen.
    /// Blocking: call it off the cooperative pool.
    /// `cancelled` is checked before each entry; once it is true the scan throws `CancellationError` and saves nothing.
    static func scan(
        root: String, scope: String, excludes: ShareExcludes, cachePath: String, verify: Bool, cancelled: () -> Bool = { false }
    ) throws -> HostTree {
        var cache = (try? JSONDecoder().decode(Cache.self, from: Data(contentsOf: URL(fileURLWithPath: cachePath)))) ?? Cache(hostPath: root, files: [:])
        if cache.hostPath != root { cache = Cache(hostPath: root, files: [:]) }
        let previous = cache.files
        cache.files = previous.filter { !inScope($0.key, scope) }
        var tree = HostTree()

        // Every directory is opened relative to its parent's descriptor with O_NOFOLLOW, and every entry is examined
        // with fstatat(AT_SYMLINK_NOFOLLOW): the walk never follows a symbolic link, so nothing outside the share is
        // read, even if a directory is swapped for a link while it runs. Links are skipped and reported.
        func walk(_ fd: Int32, _ rel: String) throws {
            guard let dir = fdopendir(fd) else {
                close(fd)
                throw posixError(rel)
            }
            defer { closedir(dir) }
            while let entry = readdir(dir) {
                let name = withUnsafeBytes(of: entry.pointee.d_name) { raw in
                    String(decoding: raw.prefix(Int(entry.pointee.d_namlen)), as: UTF8.self)
                }
                if name == "." || name == ".." { continue }
                if cancelled() { throw CancellationError() }
                let path = rel.isEmpty ? name : rel + "/" + name
                var st = stat()
                guard fstatat(dirfd(dir), name, &st, AT_SYMLINK_NOFOLLOW) == 0 else { continue }  // removed while listing
                let type = st.st_mode & S_IFMT
                let isDir = type == S_IFDIR
                if excludes.excludes(path, isDir: isDir) { continue }
                guard type == S_IFDIR || type == S_IFREG, windowsName(name) else {
                    tree.skipped.append(path)
                    continue
                }
                if isDir {
                    let child = openat(dirfd(dir), name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
                    guard child >= 0 else {
                        tree.skipped.append(path)  // swapped for a link since fstatat
                        continue
                    }
                    tree.dirs.insert(path)
                    try walk(child, path)
                    continue
                }
                let key = "\(st.st_size):\(nanos(st.st_mtimespec)):\(nanos(st.st_ctimespec)):\(st.st_ino)"
                var hash = previous[path].flatMap { $0.key == key && !verify ? $0.hash : nil }
                if hash == nil {
                    let file = openat(dirfd(dir), name, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
                    guard file >= 0 else {
                        tree.skipped.append(path)
                        continue
                    }
                    hash = try sha256(fd: file, path)
                    tree.hashed += 1
                }
                tree.files[path] = .init(size: Int(st.st_size), hash: hash!)
                cache.files[path] = .init(key: key, hash: hash!)
            }
        }
        try walk(try openDirectory(root: root, scope), scope)

        try FileManager.default.createDirectory(
            atPath: (cachePath as NSString).deletingLastPathComponent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try JSONEncoder().encode(cache).write(to: URL(fileURLWithPath: cachePath), options: .atomic)
        return tree
    }

    struct OutsideShare: Error, CustomStringConvertible {
        var path: String
        var description: String { "\(path) is not a plain directory or file inside the share (a symbolic link, or '..'); nothing was synced" }
    }

    /// Opens `rel` below `root` one component at a time with O_NOFOLLOW, so no symbolic link (and no `..`) can lead
    /// out of the share; the last component is a directory when `directory`, else a regular file. Returns the descriptor.
    static func openBelow(root: String, _ rel: String, directory: Bool) throws -> Int32 {
        let parts = rel.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        if !rel.isEmpty, parts.contains(where: { $0.isEmpty || $0 == "." || $0 == ".." }) { throw OutsideShare(path: rel) }
        var fd = open(root, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
        guard fd >= 0 else { throw posixError(root) }
        for (i, part) in (rel.isEmpty ? [] : parts).enumerated() {
            let last = i == parts.count - 1
            let flags = O_RDONLY | O_NOFOLLOW | O_CLOEXEC | (last && !directory ? 0 : O_DIRECTORY)
            let next = openat(fd, part, flags)
            close(fd)
            guard next >= 0 else {
                if errno == ELOOP || errno == ENOTDIR { throw OutsideShare(path: rel) }
                throw posixError(root + "/" + rel)
            }
            fd = next
        }
        var st = stat()
        guard fstat(fd, &st) == 0, (st.st_mode & S_IFMT) == (directory ? S_IFDIR : S_IFREG) else {
            close(fd)
            throw OutsideShare(path: rel)
        }
        return fd
    }

    private static func openDirectory(root: String, _ scope: String) throws -> Int32 { try openBelow(root: root, scope, directory: true) }

    private static func inScope(_ path: String, _ scope: String) -> Bool { scope.isEmpty || path == scope || path.hasPrefix(scope + "/") }

    private static func nanos(_ t: timespec) -> Int { t.tv_sec * 1_000_000_000 + t.tv_nsec }

    private static func posixError(_ path: String) -> any Error {
        CocoaError(.fileReadUnknown, userInfo: [NSFilePathErrorKey: path, NSUnderlyingErrorKey: POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)])
    }

    /// SHA-256 of the open file `fd` (which it closes), in hex; `path` names it in errors.
    static func sha256(fd: Int32, _ path: String) throws -> String {
        defer { close(fd) }
        var hasher = SHA256()
        var buffer = [UInt8](repeating: 0, count: 1 << 20)
        while true {
            let n = buffer.withUnsafeMutableBytes { read(fd, $0.baseAddress, $0.count) }
            if n < 0 && errno == EINTR { continue }
            if n < 0 { throw posixError(path) }
            if n == 0 { break }
            buffer.withUnsafeBytes { hasher.update(bufferPointer: UnsafeRawBufferPointer(rebasing: $0.prefix(n))) }
        }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }

    private static let reservedNames = Set(["CON", "PRN", "AUX", "NUL"] + (1...9).flatMap { ["COM\($0)", "LPT\($0)"] })

    /// Whether NTFS through Win32 can hold a file of this name.
    static func windowsName(_ name: String) -> Bool {
        guard !name.unicodeScalars.contains(where: { $0.value < 0x20 || "<>:\"\\|?*".unicodeScalars.contains($0) }),
            !name.hasSuffix("."), !name.hasSuffix(" ")
        else { return false }
        let stem = name.split(separator: ".", maxSplits: 1).first.map { String($0).uppercased() } ?? ""
        return !reservedNames.contains(stem.trimmingCharacters(in: .whitespaces))
    }
}

/// Set from a task's cancellation handler, read by work on a thread of its own (`Blocking.run`), which cannot see the task.
final class CancelFlag: Sendable {
    private let flag = Atomic(false)
    var isSet: Bool { flag.load(ordering: .relaxed) }
    func set() { flag.store(true, ordering: .relaxed) }
}
