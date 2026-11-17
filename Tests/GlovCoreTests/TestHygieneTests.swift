import Foundation
import Testing

@testable import GlovCore

@Suite struct TestHygieneTests {
    /// Swift Testing runs every test on the cooperative pool, and `waitUntilExit` holds a pool thread until the child
    /// exits. In the strict pass (`LIBDISPATCH_COOPERATIVE_POOL_STRICT`), where the pool has one thread, that stalls
    /// every other test, and on the 3-core CI runner it pushed polling waits past their deadlines. Wait with
    /// `runToExit` instead. Only a wait right after `terminate()` is allowed, because the child is already exiting.
    @Test func testsWaitForChildProcessesWithoutBlockingThePool() async throws {
        let tests = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
        let offenders = try await Blocking.run { () throws -> [String] in
            let wait = "waitUntil" + "Exit()"  // split, so this file does not match itself
            var offenders: [String] = []
            for case let file as URL in FileManager.default.enumerator(at: tests, includingPropertiesForKeys: nil)! where file.pathExtension == "swift" {
                let lines = try String(contentsOf: file, encoding: .utf8).split(separator: "\n", omittingEmptySubsequences: false)
                for (i, line) in lines.enumerated() where line.contains(wait) && !line.trimmingCharacters(in: .whitespaces).hasPrefix("//") {
                    let previous = i > 0 ? lines[i - 1] : ""
                    if !line.contains("terminate()") && !previous.contains("terminate()") {
                        offenders.append("\(file.lastPathComponent):\(i + 1)")
                    }
                }
            }
            return offenders
        }
        #expect(offenders.isEmpty, "wait with runToExit, not waitUntilExit: \(offenders)")
    }
}
