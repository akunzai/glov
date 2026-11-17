import AppKit
import Carbon.HIToolbox
import IOSurface
import Virtualization

/// Pure mappings from agent-facing input to the NSEvents a `VZVirtualMachineView` turns into USB keyboard and pointer reports.
enum VZInput {
    /// NSEvent.ModifierFlags plus the left-hand NX_DEVICEL*KEYMASK bit: VZ ignores a modifier without its device bit.
    static let shift: UInt = NSEvent.ModifierFlags.shift.rawValue | 0x02
    static let ctrl: UInt = NSEvent.ModifierFlags.control.rawValue | 0x01
    static let alt: UInt = NSEvent.ModifierFlags.option.rawValue | 0x20
    static let command: UInt = NSEvent.ModifierFlags.command.rawValue | 0x08

    struct Stroke: Equatable {
        var keyCode: UInt16
        var down: Bool
        /// A modifier goes as a flagsChanged event, anything else as keyDown/keyUp.
        var isModifier: Bool
        /// The modifier flags in effect after this stroke.
        var flags: UInt
    }

    static let modifiers: [String: (keyCode: Int, flags: UInt)] = [
        "shift": (kVK_Shift, shift), "ctrl": (kVK_Control, ctrl), "alt": (kVK_Option, alt),
        // The guest sees Command as its Super/Windows key.
        "meta_l": (kVK_Command, command), "caps_lock": (kVK_CapsLock, NSEvent.ModifierFlags.capsLock.rawValue),
    ]

    /// Mac virtual key codes for the qcodes `Input` produces (a US layout, like QEMU's send-key).
    static let keyCodes: [String: Int] = [
        "a": kVK_ANSI_A, "b": kVK_ANSI_B, "c": kVK_ANSI_C, "d": kVK_ANSI_D, "e": kVK_ANSI_E, "f": kVK_ANSI_F, "g": kVK_ANSI_G,
        "h": kVK_ANSI_H, "i": kVK_ANSI_I, "j": kVK_ANSI_J, "k": kVK_ANSI_K, "l": kVK_ANSI_L, "m": kVK_ANSI_M, "n": kVK_ANSI_N,
        "o": kVK_ANSI_O, "p": kVK_ANSI_P, "q": kVK_ANSI_Q, "r": kVK_ANSI_R, "s": kVK_ANSI_S, "t": kVK_ANSI_T, "u": kVK_ANSI_U,
        "v": kVK_ANSI_V, "w": kVK_ANSI_W, "x": kVK_ANSI_X, "y": kVK_ANSI_Y, "z": kVK_ANSI_Z,
        "0": kVK_ANSI_0, "1": kVK_ANSI_1, "2": kVK_ANSI_2, "3": kVK_ANSI_3, "4": kVK_ANSI_4, "5": kVK_ANSI_5, "6": kVK_ANSI_6,
        "7": kVK_ANSI_7, "8": kVK_ANSI_8, "9": kVK_ANSI_9,
        "minus": kVK_ANSI_Minus, "equal": kVK_ANSI_Equal, "bracket_left": kVK_ANSI_LeftBracket, "bracket_right": kVK_ANSI_RightBracket,
        "backslash": kVK_ANSI_Backslash, "semicolon": kVK_ANSI_Semicolon, "apostrophe": kVK_ANSI_Quote, "comma": kVK_ANSI_Comma,
        "dot": kVK_ANSI_Period, "slash": kVK_ANSI_Slash, "grave_accent": kVK_ANSI_Grave,
        "spc": kVK_Space, "ret": kVK_Return, "tab": kVK_Tab, "esc": kVK_Escape, "backspace": kVK_Delete, "delete": kVK_ForwardDelete,
        "insert": kVK_Help, "home": kVK_Home, "end": kVK_End, "pgup": kVK_PageUp, "pgdn": kVK_PageDown,
        "up": kVK_UpArrow, "down": kVK_DownArrow, "left": kVK_LeftArrow, "right": kVK_RightArrow, "menu": kVK_ContextualMenu,
        "f1": kVK_F1, "f2": kVK_F2, "f3": kVK_F3, "f4": kVK_F4, "f5": kVK_F5, "f6": kVK_F6, "f7": kVK_F7, "f8": kVK_F8,
        "f9": kVK_F9, "f10": kVK_F10, "f11": kVK_F11, "f12": kVK_F12,
    ]

