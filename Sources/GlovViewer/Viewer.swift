import AppKit
import Foundation
import GlovCore
import GlovDaemonAPI
import ImageIO

/// Opens the viewer window for `vm` and runs the AppKit event loop until the app terminates (window closed or Quit).
///
/// Call it on the process's main thread, outside any Swift concurrency job: `NSApplication.run()` nested in a main-queue
/// job starves the main actor's other jobs (the viewer's frame loops), and Swift's own main run loop stops on its own.
public func runViewer(vm: String, client: DaemonClient, logFps: Bool, readOnly: Bool) {
    MainActor.assumeIsolated {
        let app = NSApplication.shared
        app.setActivationPolicy(.regular)
        app.applicationIconImage = AppIcon.image(.plain)
        let controller = ViewerController(vm: vm, client: client, logFps: logFps, readOnly: readOnly)
        app.delegate = controller
        app.activate()
        app.run()
    }
}

struct Frame: @unchecked Sendable {
    let image: CGImage
    /// Size of the whole screen in screenshot pixels; input coordinates are relative to it.
    let size: CGSize
    let connectionID: String?
    let payloadBytes: Int
    let roundTrip: Duration
}

/// Fetches and decodes one frame off the main thread.
func fetchFrame(vm: String, client: DaemonClient) async throws -> Frame {
    let started = ContinuousClock.now
    let reply = try await client.callWithConnectionID(Method.screenshot, try JSONValue(encoding: Params.Screenshot(vm: vm)))
    let shot = try reply.result.decode(Results.Screenshot.self)
    guard let data = Data(base64Encoded: shot.png), let src = CGImageSourceCreateWithData(data as CFData, nil),
        let image = CGImageSourceCreateImageAtIndex(src, 0, nil)
    else { throw DaemonClientError.badReply("screenshot is not a PNG") }
    return Frame(image: image, size: CGSize(width: shot.frameWidth, height: shot.frameHeight), connectionID: reply.connectionID,
                 payloadBytes: shot.png.utf8.count, roundTrip: ContinuousClock.now - started)
}

enum InputOp: Sendable {
    case move(Int, Int)
    case click(x: Int, y: Int, button: String, count: Int)
    case drag(from: (Int, Int), to: (Int, Int))
    case scroll(x: Int, y: Int, dx: Int, dy: Int)
    case action(KeyAction)
    case keyCombo(String)
}

@MainActor
final class ScreenView: NSView {
    var frameSize = CGSize.zero
    /// Called with an operation to forward; the controller decides whether input is allowed.
    var send: (InputOp) -> Void = { _ in }
    var inputTicket: () -> ViewerInputState.Ticket? = { nil }
    private var dragStart: (point: (Int, Int), ticket: ViewerInputState.Ticket)?
    private var downButton = "left"
    private var downCount = 1

    override var acceptsFirstResponder: Bool { true }
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
    override var wantsUpdateLayer: Bool { true }

    override init(frame: NSRect) {
        super.init(frame: frame)
        wantsLayer = true
        layer?.contentsGravity = .resizeAspect
        layer?.backgroundColor = NSColor.black.cgColor
        addTrackingArea(NSTrackingArea(rect: .zero, options: [.mouseMoved, .activeInKeyWindow, .inVisibleRect], owner: self))
    }
    required init?(coder: NSCoder) { fatalError() }

    private func point(_ e: NSEvent, clamping: Bool = false) -> (x: Int, y: Int)? {
        guestPoint(convert(e.locationInWindow, from: nil), viewSize: bounds.size, frame: frameSize, clamping: clamping)
    }

    override func mouseMoved(with e: NSEvent) { if let p = point(e) { send(.move(p.x, p.y)) } }
    override func mouseDown(with e: NSEvent) { begin(e, "left") }
    override func rightMouseDown(with e: NSEvent) { begin(e, "right") }
    override func otherMouseDown(with e: NSEvent) { begin(e, "middle") }
    override func mouseUp(with e: NSEvent) { end(e) }
    override func rightMouseUp(with e: NSEvent) { end(e) }
    override func otherMouseUp(with e: NSEvent) { end(e) }

