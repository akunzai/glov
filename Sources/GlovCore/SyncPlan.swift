/// What `share sync` does to make the guest copy of a share mirror the host folder: the host's files whose SHA-256
/// the guest lacks are written, guest files and directories the host lacks are removed, and every guest path that is
/// excluded, or lies under a host path the sync skips (`untouched`), is left alone. Paths are relative to the share
/// root, `/`-separated.
public struct SyncPlan: Equatable, Sendable {
    public struct HostFile: Equatable, Sendable {
        public var size: Int
        public var hash: String
        public init(size: Int, hash: String) { self.size = size; self.hash = hash }
    }

    public struct GuestFile: Equatable, Sendable {
        public var size: Int
        /// Empty when the guest could not read the file: it is written again.
        public var hash: String
        /// Changed in the guest since the last sync (`sync.manifest`).
        public var modified: Bool
        public init(size: Int, hash: String, modified: Bool = false) { self.size = size; self.hash = hash; self.modified = modified }
    }

    /// Guest files to delete, sorted.
    public var deletes: [String] = []
    /// Guest directories to remove when empty, deepest first.
    public var rmdirs: [String] = []
    /// Directories to create, parents first.
    public var mkdirs: [String] = []
    /// Host files to send, sorted.
    public var writes: [String] = []
    /// Files already equal on both sides.
    public var unchanged = 0
    /// Guest-modified files the plan replaces or deletes, sorted.
    public var overwritten: [String] = []

    public init(
        hostFiles: [String: HostFile], hostDirs: Set<String>, guestFiles: [String: GuestFile], guestDirs: Set<String>,
        excludes: ShareExcludes, untouched: [String] = []
    ) {
        func kept(_ path: String, isDir: Bool) -> Bool {
            excludes.excludesPathOrParent(path, isDir: isDir) || untouched.contains { path == $0 || path.hasPrefix($0 + "/") }
        }
        for (path, g) in guestFiles where !kept(path, isDir: false) {
            if let h = hostFiles[path] {
                if !g.hash.isEmpty && g.hash == h.hash {
                    unchanged += 1
                    continue
                }
            } else {
                deletes.append(path)
            }
            if g.modified { overwritten.append(path) }
        }
        for (path, h) in hostFiles where !kept(path, isDir: false) {
            if let g = guestFiles[path], !g.hash.isEmpty, g.hash == h.hash { continue }
            writes.append(path)
        }
        rmdirs = guestDirs.filter { !hostDirs.contains($0) && !kept($0, isDir: true) }
        mkdirs = hostDirs.filter { !guestDirs.contains($0) && !kept($0, isDir: true) }
        func depth(_ p: String) -> Int { p.utf8.reduce(0) { $0 + ($1 == UInt8(ascii: "/") ? 1 : 0) } }
        deletes.sort()
        writes.sort()
        overwritten.sort()
        rmdirs.sort { (depth($0), $0) > (depth($1), $1) }
        mkdirs.sort { (depth($0), $0) < (depth($1), $1) }
    }
}
