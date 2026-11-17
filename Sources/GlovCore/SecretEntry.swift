import Foundation

/// Typing or pasting a secret at a terminal in raw mode (`glov secret set`): one `*` per character, so the length is
/// visible while it is entered, with no line limit (canonical mode cuts a line at MAX_CANON, 1024 bytes on macOS).
public struct SecretEntry: Sendable {
    /// The longest value accepted at the prompt; a longer one is piped on stdin.
    public static let maxBytes = 64 << 10

    public enum Step: Equatable, Sendable {
        /// Write this to the terminal and read on.
        case echo(String)
        case done(String)
        case cancelled
        case tooLong
    }

    private var bytes: [UInt8] = []
    /// Inside an escape sequence (an arrow key, a bracketed-paste marker), which is no part of the value.
    private var escape: [UInt8]?

    public init() {}

    public mutating func feed(_ byte: UInt8) -> Step {
        if var seq = escape {
            seq.append(byte)
            // ESC alone then a byte, or ESC [ parameters, ended by a final byte 0x40–0x7E.
            let ended = seq.count == 2 ? (byte != UInt8(ascii: "[") && byte != UInt8(ascii: "O")) : (0x40...0x7E).contains(byte)
            escape = ended ? nil : seq
            return .echo("")
        }
        switch byte {
        case 0x1B:
            escape = [byte]
            return .echo("")
        case 0x0D, 0x0A: return .done(String(decoding: bytes, as: UTF8.self))
        case 0x03: return .cancelled
        case 0x04: return bytes.isEmpty ? .cancelled : .echo("")
        case 0x7F, 0x08:
            guard !bytes.isEmpty else { return .echo("") }
            removeLastCharacter()
            return .echo("\u{8} \u{8}")
        case 0x15:
            let n = characterCount
            bytes.removeAll()
            return .echo(String(repeating: "\u{8} \u{8}", count: n))
        default:
            guard bytes.count < Self.maxBytes else { return .tooLong }
            bytes.append(byte)
            // A UTF-8 continuation byte belongs to the character already shown.
            return .echo((0x80...0xBF).contains(byte) ? "" : "*")
        }
    }

    private var characterCount: Int { bytes.filter { !(0x80...0xBF).contains($0) }.count }

    private mutating func removeLastCharacter() {
        while let last = bytes.popLast(), (0x80...0xBF).contains(last) {}
    }

    /// What may be shown of a value: its length, and its last 4 characters when it is long enough that they give
    /// little away.
    public static func describe(length: Int, ending: String?) -> String {
        "\(length) character\(length == 1 ? "" : "s")" + (ending.map { ", ending …\($0)" } ?? "")
    }

    /// The last 4 characters of a value of at least 16, else nil.
    public static func ending(of value: String) -> String? { value.count >= 16 ? String(value.suffix(4)) : nil }

    /// How many whitespace or control characters `value` has; tokens have none.
    public static func oddCharacters(_ value: String) -> Int {
        value.unicodeScalars.filter { CharacterSet.whitespacesAndNewlines.contains($0) || CharacterSet.controlCharacters.contains($0) }.count
    }
}
