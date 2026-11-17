import CoreGraphics
import Testing

@testable import GlovViewer

@Suite struct ViewerConnectionTests {
    @Test func inputRequiresAFrameAndNeverSurvivesADisconnect() throws {
        var state = ViewerInputState()
        #expect(state.ticket == nil)
        state.frameReceived(connectionID: "first")
        let first = try #require(state.ticket)
        #expect(state.accepts(first))
        state.disconnected()
        #expect(state.ticket == nil && !state.accepts(first))
        state.frameReceived(connectionID: "first")
        #expect(!state.accepts(first))
        let resumed = try #require(state.ticket)
        state.frameReceived(connectionID: "replacement")
        #expect(!state.accepts(resumed))
        #expect(state.accepts(try #require(state.ticket)))
    }
}

@Suite struct CoordinateTests {
    // A 200x100 frame in a 400x400 view: scale 2, picture 400x200 centred, bars of 100 above and below.
    let frame = CGSize(width: 200, height: 100)
    let view = CGSize(width: 400, height: 400)

    @Test func fitsAndCentres() {
        #expect(fittedRect(frame: frame, in: view) == CGRect(x: 0, y: 100, width: 400, height: 200))
        #expect(fittedRect(frame: frame, in: CGSize(width: 100, height: 400)) == CGRect(x: 0, y: 175, width: 100, height: 50))
        #expect(fittedRect(frame: .zero, in: view) == .zero)
    }

    @Test func mapsViewPointsToTopLeftPixels() {
        // View origin is bottom-left: y 300 is the picture's top edge.
        #expect(guestPoint(CGPoint(x: 0, y: 299.9), viewSize: view, frame: frame)! == (0, 0))
        #expect(guestPoint(CGPoint(x: 200, y: 200), viewSize: view, frame: frame)! == (100, 50))
        #expect(guestPoint(CGPoint(x: 399, y: 101), viewSize: view, frame: frame)! == (199, 99))
    }

    @Test func rejectsTheLetterboxUnlessClamping() {
        #expect(guestPoint(CGPoint(x: 10, y: 50), viewSize: view, frame: frame) == nil)
        let c = guestPoint(CGPoint(x: -50, y: 50), viewSize: view, frame: frame, clamping: true)!
        #expect(c == (0, 99))
        let d = guestPoint(CGPoint(x: 900, y: 900), viewSize: view, frame: frame, clamping: true)!
        #expect(d == (199, 0))
    }
}

@Suite struct KeyMapperTests {
    func act(_ code: UInt16, _ chars: String?, _ unmod: String? = nil, _ m: KeyModifiers = []) -> KeyAction? {
        KeyMapper.action(keyCode: code, characters: chars, unmodified: unmod ?? chars, modifiers: m)
    }

    @Test func printableTextIsTyped() {
        #expect(act(0, "a") == .type("a"))
        #expect(act(0, "A", "A", .shift) == .type("A"))
        #expect(act(18, "!", "!", .shift) == .type("!"))
    }

    @Test func namedKeysCarryModifiers() {
        #expect(act(36, "\r") == .key("enter"))
        #expect(act(123, "\u{F702}") == .key("left"))
        #expect(act(117, "\u{F728}", nil, [.control, .option]) == .key("ctrl+alt+delete"))
        #expect(act(122, "\u{F704}", nil, .shift) == .key("shift+f1"))
        #expect(act(49, " ") == .key("space"))
    }

    @Test func controlAndOptionUseTheUnmodifiedCharacter() {
        #expect(act(8, "\u{3}", "c", .control) == .key("ctrl+c"))
        #expect(act(3, "ƒ", "f", .option) == .key("alt+f"))
        #expect(act(3, "\u{6}", "F", [.control, .shift]) == .key("ctrl+shift+f"))
    }

    @Test func unsupportedInputIsDropped() {
        #expect(act(8, "c", nil, .command) == nil)
        #expect(act(0, "é") == nil)
        #expect(act(56, "") == nil)
        #expect(act(0, "\u{F700}") == nil)
    }
}

@Suite struct PacerTests {
    @Test func skipsWhileInFlightOrPaused() {
        var p = FramePacer()
        #expect(p.shouldPoll)
        p.inFlight = true
        #expect(!p.shouldPoll)
        p.inFlight = false
        p.paused = true
        #expect(!p.shouldPoll)
    }

    @Test func backsOffWhileFailing() {
        var p = FramePacer()
        #expect(p.nextDelay == .milliseconds(100))
        p.failing = true
        #expect(p.nextDelay == .seconds(1))
    }
}

@Test func inputRefusalIsRecognised() {
    struct E: Error, CustomStringConvertible { var description: String }
    #expect(describeFailure(E(description: "agent control is disabled for VM x")).inputRefused)
    #expect(!describeFailure(E(description: "VM x is not running")).inputRefused)
}
