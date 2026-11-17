import Foundation
import Testing

@testable import GlovCore

@Suite struct EgressLogTests {
    static func record(_ host: String?, _ decision: String, at seconds: Double = 0) -> EgressRecord {
        EgressRecord(
            time: Date(timeIntervalSince1970: 1_793_400_000 + seconds), vm: "box", decision: .init(rawValue: decision)!, host: host, port: host == nil ? nil : 443,
            bytesIn: decision == "allow" ? 10 : 0, bytesOut: decision == "allow" ? 5 : 0, reason: decision == "allow" ? nil : "not in the allowlist")
    }

    func withLog(cap: Int = 1 << 20, _ body: (EgressLog, String) throws -> Void) throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        try body(EgressLog(path: dir + "/egress.log", cap: cap), dir + "/egress.log")
    }

    @Test func aRecordReadsAsOneMetadataLine() {
        #expect(Self.record("api.example.com", "allow").line == "egress allow api.example.com:443 in=10 out=5")
        #expect(Self.record("evil.example.net", "deny").line == "egress deny evil.example.net:443 in=0 out=0 (not in the allowlist)")
        #expect(Self.record(nil, "deny").line == "egress deny - in=0 out=0 (not in the allowlist)")
        var v6 = Self.record("2001:db8::7", "allow")
        v6.port = 8080
        #expect(v6.line == "egress allow [2001:db8::7]:8080 in=10 out=5")
    }

    @Test func recordsAreJSONLinesWithUTCTimes() throws {
        try withLog { log, path in
            log.append(Self.record("api.example.com", "allow"))
            let text = try String(contentsOfFile: path, encoding: .utf8)
            #expect(text.hasSuffix("}\n") && text.split(separator: "\n").count == 1)
            #expect(text.contains("\"time\":\"2026-10-30T22:40:00Z\""))
            #expect(text.contains("Z\""))
        }
    }

    @Test func withoutACursorTheLastRecordsComeBackAndDeniedKeepsOnlyRefusals() throws {
        try withLog { log, path in
            for (i, (host, decision)) in [("a.com", "allow"), ("b.com", "deny"), ("c.com", "fail"), ("d.com", "allow"), ("e.com", "deny")].enumerated() {
                log.append(Self.record(host, decision, at: Double(i)))
            }
            #expect(EgressLog.read(path: path, cursor: nil, deniedOnly: false, limit: 3).records.map(\.host) == ["c.com", "d.com", "e.com"])
            #expect(EgressLog.read(path: path, cursor: nil, deniedOnly: true, limit: 10).records.map(\.host) == ["b.com", "c.com", "e.com"])
        }
    }

    @Test func aCursorReturnsOnlyWhatCameAfterItAcrossARotation() throws {
        try withLog(cap: 600) { log, path in
            log.append(Self.record("a.com", "allow"))
            let start = EgressLog.read(path: path, cursor: nil, deniedOnly: false, limit: 10)
            #expect(start.records.map(\.host) == ["a.com"])
            #expect(EgressLog.read(path: path, cursor: start.cursor, deniedOnly: false, limit: 10).records.isEmpty)
            for i in 0..<6 { log.append(Self.record("n\(i).com", "deny", at: Double(i))) }  // past the cap: rotates
            #expect(FileManager.default.fileExists(atPath: path + ".1"))
            let next = EgressLog.read(path: path, cursor: start.cursor, deniedOnly: false, limit: 10)
            #expect(next.records.map(\.host) == (0..<6).map { "n\($0).com" })
            #expect(EgressLog.read(path: path, cursor: next.cursor, deniedOnly: false, limit: 10).records.isEmpty)
        }
    }

    @Test func aMissingLogIsEmptyAndAPartialLastLineWaits() throws {
        try withLog { log, path in
            let empty = EgressLog.read(path: path, cursor: nil, deniedOnly: false, limit: 10)
            #expect(empty.records.isEmpty)
            log.append(Self.record("a.com", "allow"))
            let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
            try h.seekToEnd()
            try h.write(contentsOf: Data("{\"time\":".utf8))
            try h.close()
            let r = EgressLog.read(path: path, cursor: empty.cursor, deniedOnly: false, limit: 10)
            #expect(r.records.map(\.host) == ["a.com"])
        }
    }
}
