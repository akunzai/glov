import Foundation

public enum HostPath {
    /// `path` with every symbolic link, `.` and `..` resolved, or nil when it does not exist.
    public static func realpath(_ path: String) -> String? {
        guard let p = Foundation.realpath(path, nil) else { return nil }
        defer { free(p) }
        return String(cString: p)
    }
}
