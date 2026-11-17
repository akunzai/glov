import Testing

@testable import GlovCore

@Suite struct ShareExcludesTests {
    private func excluded(_ patterns: [String], _ path: String, dir: Bool = false) throws -> Bool {
        try ShareExcludes(patterns).excludes(path, isDir: dir)
    }

    @Test func aTrailingSlashMatchesDirectoriesAtAnyDepth() throws {
        #expect(try excluded(["bin/"], "bin", dir: true))
        #expect(try excluded(["bin/"], "src/app/bin", dir: true))
        #expect(try !excluded(["bin/"], "bin"), "a file named bin is not a directory")
        #expect(try !excluded(["bin/"], "binary", dir: true))
    }

    @Test func aLeadingSlashAnchorsToTheShareRoot() throws {
        #expect(try excluded(["/root-only"], "root-only", dir: true))
        #expect(try excluded(["/root-only"], "root-only"))
        #expect(try !excluded(["/root-only"], "sub/root-only", dir: true))
    }

    @Test func aSlashInTheMiddleAnchorsToo() throws {
        #expect(try excluded(["docs/*.md"], "docs/a.md"))
        #expect(try !excluded(["docs/*.md"], "x/docs/a.md"))
        #expect(try !excluded(["docs/*.md"], "docs/sub/a.md"), "* does not cross /")
    }

    @Test func wildcardsAndClasses() throws {
        #expect(try excluded(["*.log"], "a/b/c.log"))
        #expect(try !excluded(["*.log"], "c.log.txt"))
        #expect(try excluded(["?.txt"], "a.txt") && !(try excluded(["?.txt"], "ab.txt")))
        #expect(try excluded(["[ab].txt"], "b.txt") && !(try excluded(["[ab].txt"], "c.txt")))
        #expect(try excluded(["[!ab].txt"], "c.txt") && !(try excluded(["[!ab].txt"], "a.txt")))
        #expect(try excluded(["[a-c]x"], "bx") && !(try excluded(["[a-c]x"], "dx")))
        #expect(try excluded(["a+b(1).txt"], "a+b(1).txt"), "regex metacharacters are literal")
    }

    @Test func doubleStars() throws {
        #expect(try excluded(["**/tmp"], "tmp", dir: true) && excluded(["**/tmp"], "a/b/tmp", dir: true))
        #expect(try excluded(["a/**/b"], "a/b") && excluded(["a/**/b"], "a/x/y/b"))
        #expect(try !excluded(["a/**/b"], "x/a/b"))
        #expect(try excluded(["logs/**"], "logs/x/y.txt") && !(try excluded(["logs/**"], "logs", dir: true)))
    }

    @Test func theLastMatchingRuleWinsAndNegationReincludes() throws {
        let p = ["*.log", "!keep.log"]
        #expect(try excluded(p, "a.log") && !(try excluded(p, "sub/keep.log")))
        #expect(try excluded(["!keep.log", "*.log"], "keep.log"), "order matters")
    }

    @Test func commentsBlanksAndEscapes() throws {
        let rules = try ShareExcludes(["# a comment", "", "   ", "\\#hash", "\\!bang", "trailing   "])
        #expect(rules.rules.count == 3)
        #expect(rules.excludes("#hash", isDir: false) && rules.excludes("!bang", isDir: false) && rules.excludes("trailing", isDir: false))
    }

    @Test func aPathIsExcludedWhenADirectoryAboveItIs() throws {
        let rules = try ShareExcludes(["bin/", "*.log", "!important.log"])
        #expect(rules.excludesPathOrParent("src/bin/x/y.dll"))
        #expect(!rules.excludesPathOrParent("bin"), "a file named bin is no directory")
        #expect(rules.excludesPathOrParent("bin", isDir: true))
        #expect(rules.excludesPathOrParent("bin/important.log"), "nothing below an excluded directory comes back")
        #expect(!rules.excludesPathOrParent("src/a.cs"))
    }

    /// The Guest Service evaluates the same expressions with Go's RE2 (guest/internal/mirror): these must stay in the syntax both share.
    @Test func theRulesSentToTheGuestAreRE2Expressions() throws {
        let rules = try ShareExcludes(["bin/", "/root-only", "!keep.log", "a/**/b", "[!x]?.c"]).rules
        #expect(rules == [
            .init(pattern: "^(?:.*/)?bin$", negate: false, dirOnly: true),
            .init(pattern: "^root-only$", negate: false, dirOnly: false),
            .init(pattern: "^(?:.*/)?keep\\.log$", negate: true, dirOnly: false),
            .init(pattern: "^a/(?:.*/)?b$", negate: false, dirOnly: false),
            .init(pattern: "^(?:.*/)?[^/x][^/]\\.c$", negate: false, dirOnly: false),
        ])
    }

    /// Repeated wildcards collapse, so a pathological pattern cannot make the backtracking matcher on the host take
    /// exponential time.
    @Test func redundantWildcardsCollapse() throws {
        let rules = try ShareExcludes(["**/**/**/x", "a/**/**/b", "a***b", "a*****/c"]).rules.map(\.pattern)
        #expect(rules == ["^(?:.*/)?x$", "^a/(?:.*/)?b$", "^(?:.*/)?a[^/]*b$", "^a[^/]*/c$"])
        let deep = Array(repeating: "a", count: 60).joined(separator: "/") + "/y"
        let wide = String(repeating: "a", count: 200) + "/y"
        let patterns = [Array(repeating: "**/", count: 30).joined() + "x", "a/" + Array(repeating: "**/", count: 30).joined() + "x",
                        "a" + String(repeating: "*", count: 60) + "b"]
        let excludes = try ShareExcludes(patterns)
        let started = ContinuousClock.now
        #expect(!excludes.excludesPathOrParent(deep) && !excludes.excludesPathOrParent(wide))
        #expect(ContinuousClock.now - started < .seconds(1))
    }

    @Test func anUnclosedBracketIsLiteral() throws {
        #expect(try excluded(["a[b"], "a[b"))
    }
}