    private func begin(_ e: NSEvent, _ button: String) {
        window?.makeFirstResponder(self)
        guard let p = point(e), let ticket = inputTicket() else { dragStart = nil; return }
        dragStart = ((p.x, p.y), ticket)
        downButton = button
        downCount = max(1, e.clickCount)
    }

    /// A press and release close together is a click; otherwise it is a drag from where it began to where it ended.
    private func end(_ e: NSEvent) {
        guard let start = dragStart, start.ticket == inputTicket(), let p = point(e, clamping: true) else { dragStart = nil; return }
        let s = start.point
        dragStart = nil
        if abs(p.x - s.0) + abs(p.y - s.1) < 4 {
            send(.click(x: s.0, y: s.1, button: downButton, count: downCount))
        } else {
            send(.drag(from: s, to: (p.x, p.y)))
        }
    }

    override func scrollWheel(with e: NSEvent) {
        guard let p = point(e) else { return }
        // The daemon takes wheel ticks, positive down; macOS deltas are positive up. Trackpads report points, so scale them.
        let k: CGFloat = e.hasPreciseScrollingDeltas ? 1.0 / 10 : 1
        let dy = Int((-e.scrollingDeltaY * k).rounded()), dx = Int((-e.scrollingDeltaX * k).rounded())
        if dx != 0 || dy != 0 { send(.scroll(x: p.x, y: p.y, dx: dx, dy: dy)) }
    }

    override func keyDown(with e: NSEvent) {
        var m: KeyModifiers = []
        let f = e.modifierFlags
        if f.contains(.shift) { m.insert(.shift) }
        if f.contains(.control) { m.insert(.control) }
        if f.contains(.option) { m.insert(.option) }
        if f.contains(.command) { m.insert(.command) }
        let unmodified = e.charactersIgnoringModifiers
        if let a = KeyMapper.action(keyCode: e.keyCode, characters: e.characters, unmodified: unmodified, modifiers: m) { send(.action(a)) }
    }
    // Keys the system would otherwise beep for or treat as shortcuts.
    override func performKeyEquivalent(with e: NSEvent) -> Bool { false }
}

@MainActor
final class ViewerController: NSObject, NSApplicationDelegate, NSWindowDelegate {
    let vm: String
    let client: DaemonClient
    let logFps: Bool
    var readOnly: Bool
    var inputRefused = false
    var pacer = FramePacer()
    let window: NSWindow
    let screen = ScreenView(frame: NSRect(x: 0, y: 0, width: 960, height: 600))
    let banner = NSTextField(labelWithString: "")
    var readOnlyItem: NSMenuItem?
    var frames = 0
    var lastLog = ContinuousClock.now
    var queued = 0
    var inputState = ViewerInputState()
    var payloadBytes = 0
    var frameTime = Duration.zero
    var inputTime = Duration.zero
    var inputCount = 0
    let ops: AsyncStream<(ViewerInputState.Ticket, InputOp)>
    let opsIn: AsyncStream<(ViewerInputState.Ticket, InputOp)>.Continuation

