import Foundation

/// The owner's harness config on the Mac, copied into an isolated VM (`HarnessProfile.hostConfig`): one way, as plain
/// files. Symbolic links are followed, so a skill linked from elsewhere arrives as a real directory; a broken link or a
/// cycle is left out and named. Credentials are never copied: a profile may not name such a path, and a file with
/// such a name inside a copied directory is left out.
public enum HostConfig {
    /// Names that hold sign-ins or tokens, wherever they are.
    public static let credentialNames: Set<String> = [
        ".credentials.json", "auth.json", "antigravity-oauth-token", "oauth_creds.json", "credentials.json", ".env",
    ]
    /// The most a copy holds, in bytes and files, so a stray tree cannot fill the guest.
    public static let maxBytes = 64 << 20
    public static let maxFiles = 10_000

    static func problems(_ copies: [HarnessProfile.HostCopy], files: [HarnessProfile.File]) -> [String] {
        var problems: [String] = []
        for c in copies {
            for path in [c.host, c.guest] {
                let parts = path.split(separator: "/", omittingEmptySubsequences: false)
                if path.isEmpty || path.hasPrefix("/") || parts.contains("..") || parts.contains("") {
                    problems.append("hostConfig \(path): give a relative path inside the home directory")
                }
                if let last = parts.last, credentialNames.contains(String(last)) {
                    problems.append("hostConfig \(path): holds credentials, which are never copied")
                }
            }
            // glov writes these itself (the placeholders, the proxy's model provider); a copy would undo that.
            for f in files where f.path == c.guest || f.path.hasPrefix(c.guest + "/") || c.guest.hasPrefix(f.path + "/") {
                problems.append("hostConfig \(c.guest): overlaps \(f.path), which glov writes")
            }
        }
        return problems
    }

    /// What `stage` did: the copies made (guest path, files, bytes), and what it left out, with why.
    public struct Report: Equatable, Sendable {
        public var copied: [(guest: String, files: Int, bytes: Int)] = []
        public var skipped: [String] = []
        public var files: Int { copied.reduce(0) { $0 + $1.files } }
        public var bytes: Int { copied.reduce(0) { $0 + $1.bytes } }

        public static func == (a: Report, b: Report) -> Bool {
            a.skipped == b.skipped && a.copied.elementsEqual(b.copied) { $0 == $1 }
        }
    }

    public struct TooLarge: Error, CustomStringConvertible {
        public var description: String
    }

    /// Copies each of `copies` that exists under `hostHome` into `staging/<guest path>`, links followed. Throws
    /// `TooLarge` past `maxBytes` or `maxFiles` in all.
    public static func stage(_ copies: [HarnessProfile.HostCopy], hostHome: String, into staging: String) throws -> Report {
        let fm = FileManager.default
        var report = Report()
        var files = 0, bytes = 0
        for c in copies {
            let source = hostHome + "/" + c.host
            guard (try? fm.destinationOfSymbolicLink(atPath: source)) != nil || fm.fileExists(atPath: source) else { continue }
            var copiedFiles = 0, copiedBytes = 0
            // `seen` holds the real paths of the directories above, so a link back up is a cycle.
            func copy(_ from: String, _ to: String, shown: String, seen: Set<String>) throws {
                let real = (from as NSString).resolvingSymlinksInPath
                var isDir = ObjCBool(false)
                guard fm.fileExists(atPath: real, isDirectory: &isDir) else { return report.skipped.append("~/\(shown) (broken link)") }
                let name = (shown as NSString).lastPathComponent
                if credentialNames.contains(name) { return report.skipped.append("~/\(shown) (credentials)") }
                if isDir.boolValue {
                    guard !seen.contains(real) else { return report.skipped.append("~/\(shown) (link cycle)") }
                    try fm.createDirectory(atPath: to, withIntermediateDirectories: true)
                    for child in try fm.contentsOfDirectory(atPath: real).sorted() {
                        try copy(real + "/" + child, to + "/" + child, shown: shown + "/" + child, seen: seen.union([real]))
                    }
                    return
                }
                guard let type = try fm.attributesOfItem(atPath: real)[.type] as? FileAttributeType, type == .typeRegular else {
                    return report.skipped.append("~/\(shown) (not a file)")
                }
                let size = (try fm.attributesOfItem(atPath: real)[.size] as? Int) ?? 0
                files += 1
                bytes += size
                guard files <= maxFiles, bytes <= maxBytes else {
                    throw TooLarge(description: "the host config is over \(maxFiles) files or \(maxBytes >> 20) MiB at ~/\(shown); copy less, or pass --no-host-config")
                }
                try fm.createDirectory(atPath: (to as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
                try fm.copyItem(atPath: real, toPath: to)
                copiedFiles += 1
                copiedBytes += size
            }
            try copy(source, staging + "/" + c.guest, shown: c.host, seen: [])
            if copiedFiles > 0 { report.copied.append((c.guest, copiedFiles, copiedBytes)) }
        }
        return report
    }
}
