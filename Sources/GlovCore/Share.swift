import Foundation

extension VMConfig {
    /// A host folder the guest sees at `guestPath`. QEMU (Windows) guests receive a copy through an explicit one-way sync
    /// (`glov share sync`): the host folder is mirrored into `guestPath`, leaving the paths `exclude` matches alone.
    /// VZ (Linux) guests get a live virtiofs mount of the host folder, mounted by the daemon once the Guest Service is ready
    /// (`ShareMount`). The shape is the same for both engines; only QEMU shares carry `exclude`.
    public struct Share: Codable, Sendable, Equatable {
        public enum Mode: String, Codable, Sendable, CaseIterable {
            /// `copy_to_vm` may read the host folder; nothing writes to it.
            case ro
            /// `copy_from_vm` may write to the host folder too.
            case rw
        }

        public var name: String
        /// Absolute host directory.
        public var hostPath: String
        public var guestPath: String
        public var mode: Mode
        /// Gitignore patterns, relative to the share root (`ShareExcludes`).
        public var exclude: [String]

        public init(name: String, hostPath: String, guestPath: String, mode: Mode = .rw, exclude: [String] = []) {
            self.name = name
            self.hostPath = hostPath
            self.guestPath = guestPath
            self.mode = mode
            self.exclude = exclude
        }

        enum CodingKeys: String, CodingKey { case name, hostPath, guestPath, mode, exclude }

        public init(from decoder: any Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            name = try c.decode(String.self, forKey: .name)
            hostPath = try c.decode(String.self, forKey: .hostPath)
            guestPath = try c.decode(String.self, forKey: .guestPath)
            mode = try c.decodeIfPresent(Mode.self, forKey: .mode) ?? .rw
            exclude = try c.decodeIfPresent([String].self, forKey: .exclude) ?? []
        }

        public func encode(to encoder: any Encoder) throws {
            var c = encoder.container(keyedBy: CodingKeys.self)
            try c.encode(name, forKey: .name)
            try c.encode(hostPath, forKey: .hostPath)
            try c.encode(guestPath, forKey: .guestPath)
            try c.encode(mode, forKey: .mode)
            if !exclude.isEmpty { try c.encode(exclude, forKey: .exclude) }
        }

        /// Letters, digits, `.`, `_` and `-`, not starting with `.`: the name is a file name on both sides and
        /// `share sync` reads `<name>/<subdir>`.
        public static func isValid(name: String) -> Bool {
            !name.isEmpty && !name.hasPrefix(".") && name.utf8.allSatisfy { c in
                (c >= 0x30 && c <= 0x39) || (c >= 0x41 && c <= 0x5a) || (c >= 0x61 && c <= 0x7a) || c == 0x2e || c == 0x5f || c == 0x2d
            }
        }

        /// Top-level Windows folders a sync must never mirror into: it deletes what the host folder lacks.
        private static let systemFolders = ["windows", "program files", "program files (x86)", "programdata"]

        /// `path` as `X:\dir\...`, or why it cannot be a share's guest path: it must be an absolute path on a drive,
        /// below the drive root and outside Windows' own folders and the user profiles themselves.
        public static func windowsGuestPath(_ path: String) throws -> String {
            let parts = path.replacingOccurrences(of: "/", with: "\\").split(separator: "\\").map(String.init)
            guard let drive = parts.first, drive.count == 2, drive.last == ":", drive.first!.isASCII, drive.first!.isLetter,
                path.count > 2, "\\/".contains(path[path.index(path.startIndex, offsetBy: 2)])
            else { throw ShareError.invalid("guest path \(String(reflecting: path)) must be an absolute Windows path such as C:\\work\\proj") }
            let dirs = Array(parts.dropFirst())
            guard !dirs.isEmpty else { throw ShareError.invalid("guest path \(path) is a drive root: a sync deletes what the host folder lacks, so use a folder such as \(drive)\\work") }
            guard !dirs.contains(where: { $0 == "." || $0 == ".." || $0.contains(":") }) else {
                throw ShareError.invalid("guest path \(String(reflecting: path)) may not contain '.', '..' or ':'")
            }
            let top = dirs[0].lowercased()
            if systemFolders.contains(top) || (top == "users" && dirs.count <= 2) {
                throw ShareError.invalid("guest path \(path) is inside or is a Windows system folder or a user profile; use a folder of its own such as \(drive)\\work\\<name>")
            }
            return ([drive.uppercased()] + dirs).joined(separator: "\\")
        }

        /// Why a VZ (Linux) share has no `share sync` and no `exclude`.
        public static let liveMount = "shares of VZ (Linux) VMs are live virtiofs mounts of the host folder, so there is nothing to sync and no exclude"

        /// The guest path of a VZ share added without one.
        public static func defaultLinuxGuestPath(name: String) -> String { "/mnt/" + name }

        /// The only Linux subtrees a share may be mounted in. The daemon creates the path and mounts as root, so every
        /// component must be root-owned and not writable by others (`ShareMount.directoryProblem`); /home and /tmp are not.
        public static let linuxMountRoots = ["mnt", "media", "srv", "opt"]

