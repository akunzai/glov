import Testing

@testable import GlovCore

@Suite struct InputTests {
    @Test func pixelCentersMapOntoTheTabletRange() {
        #expect(Input.absolute(0, extent: 1280) == 12)
        #expect(Input.absolute(640, extent: 1280) == 16396)
        #expect(Input.absolute(1279, extent: 1280) == 32754)
        #expect(Input.absolute(5000, extent: 1280) == 32767)
        #expect(Input.absolute(-5, extent: 1280) == 0)
    }

    @Test func moveSendsAbsoluteAxes() {
        let m = Input.move(x: 400, y: 300, frame: PixelSize(width: 800, height: 600))
        #expect(m == ["events": [
            ["type": "abs", "data": ["axis": "x", "value": 16403]],
            ["type": "abs", "data": ["axis": "y", "value": 16410]],
        ]])
    }

    @Test func buttonEvent() {
        #expect(Input.button("wheel-up", down: true) == ["events": [["type": "btn", "data": ["down": true, "button": "wheel-up"]]]])
    }

    @Test(arguments: [
        ("a", ["a"]), ("Z", ["shift", "z"]), ("7", ["7"]), (" ", ["spc"]), ("\n", ["ret"]), ("\t", ["tab"]),
        ("-", ["minus"]), ("_", ["shift", "minus"]), ("!", ["shift", "1"]), ("?", ["shift", "slash"]),
        ("\\", ["backslash"]), ("|", ["shift", "backslash"]), ("\"", ["shift", "apostrophe"]),
        ("`", ["grave_accent"]), ("~", ["shift", "grave_accent"]), (":", ["shift", "semicolon"]),
    ] as [(Character, [String])])
    func usLayoutKeys(char: Character, expected: [String]) throws {
        #expect(try Input.qcodes(for: char) == expected)
    }

    @Test func everyPrintableAsciiCharacterIsTypeable() throws {
        for v in 0x20...0x7e { _ = try Input.qcodes(for: Character(UnicodeScalar(UInt8(v)))) }
    }

    @Test func nonAsciiCannotBeTyped() {
        #expect(throws: GlovError.unsupportedCharacter("é")) { try Input.qcodes(for: "é") }
    }

    @Test func combos() throws {
        #expect(try Input.qcodes(forCombo: "ctrl+alt+Delete") == ["ctrl", "alt", "delete"])
        #expect(try Input.qcodes(forCombo: "win+r") == ["meta_l", "r"])
        #expect(try Input.qcodes(forCombo: "Enter") == ["ret"])
        #expect(try Input.qcodes(forCombo: "f5") == ["f5"])
        #expect(try Input.qcodes(forCombo: "shift+A") == ["shift", "shift", "a"])
        #expect(throws: GlovError.unknownKey("bogus")) { try Input.qcodes(forCombo: "ctrl+bogus") }
    }

    @Test func sendKeyArguments() {
        #expect(Input.sendKey(["shift", "a"]) == [
            "keys": [["type": "qcode", "data": "shift"], ["type": "qcode", "data": "a"]], "hold-time": 20,
        ])
    }
}
