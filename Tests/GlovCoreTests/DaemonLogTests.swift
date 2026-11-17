import Foundation
import Testing

@testable import GlovCore

@Suite struct DaemonLogTests {
    /// Local time with milliseconds and offset, as the Guest Service log writes it, so the two line up.
    @Test func linesAreStampedLikeTheGuestServiceLog() throws {
        let date = Date(timeIntervalSince1970: 1_793_351_087.5)
        #expect(DaemonLog.stamp(date, timeZone: try #require(TimeZone(identifier: "Asia/Taipei"))) == "2026-10-30T17:04:47.500+08:00")
    }

    /// A guest-supplied string cannot forge a log line of its own.
    @Test func controlCharactersAreEscaped() {
        #expect(DaemonLog.escaped("1.0\n2026-10-30T17:04:47.500+08:00 win11: fake\r\u{1B}[2J\t") == #"1.0\u{a}2026-10-30T17:04:47.500+08:00 win11: fake\u{d}\u{1b}[2J\u{9}"#)
        #expect(DaemonLog.escaped("a\u{2028}b") == #"a\u{2028}b"#)
        #expect(DaemonLog.escaped("C:\\src\\établi 日本") == "C:\\src\\établi 日本")
    }
}
