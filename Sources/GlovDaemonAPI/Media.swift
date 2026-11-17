import CryptoKit
import Foundation

public struct MediaError: Error, Equatable, CustomStringConvertible {
    public var description: String
    public init(_ description: String) { self.description = description }
}

/// The media cache shared by `WindowsMedia` and `UbuntuImage`: `$GLOV_CACHE`, else `~/Library/Caches/glov`,
/// with one subdirectory per kind of media.
public enum MediaCache {
    public static func root(environment: [String: String] = ProcessInfo.processInfo.environment) -> String {
        if let p = environment["GLOV_CACHE"], !p.isEmpty { return (p as NSString).expandingTildeInPath }
        return FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].appendingPathComponent("glov").path
    }
}

public enum Heartbeat {
    /// Logs `what`, then `still <what> (<elapsed>)` every `every` until `body` returns, so a long step with no output of
    /// its own still shows it is running.
    static func whileRunning<T>(_ what: String, log: @escaping (String) -> Void, every: Duration = .seconds(30), _ body: () throws -> T) rethrows -> T {
        log(what)
        let start = ContinuousClock.now
        let timer = DispatchSource.makeTimerSource(queue: .global())
        let interval = Int(every.components.seconds)
        timer.schedule(deadline: .now() + .seconds(interval), repeating: .seconds(interval))
        nonisolated(unsafe) let note = log
        timer.setEventHandler {
            let s = Int((ContinuousClock.now - start).components.seconds)
            note("still \(what) (\(s / 60)m \(String(format: "%02d", s % 60))s)")
        }
        timer.resume()
        defer { timer.cancel() }
        return try body()
    }
}

/// Verified, resumable downloads into a media cache, with seams for tests.
protocol MediaDownloads {
    var cache: String { get }
    /// Fetches `url` into `path`, continuing from the end of an existing file.
    var download: (URL, String) throws -> Void { get }
    var freeSpace: (String) -> UInt64? { get }
    var log: (String) -> Void { get }
}

extension MediaDownloads {
    /// Downloads `url` to `path` through `path.partial` (resuming it), and moves it into place only when its digest is
    /// `expected`; on a mismatch the partial file is deleted, so the next run starts over.
    func downloadVerified<H: HashFunction>(_ url: URL, to path: String, expected: String, _ hash: H.Type, _ name: String) throws {
        let fm = FileManager.default
        let file = url.lastPathComponent, partial = path + ".partial"
        let have = (try? fm.attributesOfItem(atPath: partial)[.size] as? UInt64) ?? 0
        log(have > 0 ? "resuming \(file) at \(have) bytes" : "downloading \(file) from \(url.host ?? "")")
        try download(url, partial)
        let digest = try whileRunning("verifying \(name)") { try Self.digest(of: partial, H.self) }
        guard digest == expected.lowercased() else {
            try? fm.removeItem(atPath: partial)
            throw MediaError("\(name) mismatch for \(file): expected \(expected), got \(digest); deleted the download")
        }
        try fm.moveItem(atPath: partial, toPath: path)
    }

    func whileRunning<T>(_ what: String, every: Duration = .seconds(30), _ body: () throws -> T) rethrows -> T {
        try Heartbeat.whileRunning(what, log: log, every: every, body)
    }

    func requireSpace(_ bytes: UInt64, for what: String) throws {
        guard let free = freeSpace(cache), free < bytes + (1 << 30) else { return }
        throw MediaError("\(what) needs about \(bytes >> 30) GiB but \(cache) has \(free >> 30) GiB free; set GLOV_CACHE to a larger disk")
    }

    /// `<digest>  <file>` or `<digest> *<file>` lines, as in CHECKSUM and SHA256SUMS files.
    static func checksums(_ text: String) -> [String: String] {
        var out: [String: String] = [:]
        for line in text.split(separator: "\n") {
            let f = line.split(whereSeparator: { $0 == " " || $0 == "\t" })
            if f.count == 2 { out[String(f[1]).trimmingCharacters(in: CharacterSet(charactersIn: "*"))] = f[0].lowercased() }
        }
        return out
    }

    static func digest<H: HashFunction>(of path: String, _: H.Type) throws -> String {
        let h = try FileHandle(forReadingFrom: URL(fileURLWithPath: path))
        defer { try? h.close() }
        var hash = H()
        while let chunk = try h.read(upToCount: 8 << 20), !chunk.isEmpty { hash.update(data: chunk) }
        return hash.finalize().map { String(format: "%02x", $0) }.joined()
    }

    static func defaultFreeSpace(_ path: String) -> UInt64? {
        try? URL(fileURLWithPath: path).resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey])
            .volumeAvailableCapacityForImportantUsage.map { UInt64($0) }
    }

    static func defaultLog(_ line: String) { FileHandle.standardError.write(Data("\(line)\n".utf8)) }

    /// curl ships with macOS; `-C -` continues from the end of `path`. Its progress meter goes to the terminal.
    @available(*, noasync, message: "blocks a cooperative thread; call it through Blocking.run")
    static func curl(_ url: URL, _ path: String) throws {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
        p.arguments = ["--fail", "--location", "--retry", "5", "--continue-at", "-", "--progress-bar", "--output", path, url.absoluteString]
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = FileHandle.standardError
        let exited = DispatchSemaphore(value: 0)
        p.terminationHandler = { _ in exited.signal() }
        try p.run()
        exited.wait()
        guard p.terminationStatus == 0 else {
            throw MediaError("download of \(url.absoluteString) failed (curl exit \(p.terminationStatus)); run again to resume")
        }
    }
}
