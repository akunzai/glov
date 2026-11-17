import Foundation

/// Pure mappings from agent-facing input to QMP command arguments.
enum Input {
    /// Pixel (in a frame `extent` pixels wide or tall) to the 0...32767 range of a usb-tablet.
    static func absolute(_ pixel: Int, extent: Int) -> Int {
        guard extent > 0 else { return 0 }
        let v = (Double(pixel) + 0.5) / Double(extent) * 32767
        return min(32767, max(0, Int(v)))
    }

    static func move(x: Int, y: Int, frame: PixelSize) -> JSONValue {
        ["events": [
            ["type": "abs", "data": ["axis": "x", "value": .int(absolute(x, extent: frame.width))]],
            ["type": "abs", "data": ["axis": "y", "value": .int(absolute(y, extent: frame.height))]],
        ]]
    }

    static func button(_ name: String, down: Bool) -> JSONValue {
        ["events": [["type": "btn", "data": ["down": .bool(down), "button": .string(name)]]]]
    }

    static let shifted: [Character: String] = [
        "~": "grave_accent", "!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6", "&": "7", "*": "8",
        "(": "9", ")": "0", "_": "minus", "+": "equal", "{": "bracket_left", "}": "bracket_right",
        "|": "backslash", ":": "semicolon", "\"": "apostrophe", "<": "comma", ">": "dot", "?": "slash",
    ]
    static let plain: [Character: String] = [
        " ": "spc", "-": "minus", "=": "equal", "[": "bracket_left", "]": "bracket_right", "\\": "backslash",
        ";": "semicolon", "'": "apostrophe", ",": "comma", ".": "dot", "/": "slash", "`": "grave_accent",
        "\n": "ret", "\t": "tab",
    ]

    /// qcodes pressed together to produce `c` on a US layout.
    static func qcodes(for c: Character) throws -> [String] {
        guard c.isASCII else { throw GlovError.unsupportedCharacter(c) }
        if c.isLetter { return (c.isUppercase ? ["shift"] : []) + [c.lowercased()] }
        if c.isNumber { return [String(c)] }
        if let q = plain[c] { return [q] }
        if let q = shifted[c] { return ["shift", q] }
        throw GlovError.unsupportedCharacter(c)
    }

    static let keyNames: [String: String] = [
        "ctrl": "ctrl", "control": "ctrl", "alt": "alt", "shift": "shift", "win": "meta_l", "meta": "meta_l",
        "super": "meta_l", "enter": "ret", "return": "ret", "esc": "esc", "escape": "esc", "tab": "tab",
        "space": "spc", "backspace": "backspace", "delete": "delete", "del": "delete", "insert": "insert",
        "home": "home", "end": "end", "pageup": "pgup", "pagedown": "pgdn", "up": "up", "down": "down",
        "left": "left", "right": "right", "capslock": "caps_lock", "menu": "menu",
    ]

    /// "ctrl+alt+delete" or "enter" to qcodes. Single characters map like typed text.
    static func qcodes(forCombo combo: String) throws -> [String] {
        try combo.split(separator: "+", omittingEmptySubsequences: false).flatMap { part -> [String] in
            let name = part.lowercased()
            if let q = keyNames[name] { return [q] }
            if name.count > 1, name.first == "f", let n = Int(name.dropFirst()), (1...12).contains(n) { return [name] }
            if part.count == 1, let c = part.first { return try qcodes(for: c) }
            throw GlovError.unknownKey(String(part))
        }
    }

    static func sendKey(_ qcodes: [String], holdMs: Int = 20) -> JSONValue {
        ["keys": .array(qcodes.map { ["type": "qcode", "data": .string($0)] }), "hold-time": .int(holdMs)]
    }
}