        /// `path` normalised (repeated and trailing `/` dropped), or why it cannot be a VZ share's guest path: it must be
        /// an absolute POSIX path without `.` or `..`, below /mnt, /media, /srv or /opt (not one of them itself).
        public static func linuxGuestPath(_ path: String) throws -> String {
            guard path.hasPrefix("/"), !path.unicodeScalars.contains(where: { $0.value < 0x20 || $0.value == 0x7f }) else {
                throw ShareError.invalid("guest path \(String(reflecting: path)) must be an absolute Linux path such as /mnt/proj")
            }
            let parts = path.split(separator: "/").map(String.init)
            guard !parts.contains(where: { $0 == "." || $0 == ".." }) else {
                throw ShareError.invalid("guest path \(String(reflecting: path)) may not contain '.' or '..'")
            }
            guard let top = parts.first, linuxMountRoots.contains(top), parts.count >= 2 else {
                throw ShareError.invalid(
                    "guest path \(String(reflecting: path)) must be a folder below \(linuxMountRoots.map { "/" + $0 }.joined(separator: ", ")), such as /mnt/<name>")
            }
            return "/" + parts.joined(separator: "/")
        }

        /// Whether one guest path is the other or lies inside it (Windows paths compare case-insensitively).
        func overlaps(_ other: Share) -> Bool {
            if guestPath.hasPrefix("/") || other.guestPath.hasPrefix("/") {
                return guestPath == other.guestPath || guestPath.hasPrefix(other.guestPath + "/") || other.guestPath.hasPrefix(guestPath + "/")
            }
            let a = guestPath.lowercased(), b = other.guestPath.lowercased()
            return a == b || a.hasPrefix(b + "\\") || b.hasPrefix(a + "\\")
        }

        /// Compared without case, as the default macOS volume is case-insensitive.
        func overlapsOnHost(_ other: Share) -> Bool {
            func dir(_ p: String) -> String { p.hasSuffix("/") ? p.lowercased() : p.lowercased() + "/" }
            let a = dir(hostPath), b = dir(other.hostPath)
            return a.hasPrefix(b) || b.hasPrefix(a)
        }
    }

    public enum ShareError: Error, Equatable, CustomStringConvertible {
        case invalid(String)
        case noSuchShare(String, known: [String])
        public var description: String {
            switch self {
            case .invalid(let m): m
            case .noSuchShare(let n, let known):
                "no share named \(String(reflecting: n))" + (known.isEmpty ? " (this VM has no shares)" : " (shares: \(known.joined(separator: ", ")))")
            }
        }
    }

    /// This VM with `share` added, unless its name is taken or its guest path or host folder overlaps another share's.
    /// Host folders may not overlap because copies may write into `rw` folders only: a `rw` share around a `ro` one
    /// would let `copy_from_vm` write into the `ro` folder.
    public func adding(_ share: Share) throws -> VMConfig {
        guard Share.isValid(name: share.name) else {
            throw ShareError.invalid("invalid share name \(String(reflecting: share.name)): use letters, digits, '.', '_' or '-', not starting with '.'")
        }
        if shares.contains(where: { $0.name == share.name }) { throw ShareError.invalid("VM \(name) already has a share named \(share.name)") }
        if let other = shares.first(where: { $0.overlaps(share) }) {
            throw ShareError.invalid("guest path \(share.guestPath) overlaps share \(other.name) at \(other.guestPath): each sync owns its folder")
        }
        if let other = shares.first(where: { $0.overlapsOnHost(share) }) {
            throw ShareError.invalid("host folder \(share.hostPath) overlaps share \(other.name) at \(other.hostPath): share a folder once")
        }
        var c = self
        c.shares.append(share)
        return c
    }

    public func share(_ name: String) throws -> Share {
        guard let s = shares.first(where: { $0.name == name }) else { throw ShareError.noSuchShare(name, known: shares.map(\.name)) }
        return s
    }

    /// The host directories copies may touch: `hostPathAllowlist`, and the shares' folders, every share's for reads
    /// (`copy_to_vm`), only `rw` ones for writes (`copy_from_vm`).
    public func copyAllowlist(hostWrites: Bool) -> (configured: [String], shares: [String]) {
        (hostPathAllowlist, shares.filter { !hostWrites || $0.mode == .rw }.map(\.hostPath))
    }
}

/// The `<name>[/<subdir>]` argument of `share sync`.
public enum ShareReference {
    /// The share name and the subdirectory (relative, `/`-separated, empty for the whole share).
    public static func parse(_ reference: String) throws -> (name: String, subdir: String) {
        var text = reference.replacingOccurrences(of: "\\", with: "/")
        if text.hasSuffix("/") { text.removeLast() }
        let parts = text.split(separator: "/", omittingEmptySubsequences: false).map(String.init)
        guard let name = parts.first, VMConfig.Share.isValid(name: name) else {
            throw VMConfig.ShareError.invalid("give a share as <name> or <name>/<subdir>, not \(String(reflecting: reference))")
        }
        let sub = parts.dropFirst()
        if sub.contains(where: { $0.isEmpty || $0 == "." || $0 == ".." || $0.contains(":") }) {
            throw VMConfig.ShareError.invalid("the subdirectory in \(String(reflecting: reference)) may not contain empty, '.' or '..' components or ':'")
        }
        return (name, sub.joined(separator: "/"))
    }
}
