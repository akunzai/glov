import ArgumentParser
import GlovViewer
import GlovDaemonAPI

/// Synchronous on purpose: `main.swift` runs it on the main thread before Swift concurrency's main executor starts,
/// because AppKit's run loop cannot share the main thread with it.
struct View: ParsableCommand {
    static let configuration = CommandConfiguration(
        abstract: "Watch and control a running VM in a window. A plain client of glovd: closing it never affects the VM.")
    @Argument(help: "VM name or unique prefix.") var vm: String
    @Option(help: "glov root (default: $GLOV_HOME or ~/Library/Application Support/glov).") var home: String?
    @Flag(help: "Print frame rate, payload bandwidth and frame/input RPC timing every few seconds.") var logFps = false
    @Flag(help: "Start with input forwarding off.") var readOnly = false

    func run() throws {
        let client = remoteDaemon == nil ? DaemonClient(socketPath: GlovHome.resolve(explicit: home).socketPath) : daemonClient()
        runViewer(vm: vm, client: client, logFps: logFps, readOnly: readOnly)
    }
}
