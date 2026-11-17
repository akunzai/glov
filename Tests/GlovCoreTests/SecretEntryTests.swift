import Testing

@testable import GlovCore

@Suite struct SecretEntryTests {
    func type(_ text: [UInt8]) -> (shown: String, step: SecretEntry.Step?) {
        var entry = SecretEntry(), shown = ""
        for b in text {
            switch entry.feed(b) {
            case .echo(let s): shown += s
            case let step: return (shown, step)
            }
        }
        return (shown, nil)
    }

    @Test func eachCharacterShowsOneStarAndBackspaceTakesOneBack() {
        let r = type(Array("abé".utf8) + [0x7F] + Array("cd\r".utf8))
        #expect(r.step == .done("abcd"))
        #expect(r.shown == "***\u{8} \u{8}**")
    }

    @Test func aDoublePasteIsVisibleAndCtrlUClearsIt() {
        let r = type(Array("tok".utf8) + Array("tok".utf8) + [0x15] + Array("tok\n".utf8))
        #expect(r.step == .done("tok"))
        #expect(r.shown.hasPrefix("******" + String(repeating: "\u{8} \u{8}", count: 6)))
    }

    @Test func escapeSequencesAndPasteMarkersAreNoPartOfTheValue() {
        let r = type([0x1B] + Array("[200~v1".utf8) + [0x1B] + Array("[201~".utf8) + [0x1B] + Array("[D\r".utf8))
        #expect(r.step == .done("v1") && r.shown == "**")
    }

    @Test func ctrlCAndAnEmptyCtrlDCancelAndALongValueIsRefused() {
        #expect(type(Array("ab".utf8) + [0x03]).step == .cancelled)
        #expect(type([0x04]).step == .cancelled)
        #expect(type([UInt8](repeating: 0x61, count: SecretEntry.maxBytes + 1)).step == .tooLong)
        // Longer than a canonical line, read whole.
        #expect(type([UInt8](repeating: 0x61, count: 3000) + [0x0D]).step == .done(String(repeating: "a", count: 3000)))
    }

    @Test func onlyTheLengthAndAShortEndingAreShown() {
        #expect(SecretEntry.describe(length: 108, ending: SecretEntry.ending(of: String(repeating: "x", count: 104) + "a1b2")) == "108 characters, ending …a1b2")
        #expect(SecretEntry.ending(of: "short-value") == nil)
        #expect(SecretEntry.describe(length: 1, ending: nil) == "1 character")
        #expect(SecretEntry.oddCharacters("ab c\td\u{7}") == 3)
        #expect(SecretEntry.oddCharacters("sk-ant-oat01-abc") == 0)
    }
}