    /// The qcodes pressed in order (a repeat pressed once), then released in reverse.
    static func strokes(_ qcodes: [String]) throws -> [Stroke] {
        var seen = Set<String>()
        let unique = qcodes.filter { seen.insert($0).inserted }
        var flags: UInt = 0
        var downs: [Stroke] = []
        for q in unique {
            if let m = modifiers[q] {
                flags |= m.flags
                downs.append(Stroke(keyCode: UInt16(m.keyCode), down: true, isModifier: true, flags: flags))
            } else if let code = keyCodes[q] {
                downs.append(Stroke(keyCode: UInt16(code), down: true, isModifier: false, flags: flags))
            } else {
                throw GlovError.unknownKey(q)
            }
        }
        var ups: [Stroke] = []
        for (q, down) in zip(unique, downs).reversed() {
            if let m = modifiers[q] { flags &= ~m.flags }
            ups.append(Stroke(keyCode: down.keyCode, down: false, isModifier: down.isModifier, flags: down.isModifier ? flags : down.flags))
        }
        return downs + ups
    }

    /// The centre of pixel (x, y) of a screenshot `frame` big, as a point of a view `view` big (AppKit's origin is bottom left).
    static func viewPoint(x: Int, y: Int, frame: PixelSize, view: CGSize) -> CGPoint {
        func scale(_ v: Int, _ extent: Int, _ size: CGFloat) -> CGFloat {
            guard extent > 0 else { return 0 }
            return min(size, max(0, (CGFloat(v) + 0.5) / CGFloat(extent) * size))
        }
        return CGPoint(x: scale(x, frame.width, view.width), y: view.height - scale(y, frame.height, view.height))
    }

    /// Pixels to scroll for a QEMU wheel button: positive `dy` up, positive `dx` right. VZ hands pixel deltas to the guest
    /// as 1/120-notch wheel units, so 120 is one notch (one REL_WHEEL step in Linux; fewer never reach a non-hi-res reader).
    static func wheel(_ button: String) -> (dy: Int32, dx: Int32)? {
        switch button {
        case "wheel-up": (120, 0)
        case "wheel-down": (-120, 0)
        case "wheel-left": (0, -120)
        case "wheel-right": (0, 120)
        default: nil
        }
    }
}

/// The screen of a VZ guest. Virtualization.framework has no capture or input API, so a `VZVirtualMachineView` in a window
/// that is never shown does both: its layer holds the guest framebuffer as an IOSurface we read, and NSEvents handed straight
/// to the view reach the guest's USB keyboard and pointer. Nothing goes through the window server's event or capture paths,
/// so no Screen Recording or Accessibility permission is involved. Needs the main thread to run a run loop (an async main does).
@MainActor
final class VZScreen: ScreenDevice {
    private let window: NSWindow
    private let view: VZVirtualMachineView
    private var point = CGPoint.zero
    private var held: Set<String> = []
    private let attached = ContinuousClock.now

