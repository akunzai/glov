import ArgumentParser
import Foundation
import GlovCore
import GlovDaemon
import GlovDaemonAPI

@main
struct Glovd: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "glovd", abstract: "Owns the glov VMs and serves JSON-RPC on <home>/glovd.sock.")

    @Option(help: "Root directory (default: $GLOV_HOME, else ~/Library/Application Support/glov).")
    var home: String?

    func run() async throws {
        // SO_NOSIGPIPE cannot be set on an accepted socket whose client already left; a reply to it must not kill the daemon.
        signal(SIGPIPE, SIG_IGN)
        setvbuf(stdout, nil, _IOLBF, 0)  // the log file launchd gives us would otherwise buffer
        LogRotation.rotateIfNeeded()
        let home = GlovHome.resolve(explicit: home)
        let dispatcher = Dispatcher(home: home)
        let server = try DaemonServer(home: home, dispatcher: dispatcher)
        for name in await dispatcher.attachRunning() { DaemonLog.write("reattached to running VM \(name)") }
        server.start()
        DaemonLog.write("glovd listening on \(home.socketPath)")
        let wake = HostWake { Task { await dispatcher.hostWoke() } }
        if !wake.start() { DaemonLog.write("note: cannot watch for the Mac waking; guest clocks are set only after a snapshot restore") }
        if !VZConfiguration.hasVirtualizationEntitlement {
            DaemonLog.write("note: \(GlovError.missingVirtualizationEntitlement); QEMU VMs are unaffected")
        }

        // A signal ends the daemon but leaves the QEMU VMs running; the next daemon reattaches. VZ VMs run inside this
        // process, so they get a short graceful shutdown first (launchd waits 20 s before SIGKILL).
        let sources = [SIGTERM, SIGINT].map { sig -> DispatchSourceSignal in
            signal(sig, SIG_IGN)
            let s = DispatchSource.makeSignalSource(signal: sig, queue: .main)
            s.setEventHandler {
                server.stop()
                Task {
                    await dispatcher.stopInProcessVMs(timeout: .seconds(15))
                    Foundation.exit(0)
                }
            }
            s.resume()
            return s
        }
        // The sources must outlive the loop; a release build drops a bare `_ = sources` and with it the handlers.
        defer { withExtendedLifetime((sources, wake)) {} }
        while true {
            try await Task.sleep(for: .seconds(300))
            LogRotation.rotateIfNeeded()
        }
    }
}
