import Foundation

/// The stream `glov ssh-relay` (ssh's ProxyCommand for a VZ VM) opens on the daemon socket, docs/guest-protocol.md
/// "SSH over vsock". Instead of a JSON-RPC request, a connection starts with `glov-ssh <vm>\n`; the daemon answers
/// `ok\n` and then pipes the connection to the guest's vsock port 4025, or answers `error: <reason>\n` and closes.
public enum SSHRelay {
    public static let header = "glov-ssh"
    /// The longest header line the daemon reads.
    public static let maxHeader = 512

    /// One line read byte by byte, so nothing after it is consumed; nil if the stream ends or the line is too long first.
    public static func readLine(_ fd: Int32) -> String? {
        var line: [UInt8] = []
        var byte: UInt8 = 0
        while line.count < maxHeader, read(fd, &byte, 1) == 1 {
            if byte == 10 { return String(decoding: line, as: UTF8.self) }
            line.append(byte)
        }
        return nil
    }
}