    init(vm: String, client: DaemonClient, logFps: Bool, readOnly: Bool) {
        self.vm = vm; self.client = client; self.logFps = logFps; self.readOnly = readOnly
        (ops, opsIn) = AsyncStream.makeStream()
        window = NSWindow(
            contentRect: screen.frame, styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        super.init()
    }

    func applicationDidFinishLaunching(_ n: Notification) {
        window.title = "\(vm)\(client.remoteHost.map { " @ " + $0 } ?? "") - glov"
        window.contentView = screen
        window.delegate = self
        window.isReleasedWhenClosed = false
        window.center()
        banner.textColor = .white
        banner.backgroundColor = NSColor.black.withAlphaComponent(0.75)
        banner.drawsBackground = true
        banner.alignment = .center
        banner.translatesAutoresizingMaskIntoConstraints = false
        banner.isHidden = true
        screen.addSubview(banner)
        NSLayoutConstraint.activate([
            banner.leadingAnchor.constraint(equalTo: screen.leadingAnchor), banner.trailingAnchor.constraint(equalTo: screen.trailingAnchor),
            banner.topAnchor.constraint(equalTo: screen.topAnchor),
        ])
        screen.send = { [weak self] op in self?.forward(op) }
        screen.inputTicket = { [weak self] in
            guard let self, !self.readOnly, !self.inputRefused else { return nil }
            return self.inputState.ticket
        }
        installMenu()
        let nc = NotificationCenter.default
        for name in [NSWindow.didMiniaturizeNotification, NSWindow.didDeminiaturizeNotification, NSWindow.didChangeOcclusionStateNotification] {
            nc.addObserver(forName: name, object: window, queue: .main) { [weak self] _ in MainActor.assumeIsolated { self?.updatePaused() } }
        }
        window.makeKeyAndOrderFront(nil)
        window.makeFirstResponder(screen)
        refreshBanner()
        Task { await sendLoop() }
        Task { await pollLoop() }
        Task { await badgeIcon() }
    }

    /// Replaces the plain Dock icon with the badge for the VM's guest OS, so two viewers are told apart at a glance.
    func badgeIcon() async {
        guard let info = try? await client.call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self) else { return }
        let variant = AppIcon.variant(engine: info.engine)
        if variant != .plain, let image = AppIcon.image(variant) { NSApplication.shared.applicationIconImage = image }
    }

    // Closing the window ends the viewer; the VM belongs to glovd and is untouched.
    func applicationShouldTerminateAfterLastWindowClosed(_ app: NSApplication) -> Bool { true }

    func updatePaused() {
        pacer.paused = window.isMiniaturized || !window.occlusionState.contains(.visible)
    }

    // MARK: Frames

    func pollLoop() async {
        while !Task.isCancelled {
            if pacer.shouldPoll {
                pacer.inFlight = true
                let started = ContinuousClock.now
                do {
                    let frame = try await fetchFrame(vm: vm, client: client)
                    show(frame)
                    pacer.failing = false
                } catch {
                    if logFps, !pacer.failing { FileHandle.standardError.write(Data("viewer: disconnected: \(error)\n".utf8)) }
                    pacer.failing = true
                    inputState.disconnected()
                    screen.layer?.opacity = 0.45
                    setProblem(describeFailure(error).text)
                }
                pacer.inFlight = false
                let rest = pacer.nextDelay - (ContinuousClock.now - started)
                if rest > .zero { try? await Task.sleep(for: rest) }
            } else {
                try? await Task.sleep(for: .milliseconds(50))
            }
        }
    }

    func show(_ frame: Frame) {
        if logFps, pacer.failing { FileHandle.standardError.write(Data("viewer: frame stream recovered\n".utf8)) }
        inputState.frameReceived(connectionID: frame.connectionID)
        screen.layer?.opacity = 1
        if screen.frameSize != frame.size {
            screen.frameSize = frame.size
            window.contentAspectRatio = frame.size
            let scale = min(1, 1280 / frame.size.width)
            window.setContentSize(NSSize(width: frame.size.width * scale, height: frame.size.height * scale))
        }
        screen.layer?.contents = frame.image
        problem = nil
        refreshBanner()
        frames += 1
        payloadBytes += frame.payloadBytes
        frameTime += frame.roundTrip
        let now = ContinuousClock.now
        if logFps, now - lastLog >= .seconds(3) {
            func seconds(_ duration: Duration) -> Double { Double(duration.components.seconds) + Double(duration.components.attoseconds) / 1e18 }
            let secs = seconds(now - lastLog)
            let input = inputCount == 0 ? "n/a" : String(format: "%.0f ms", 1000 * seconds(inputTime) / Double(inputCount))
            FileHandle.standardError.write(Data(String(format: "viewer: %.1f fps, %.2f MiB/s frame payload, %.0f ms frame RTT, input RPC %@ (%dx%d)\n",
                Double(frames) / secs, Double(payloadBytes) / secs / 1_048_576, 1000 * seconds(frameTime) / Double(frames), input, Int(frame.size.width), Int(frame.size.height)).utf8))
            frames = 0
            payloadBytes = 0; frameTime = .zero; inputTime = .zero; inputCount = 0
            lastLog = now
        }
    }

