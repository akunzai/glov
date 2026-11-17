import Foundation

/// How a VZ (Linux) share reaches the guest: a virtiofs device tagged by `tag`, which the daemon mounts through the
/// Guest Service's `exec` once the Guest Service is ready. Commands are argument vectors, never shell strings.
public enum ShareMount {
    /// virtiofs tags are at most 36 bytes of UTF-8 (`VZVirtioFileSystemDeviceConfiguration`).
    public static let maxTagBytes = 36

    /// The tag of the share's virtiofs device: its name, which is already limited to letters, digits, '.', '_' and '-'.
    public static func tag(for share: VMConfig.Share) -> String { share.name }

    /// Whether `guestPath` already is a mount point (exit 0).
    public static func isMountedArguments(_ share: VMConfig.Share) -> [String] { ["mountpoint", "-q", "--", share.guestPath] }

    public static func mountArguments(_ share: VMConfig.Share) -> [String] {
        ["mount", "-t", "virtiofs"] + (share.mode == .ro ? ["-o", "ro"] : []) + ["--", tag(for: share), share.guestPath]
    }

    /// Every directory from `/` down to `path` (a normalised absolute path), in order: `["/", "/mnt", "/mnt/proj"]`.
    public static func components(of path: String) -> [String] {
        var result = ["/"], current = ""
        for part in path.split(separator: "/") {
            current += "/" + part
            result.append(current)
        }
        return result
    }

    /// Prints owner, permission bits and file type of `path` itself, not of what a link points to.
    public static func statArguments(_ path: String) -> [String] { ["stat", "-c", "%u %a %F", "--", path] }

    /// `mkdir` of one component, root-only writable whatever the umask.
    public static func createComponentArguments(_ path: String) -> [String] { ["mkdir", "-m", "0755", "--", path] }

    /// Why a path component is not safe to create below or mount on as root, from `statArguments` output; nil when it is
    /// a real directory owned by root that neither group nor others can write, so no other user can swap it afterwards.
    public static func directoryProblem(_ path: String, statOutput: String) -> String? {
        let fields = statOutput.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: " ", maxSplits: 2).map(String.init)
        guard fields.count == 3, let uid = Int(fields[0]), let mode = Int(fields[1], radix: 8) else {
            return "cannot read the owner and mode of \(path): \(String(reflecting: statOutput))"
        }
        guard fields[2] == "directory" else { return "\(path) is not a real directory (\(fields[2]))" }
        guard uid == 0 else { return "\(path) is owned by uid \(uid), not root" }
        guard mode & 0o022 == 0 else { return "\(path) is writable by group or others (mode \(fields[1]))" }
        return nil
    }
}
