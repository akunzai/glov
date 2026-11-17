import CoreGraphics
import Foundation
import GlovDaemonAPI

/// Input belongs to the frame the user saw; a disconnect or replacement connection invalidates queued events.
struct ViewerInputState {
    struct Ticket: Equatable, Sendable {
        let generation: Int
        let connectionID: String?
    }
    private var generation = 0
    private var connected = false
    private var connectionID: String?
    var ticket: Ticket? { connected ? Ticket(generation: generation, connectionID: connectionID) : nil }

    mutating func frameReceived(connectionID: String?) {
        if connected && self.connectionID != connectionID { generation += 1 }
        self.connectionID = connectionID
        connected = true
    }

    mutating func disconnected() { connected = false; generation += 1 }
    func accepts(_ ticket: Ticket) -> Bool { self.ticket == ticket }
}

/// Where the frame sits inside a view of `viewSize`, scaled to fit and centred (letterboxed).
func fittedRect(frame: CGSize, in viewSize: CGSize) -> CGRect {
    guard frame.width > 0, frame.height > 0, viewSize.width > 0, viewSize.height > 0 else { return .zero }
    let scale = min(viewSize.width / frame.width, viewSize.height / frame.height)
    let size = CGSize(width: frame.width * scale, height: frame.height * scale)
    return CGRect(x: (viewSize.width - size.width) / 2, y: (viewSize.height - size.height) / 2, width: size.width, height: size.height)
}

/// A point in a bottom-left-origin view to screenshot pixels (top-left origin), in the daemon's input coordinates.
/// Outside the picture it returns nil, or with `clamping` the nearest edge pixel.
func guestPoint(_ p: CGPoint, viewSize: CGSize, frame: CGSize, clamping: Bool = false) -> (x: Int, y: Int)? {
    let r = fittedRect(frame: frame, in: viewSize)
    guard r.width > 0, r.height > 0 else { return nil }
    if !clamping && !r.contains(p) { return nil }
    let fx = min(1, max(0, (p.x - r.minX) / r.width))
    let fy = min(1, max(0, (r.maxY - p.y) / r.height))
    return (min(Int(frame.width) - 1, Int(fx * frame.width)), min(Int(frame.height) - 1, Int(fy * frame.height)))
}

/// Decides when to ask the daemon for the next frame: never while a request is in flight or the window is hidden.
struct FramePacer {
    var inFlight = false
    var paused = false
    /// Polling interval; 100 ms is 10 fps at best.
    var interval: Duration = .milliseconds(100)
    /// Interval while the daemon or VM is unavailable, so a stopped VM is not hammered.
    var errorInterval: Duration = .seconds(1)
    var failing = false

    var shouldPoll: Bool { !inFlight && !paused }
    var nextDelay: Duration { failing ? errorInterval : interval }
}

enum KeyAction: Equatable {
    /// `input.type`: printable text.
    case type(String)
    /// `input.key`: a combo such as `ctrl+alt+delete` or `enter`.
    case key(String)
}

struct KeyModifiers: OptionSet {
    let rawValue: Int
    static let shift = KeyModifiers(rawValue: 1)
    static let control = KeyModifiers(rawValue: 2)
    static let option = KeyModifiers(rawValue: 4)
    static let command = KeyModifiers(rawValue: 8)
}

enum KeyMapper {
    /// macOS virtual key codes of the non-printing keys, to the daemon's key names.
    static let special: [UInt16: String] = [
        36: "enter", 76: "enter", 48: "tab", 49: "space", 51: "backspace", 117: "delete", 53: "esc",
        114: "insert", 115: "home", 119: "end", 116: "pageup", 121: "pagedown",
        123: "left", 124: "right", 125: "down", 126: "up",
        122: "f1", 120: "f2", 99: "f3", 118: "f4", 96: "f5", 97: "f6", 98: "f7", 100: "f8", 101: "f9", 109: "f10", 103: "f11", 111: "f12",
    ]

    static func isPrintableASCII(_ s: String) -> Bool { !s.isEmpty && s.utf8.allSatisfy { $0 >= 0x20 && $0 < 0x7f } }

    /// What to send for a key press, or nil when the daemon cannot express it (Command shortcuts, non-ASCII, bare modifiers).
    /// `unmodified` is the key's character ignoring Control and Option, so Option+F is "f", not "ƒ".
    static func action(keyCode: UInt16, characters: String?, unmodified: String?, modifiers: KeyModifiers) -> KeyAction? {
        if modifiers.contains(.command) { return nil }
        var parts: [String] = []
        if modifiers.contains(.control) { parts.append("ctrl") }
        if modifiers.contains(.option) { parts.append("alt") }
        if modifiers.contains(.shift) { parts.append("shift") }
        if let name = special[keyCode] { return .key((parts + [name]).joined(separator: "+")) }
        if modifiers.contains(.control) || modifiers.contains(.option) {
            guard let u = unmodified?.lowercased(), u.count == 1, isPrintableASCII(u) else { return nil }
            return .key((parts + [u]).joined(separator: "+"))
        }
        guard let c = characters, isPrintableASCII(c) else { return nil }
        return .type(c)
    }
}

/// A message for the window when a daemon call fails, and whether it means input is refused rather than the VM or daemon being away.
func describeFailure(_ error: Error) -> (text: String, inputRefused: Bool) {
    let text = "\(error)"
    return (text, text.contains("agent control is disabled"))
}
