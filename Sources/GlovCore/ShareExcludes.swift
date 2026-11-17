import Foundation

/// A share's `exclude` patterns in gitignore syntax, relative to the share root: `#` starts a comment, `!` re-includes,
/// a trailing `/` matches directories only, a `/` at the start or in the middle anchors the pattern to the share root
/// (otherwise it matches at any depth), `*`, `?` and `[...]` never match `/`, and `**` matches across directories as a
/// leading `**/`, a trailing `/**` or an inner `/**/`. The last matching pattern decides, and nothing below an excluded
/// directory is seen, so it cannot be re-included.
///
/// Each pattern compiles to a regular expression over the whole relative path in the syntax ICU and Go's RE2 share,
/// because the Guest Service applies the same `rules` while it walks (docs/guest-protocol.md, `sync.manifest`).
public struct ShareExcludes: @unchecked Sendable {  // NSRegularExpression is immutable
    public struct Rule: Codable, Sendable, Equatable {
        public var pattern: String
        public var negate: Bool
        public var dirOnly: Bool
        public init(pattern: String, negate: Bool, dirOnly: Bool) { self.pattern = pattern; self.negate = negate; self.dirOnly = dirOnly }
    }

    public struct InvalidPattern: Error, CustomStringConvertible {
        public var pattern: String
        public var description: String { "cannot read exclude pattern \(String(reflecting: pattern))" }
    }

    public let rules: [Rule]
    private let compiled: [NSRegularExpression]

    public init(_ patterns: [String]) throws {
        var rules: [Rule] = []
        var compiled: [NSRegularExpression] = []
        for p in patterns {
            guard let rule = Self.compile(p) else { continue }
            do { compiled.append(try NSRegularExpression(pattern: rule.pattern)) } catch { throw InvalidPattern(pattern: p) }
            rules.append(rule)
        }
        self.rules = rules
        self.compiled = compiled
    }

    /// Whether `path` (relative to the share root, `/`-separated) itself is excluded.
    public func excludes(_ path: String, isDir: Bool) -> Bool {
        var excluded = false
        let range = NSRange(path.startIndex..., in: path)
        for (rule, re) in zip(rules, compiled) where !rule.dirOnly || isDir {
            if re.firstMatch(in: path, range: range) != nil { excluded = !rule.negate }
        }
        return excluded
    }

    /// Whether `path` or a directory above it is excluded.
    public func excludesPathOrParent(_ path: String, isDir: Bool = false) -> Bool {
        let parts = path.split(separator: "/")
        for i in parts.indices.dropLast() where excludes(parts[...i].joined(separator: "/"), isDir: true) { return true }
        return excludes(path, isDir: isDir)
    }

    static func compile(_ raw: String) -> Rule? {
        var p = Substring(raw)
        // Trailing spaces go unless escaped.
        while p.hasSuffix(" ") && !p.hasSuffix("\\ ") { p = p.dropLast() }
        if p.isEmpty || p.hasPrefix("#") { return nil }
        var negate = false
        if p.hasPrefix("!") { negate = true; p = p.dropFirst() }
        var dirOnly = false
        while p.hasSuffix("/") { dirOnly = true; p = p.dropLast() }
        if p.isEmpty { return nil }
        let anchored = p.dropLast().contains("/")
        if p.hasPrefix("/") { p = p.dropFirst() }
        let chars = Array(p)
        let anyDirs = "(?:.*/)?", anyName = "[^/]*"
        // Unanchored patterns start with anyDirs. Repeated wildcards are collapsed as they are emitted ("**/**/" is
        // "**/", "***" and "**" inside a name are "*"): each copy would multiply the ways a backtracking matcher can
        // fail, which on the host (ICU) is exponential in their number.
        var out = anchored ? "" : anyDirs
        func emit(_ part: String) { if !(part == anyDirs || part == anyName) || !out.hasSuffix(part) { out += part } }
        var i = 0
        while i < chars.count {
            let c = chars[i]
            switch c {
            case "*":
                var end = i
                while end < chars.count && chars[end] == "*" { end += 1 }
                let atStart = i == 0, afterSlash = i > 0 && chars[i - 1] == "/"
                let atEnd = end == chars.count, beforeSlash = end < chars.count && chars[end] == "/"
                if end - i == 2 && (atStart || afterSlash) && beforeSlash {
                    emit(anyDirs)  // "**/" at the start, or "/**/": zero or more directories
                    i = end + 1
                } else if end - i == 2 && afterSlash && atEnd {
                    out += ".*"  // "/**": everything inside
                    i = end
                } else {
                    emit(anyName)
                    i = end
                }
                continue
            case "?": out += "[^/]"
            case "[":
                if let (cls, next) = bracket(chars, i) { out += cls; i = next; continue }
                out += "\\["
            case "\\":
                if i + 1 < chars.count { i += 1; out += literal(chars[i]) } else { out += "\\\\" }
            default: out += literal(c)
            }
            i += 1
        }
        return Rule(pattern: "^" + out + "$", negate: negate, dirOnly: dirOnly)
    }

    private static func literal(_ c: Character) -> String { ".^$|?*+()[]{}\\".contains(c) ? "\\" + String(c) : String(c) }

    /// `[...]` starting at `start` as a class that never matches `/`, and the index after it; nil when unclosed.
    private static func bracket(_ chars: [Character], _ start: Int) -> (String, Int)? {
        var i = start + 1
        var negated = false
        if i < chars.count, chars[i] == "!" || chars[i] == "^" { negated = true; i += 1 }
        var body = ""
        var first = true
        while i < chars.count {
            let c = chars[i]
            if c == "]" && !first { return ("[" + (negated ? "^/" : "") + body + "]", i + 1) }
            first = false
            switch c {
            case "/": break  // a class never matches the separator
            case "\\":
                if i + 1 < chars.count { i += 1; body += "\\" + String(chars[i]) }
            case "[", "]", "^", "&", "~": body += "\\" + String(c)
            default: body += String(c)
            }
            i += 1
        }
        return nil
    }
}
