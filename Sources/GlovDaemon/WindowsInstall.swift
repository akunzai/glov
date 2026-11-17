import Foundation
import GlovCore
import GlovDaemonAPI

/// The timing of a Windows install, all in one place.
public struct InstallTiming: Sendable {
    /// From QEMU's start to typing the installer's boot line, blind. With the install drives attached, EDK2 first offers
    /// the CD ("Press any key to boot from CD or DVD" about 7 s after start), lets it time out, counts down for
    /// startup.nsh and leaves the UEFI shell prompt idle about 15 s after start (measured on an M-series Mac). A key
    /// sent earlier is lost, or, during the countdown, eaten by it; keys sent later wait at the prompt. So this is twice
    /// the measured time, for a busy host.
    public var bootDelay: Duration
    /// From the boot line's Enter to the key for "Press any key to boot from CD or DVD", which waits about 5 s.
    public var cdKeyDelay: Duration
    /// The install fails unless it finishes within this long of the first start. A full install takes 10 to 15 minutes.
    public var timeout: Duration
    /// How often the daemon checks whether the install has finished.
    public var poll: Duration

    public init(bootDelay: Duration, cdKeyDelay: Duration, timeout: Duration, poll: Duration) {
        self.bootDelay = bootDelay
        self.cdKeyDelay = cdKeyDelay
        self.timeout = timeout
        self.poll = poll
    }

    public static let windows = InstallTiming(bootDelay: .seconds(30), cdKeyDelay: .milliseconds(1500), timeout: .seconds(3600), poll: .seconds(2))
}

/// The install `glov vm create` prepared: boot the installer from the UEFI shell, then wait until the Guest Service
/// answers and the first-logon `setup.ps1` has finished.
enum WindowsInstall {
    enum Step: Equatable {
        case type(String)
        case key(String)
        case pause(Duration)
    }

    enum Outcome: Equatable {
        case finished
        case timedOut
        /// The VM stopped first.
        case stopped
        /// The watch was cancelled (a new watch replaced it); the install's state is left as it is.
        case cancelled
    }

    /// EDK2 maps the install ISO (the first USB drive) as fs0. Its loader then asks for a key to boot from the CD.
    static func bootSteps(_ timing: InstallTiming) -> [Step] {
        [.pause(timing.bootDelay), .type(#"fs0:\efi\boot\bootaa64.efi"#), .key("enter"), .pause(timing.cdKeyDelay), .key("space")]
    }

    /// Runs the install's host side: on the first start (`typeBoot`) the boot steps, then a poll until `finished` holds.
    /// A failed step is reported and the wait goes on: the installer can still be booted by hand (screenshot, type, key).
    static func watch(
        started: Date, timing: InstallTiming, typeBoot: Bool, now: @Sendable () -> Date = Date.init,
        send: @Sendable (Step) async throws -> Void, isRunning: @Sendable () async -> Bool, finished: @Sendable () async -> Bool,
        log: @Sendable (String) -> Void
    ) async -> Outcome {
        let deadline = started.addingTimeInterval(Double(timing.timeout.components.seconds) + Double(timing.timeout.components.attoseconds) / 1e18)
        if typeBoot {
            for step in bootSteps(timing) {
                if Task.isCancelled { return .cancelled }
                guard await isRunning() else { return .stopped }
                do {
                    if case .pause(let d) = step { try await Task.sleep(for: d) } else { try await send(step) }
                } catch {
                    if Task.isCancelled || error is CancellationError { return .cancelled }
                    log("typing the installer's boot line failed: \(error)")
                    break
                }
            }
        }
        while true {
            if Task.isCancelled { return .cancelled }
            guard await isRunning() else { return .stopped }
            if await finished() { return .finished }
            if now() >= deadline { return .timedOut }
            do { try await Task.sleep(for: timing.poll) } catch { return .cancelled }
        }
    }

    /// Where a running install is, from what the host sees: the boot line not typed yet, Setup running until the Guest
    /// Service first answers (Setup installs it), then the first sign-in's setup script until it leaves its marker.
    static func progress(started: Date, now: Date, timing: InstallTiming, guestReady: Bool) -> Results.InstallProgress {
        let elapsed = max(0, Int(now.timeIntervalSince(started)))
        let booting = Int((timing.bootDelay + timing.cdKeyDelay).components.seconds) + 1
        let phase = guestReady ? "first sign-in and setup script" : elapsed < booting ? "booting the installer" : "Windows Setup"
        return .init(phase: phase, elapsedSeconds: elapsed, limitSeconds: Int(timing.timeout.components.seconds))
    }

    /// What `vm status` reports for a VM, from whether it runs and its install.
    static func state(running: Bool, install: VMConfig.Install?) -> String {
        guard let install else { return running ? "running" : "stopped" }
        if install.failure != nil { return "install-failed" }
        if running { return "installing" }
        return install.started == nil ? "stopped" : "install-failed"
    }

    /// Why the next start must not boot the installer again, or nil when it may.
    static func refusal(_ name: String, _ install: VMConfig.Install?) -> String? {
        guard let install, install.started != nil || install.failure != nil else { return nil }
        let why = install.failure ?? "was interrupted"
        return "the Windows install of VM \(name) \(install.failure == nil ? why : "failed: " + why); an install is not resumed: delete the VM (glov vm delete \(name)) and create it again"
    }
}
