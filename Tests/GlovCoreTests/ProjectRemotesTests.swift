import Foundation
import Testing

@testable import GlovCore

@Suite struct ProjectRemotesTests {
    @Test func confirmedRemotesAreKeptPerProject() throws {
        let home = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).path
        try FileManager.default.createDirectory(atPath: home, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(atPath: home) }
        let store = ProjectRemotes(home: home)
        #expect(store.confirmed("/p").isEmpty)
        try store.confirm("/p", [RepoPattern("github.com/o/a")!])
        try store.confirm("/p", [RepoPattern("github.com/o/b")!, RepoPattern("github.com/o/a")!])
        #expect(store.confirmed("/p") == ["github.com/o/a", "github.com/o/b"] && store.confirmed("/q").isEmpty)
    }

    @Test func enterAcceptsOnlyWhatWasConfirmedBefore() {
        func apply(_ anyNew: Bool, terminal: Bool = true, flag: Bool = false, _ answer: String?) -> Bool {
            ProjectRemotes.apply(anyNew: anyNew, isTerminal: terminal, allowFlag: flag) { answer }
        }
        #expect(apply(false, "") && apply(false, nil) && !apply(false, "n"))
        #expect(!apply(true, "") && !apply(true, nil) && apply(true, "y") && apply(true, " Yes "))
        #expect(!apply(false, terminal: false, "y"), "no terminal: no question")
        #expect(apply(true, terminal: false, flag: true, nil))
    }
}
