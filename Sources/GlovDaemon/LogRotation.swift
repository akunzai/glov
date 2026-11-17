import Foundation

/// Keeps the file the daemon logs to under a size cap. launchd opens `<home>/glovd.log` as stdout and stderr and never
/// rotates it; the daemon does it itself, the way the Guest Service caps its log: past the cap the file is renamed to
/// `<file>.1` (replacing the previous one) and a fresh file takes its place on the same descriptors, so at most about
/// twice the cap stays on disk. Whatever regular file stdout and stderr point at is handled, so a hand-started daemon
/// redirected to a file is capped too; a terminal or pipe is left alone.
public enum LogRotation {
    public static let cap = 4 << 20

    /// Rotates the file behind `fds` (the first one decides) when it holds `cap` bytes or more. Descriptors that point
    /// at another file are left alone. Returns whether it rotated.
    @discardableResult
    public static func rotateIfNeeded(fds: [Int32] = [STDOUT_FILENO, STDERR_FILENO], cap: Int = cap) -> Bool {
        guard let first = fds.first else { return false }
        var st = stat()
        guard fstat(first, &st) == 0, st.st_mode & S_IFMT == S_IFREG, st.st_size >= cap else { return false }
        var buffer = [UInt8](repeating: 0, count: Int(MAXPATHLEN))
        guard fcntl(first, F_GETPATH, &buffer) == 0 else { return false }
        let path = String(decoding: buffer.prefix { $0 != 0 }, as: UTF8.self)
        let same = fds.filter { fd in
            var other = stat()
            return fstat(fd, &other) == 0 && other.st_dev == st.st_dev && other.st_ino == st.st_ino
        }
        fflush(stdout)
        guard rename(path, path + ".1") == 0 else { return false }
        let fresh = open(path, O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0o600)
        guard fresh >= 0 else { return false }  // the descriptors keep writing to <file>.1
        for fd in same { dup2(fresh, fd) }
        close(fresh)
        return true
    }
}
