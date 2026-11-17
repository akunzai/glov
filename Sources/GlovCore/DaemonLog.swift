import Foundation

/// The daemon's log lines: stderr, which launchd points at `glovd.log`. Each line starts with the local time, in the
/// Guest Service log's format (`2026-10-30T17:04:47.402+08:00`), so the two can be read side by side.
public enum DaemonLog {
    public static func write(_ line: String) { FileHandle.standardError.write(Data("\(stamp(Date())) \(escaped(line))\n".utf8)) }

    /// `line` with control characters and line separators written as `\u{..}`: lines carry text from the guest (its version, file names),
    /// which must not be able to start a line of its own.
    static func escaped(_ line: String) -> String {
        var out = ""
        for scalar in line.unicodeScalars {
            switch scalar.properties.generalCategory {
            case .control, .lineSeparator, .paragraphSeparator:
                out += "\\u{\(String(scalar.value, radix: 16))}"
            default:
                out.unicodeScalars.append(scalar)
            }
        }
        return out
    }

    /// Writes lines about one VM, each prefixed with its name.
    public static func vm(_ name: String) -> GuestChannel.Log { { write("\(name): \($0)") } }

    static func stamp(_ date: Date, timeZone: TimeZone = .current) -> String {
        date.formatted(Date.ISO8601FormatStyle(timeZoneSeparator: .colon, includingFractionalSeconds: true, timeZone: timeZone))
    }
}