    init(machine: VZVirtualMachine, size: CGSize) {
        view = VZVirtualMachineView(frame: CGRect(origin: .zero, size: size))
        view.virtualMachine = machine
        window = NSWindow(contentRect: view.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.contentView = view
    }

    func close() {
        view.virtualMachine = nil
        window.close()
    }

    func capture() throws -> CGImage {
        guard let surface = Self.surface(in: view.layer) else { throw Self.noFramebuffer(after: ContinuousClock.now - attached) }
        return try Self.image(from: surface)
    }

    func move(x: Int, y: Int, frame: PixelSize) {
        point = VZInput.viewPoint(x: x, y: y, frame: frame, view: view.bounds.size)
        if held.contains("left") { view.mouseDragged(with: mouse(.leftMouseDragged)) }
        else if held.contains("right") { view.rightMouseDragged(with: mouse(.rightMouseDragged, button: 1)) }
        else if held.contains("middle") { view.otherMouseDragged(with: mouse(.otherMouseDragged, button: 2)) }
        else { view.mouseMoved(with: mouse(.mouseMoved)) }
    }

    func button(_ name: String, down: Bool) throws {
        if let wheel = VZInput.wheel(name) {
            guard down, let cg = CGEvent(scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 2, wheel1: wheel.dy, wheel2: wheel.dx, wheel3: 0),
                let e = NSEvent(cgEvent: cg)
            else { return }
            view.scrollWheel(with: e)
            return
        }
        if down { held.insert(name) } else { held.remove(name) }
        switch (name, down) {
        case ("left", true): view.mouseDown(with: mouse(.leftMouseDown))
        case ("left", false): view.mouseUp(with: mouse(.leftMouseUp))
        case ("right", true): view.rightMouseDown(with: mouse(.rightMouseDown, button: 1))
        case ("right", false): view.rightMouseUp(with: mouse(.rightMouseUp, button: 1))
        case ("middle", true): view.otherMouseDown(with: mouse(.otherMouseDown, button: 2))
        case ("middle", false): view.otherMouseUp(with: mouse(.otherMouseUp, button: 2))
        default: throw GlovError.unknownKey(name)
        }
    }

    func keys(_ qcodes: [String]) async throws {
        let strokes = try VZInput.strokes(qcodes)
        for s in strokes where s.down { send(s) }
        try await Task.sleep(for: .milliseconds(20))  // QEMU's send-key hold time
        for s in strokes where !s.down { send(s) }
    }

    private func send(_ s: VZInput.Stroke) {
        let type: NSEvent.EventType = s.isModifier ? .flagsChanged : s.down ? .keyDown : .keyUp
        // VZ reads the key code and flags; characters do not matter.
        guard let e = NSEvent.keyEvent(
            with: type, location: .zero, modifierFlags: NSEvent.ModifierFlags(rawValue: s.flags), timestamp: ProcessInfo.processInfo.systemUptime,
            windowNumber: window.windowNumber, context: nil, characters: "", charactersIgnoringModifiers: "", isARepeat: false, keyCode: s.keyCode)
        else { return }
        switch type {
        case .keyDown: view.keyDown(with: e)
        case .keyUp: view.keyUp(with: e)
        default: view.flagsChanged(with: e)
        }
    }

    private func mouse(_ type: NSEvent.EventType, button: Int = 0) -> NSEvent {
        let e = NSEvent.mouseEvent(
            with: type, location: point, modifierFlags: [], timestamp: ProcessInfo.processInfo.systemUptime, windowNumber: window.windowNumber,
            context: nil, eventNumber: 0, clickCount: 1, pressure: held.isEmpty ? 0 : 1)!
        // VZ goes by buttonNumber, which NSEvent.mouseEvent leaves at 0 (left); only the CGEvent form can set it.
        guard button != 0, let cg = e.cgEvent else { return e }
        cg.setIntegerValueField(.mouseEventButtonNumber, value: Int64(button))
        return NSEvent(cgEvent: cg) ?? e
    }

    /// Early in boot the guest may not have drawn yet. Long after, the likelier cause is a macOS update that changed the
    /// view's private layer tree, which `surface(in:)` relies on, so say so rather than blame the guest.
    nonisolated static func noFramebuffer(after elapsed: Duration) -> GlovError {
        guard elapsed >= .seconds(30) else { return .screenshotFailed("the guest has not drawn its screen yet") }
        return .screenshotFailed(
            "no guest framebuffer in the VZ view after \(Int(elapsed.components.seconds)) s: VZVirtualMachineView's layer tree may have"
                + " changed in a macOS update (see VZScreen.surface(in:) in Sources/GlovCore/VZScreen.swift)")
    }

    /// The framebuffer surface somewhere in the view's layer tree (the layout is VZ's own, so it is searched rather than assumed).
    private static func surface(in layer: CALayer?) -> IOSurfaceRef? {
        guard let layer else { return nil }
        if let contents = layer.contents, CFGetTypeID(contents as CFTypeRef) == IOSurfaceGetTypeID() { return (contents as! IOSurfaceRef) }
        for sublayer in layer.sublayers ?? [] { if let s = surface(in: sublayer) { return s } }
        return nil
    }

    /// A copy of a 32-bit BGRA surface, the format VZ draws virtio-gpu scanouts in.
    nonisolated static func image(from surface: IOSurfaceRef) throws -> CGImage {
        let format = IOSurfaceGetPixelFormat(surface)
        guard format == 0x4247_5241 else {  // 'BGRA'
            throw GlovError.screenshotFailed(String(format: "unsupported framebuffer pixel format %08x", format))
        }
        IOSurfaceLock(surface, .readOnly, nil)
        let (w, h, bpr) = (IOSurfaceGetWidth(surface), IOSurfaceGetHeight(surface), IOSurfaceGetBytesPerRow(surface))
        let data = Data(bytes: IOSurfaceGetBaseAddress(surface), count: bpr * h)
        IOSurfaceUnlock(surface, .readOnly, nil)
        guard w > 0, h > 0, let provider = CGDataProvider(data: data as CFData),
            let image = CGImage(
                width: w, height: h, bitsPerComponent: 8, bitsPerPixel: 32, bytesPerRow: bpr, space: CGColorSpace(name: CGColorSpace.sRGB)!,
                bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.noneSkipFirst.rawValue | CGBitmapInfo.byteOrder32Little.rawValue),
                provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent)
        else { throw GlovError.screenshotFailed("cannot read the \(w)x\(h) framebuffer") }
        return image
    }
}
