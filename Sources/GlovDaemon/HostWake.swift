import Foundation
import IOKit.pwr_mgt

/// Calls `woke` each time the Mac finishes waking from sleep, dark wakes included: a QEMU guest's clock stands still
/// while the Mac sleeps. Sleep requests are acknowledged at once, so the daemon never delays the Mac's sleep.
/// https://developer.apple.com/library/archive/qa/qa1340/_index.html
public final class HostWake: @unchecked Sendable {
    // iokit_common_msg(...) from IOKit/IOMessage.h; the function-like macros are not imported into Swift.
    private static let canSystemSleep: UInt32 = 0xE000_0270
    private static let systemWillSleep: UInt32 = 0xE000_0280
    private static let systemHasPoweredOn: UInt32 = 0xE000_0300

    private let woke: @Sendable () -> Void
    private let queue = DispatchQueue(label: "org.glov.host-wake")
    private var rootPort: io_connect_t = 0
    private var notifyPort: IONotificationPortRef?
    private var notifier: io_object_t = 0

    public init(woke: @escaping @Sendable () -> Void) { self.woke = woke }

    /// Returns false when the power notification cannot be registered; guest clocks are then set only after a restore.
    @discardableResult public func start() -> Bool {
        let refcon = Unmanaged.passUnretained(self).toOpaque()
        rootPort = IORegisterForSystemPower(refcon, &notifyPort, { refcon, _, message, argument in
            guard let refcon else { return }
            Unmanaged<HostWake>.fromOpaque(refcon).takeUnretainedValue().handle(message, argument)
        }, &notifier)
        guard rootPort != 0, let notifyPort else { return false }
        IONotificationPortSetDispatchQueue(notifyPort, queue)
        return true
    }

    private func handle(_ message: UInt32, _ argument: UnsafeMutableRawPointer?) {
        switch message {
        case Self.canSystemSleep, Self.systemWillSleep: IOAllowPowerChange(rootPort, Int(bitPattern: argument))
        case Self.systemHasPoweredOn: woke()
        default: break
        }
    }
}