    // MARK: Status banner

    var problem: String?
    func setProblem(_ text: String) { problem = text; refreshBanner() }

    func refreshBanner() {
        let text = problem ?? (inputRefused ? "Input refused: agent control is off for this VM (view only)" : readOnly ? "Read-only: input is not forwarded" : nil)
        banner.stringValue = text.map { " \($0) " } ?? ""
        banner.isHidden = text == nil
    }

    // MARK: Input

    func forward(_ op: InputOp) {
        guard !readOnly, !inputRefused, let ticket = inputState.ticket else { return }
        // Pointer moves are only hints: drop them while earlier input is still being delivered.
        if case .move = op, queued > 0 { return }
        queued += 1
        opsIn.yield((ticket, op))
    }

    /// Delivers input in order, one call at a time.
    func sendLoop() async {
        for await (ticket, op) in ops {
            defer { queued -= 1 }
            guard !readOnly, !inputRefused, inputState.accepts(ticket) else { continue }
            let started = ContinuousClock.now
            do {
                try await deliver(op, connectionID: ticket.connectionID)
                inputTime += ContinuousClock.now - started
                inputCount += 1
            } catch {
                let f = describeFailure(error)
                if f.inputRefused { inputRefused = true } else { inputState.disconnected(); setProblem(f.text) }
                refreshBanner()
            }
        }
    }

    func deliver(_ op: InputOp, connectionID: String?) async throws {
        let vm = vm
        func call<P: Encodable>(_ method: String, _ p: P) async throws {
            _ = try await client.call(method, try JSONValue(encoding: p), expectedConnectionID: connectionID)
        }
        switch op {
        case .move(let x, let y): try await call(Method.move, Params.Move(vm: vm, x: x, y: y))
        case .click(let x, let y, let b, let n): try await call(Method.click, Params.Click(vm: vm, x: x, y: y, button: b, count: n))
        case .drag(let f, let t): try await call(Method.drag, Params.Drag(vm: vm, fromX: f.0, fromY: f.1, toX: t.0, toY: t.1))
        case .scroll(let x, let y, let dx, let dy): try await call(Method.scroll, Params.Scroll(vm: vm, x: x, y: y, dx: dx, dy: dy))
        case .action(.type(let s)): try await call(Method.type, Params.TypeText(vm: vm, text: s))
        case .action(.key(let k)), .keyCombo(let k): try await call(Method.key, Params.Key(vm: vm, key: k))
        }
    }

    // MARK: Menu

    func installMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem(); main.addItem(appItem)
        let appMenu = NSMenu(); appItem.submenu = appMenu
        appMenu.addItem(withTitle: "Quit Viewer", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        let inputItem = NSMenuItem(); main.addItem(inputItem)
        let inputMenu = NSMenu(title: "Input"); inputItem.submenu = inputMenu
        let cad = inputMenu.addItem(withTitle: "Send Ctrl+Alt+Delete", action: #selector(sendCAD), keyEquivalent: "")
        cad.target = self
        let ro = inputMenu.addItem(withTitle: "Read-only", action: #selector(toggleReadOnly), keyEquivalent: "")
        ro.target = self
        ro.state = readOnly ? .on : .off
        readOnlyItem = ro
        NSApplication.shared.mainMenu = main
    }

    @objc func sendCAD() { forward(.keyCombo("ctrl+alt+delete")) }

    @objc func toggleReadOnly() {
        readOnly.toggle()
        inputState.disconnected()
        readOnlyItem?.state = readOnly ? .on : .off
        refreshBanner()
    }
}
