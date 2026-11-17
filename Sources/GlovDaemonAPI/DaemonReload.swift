import Foundation
import GlovCore

/// `glov host install` and `uninstall` replace the running glovd. Linux (VZ) VMs run inside it and end with it, and
/// every running job or share sync is cut off; QEMU VMs keep running and are reattached. This finds what a reload would
/// interrupt, stops Linux VMs cleanly first, and afterwards starts them again and brings Guest Services up to date. CLI only.
public struct DaemonReload: Sendable {
    public typealias RPC = @Sendable (_ method: String, _ params: JSONValue) async throws -> JSONValue

    /// What a reload interrupts.
    public struct Impact: Equatable, Sendable {
        /// Running Linux VMs: powered off by the reload.
        public var linuxVMs: [String] = []
        /// Linux VMs among them without a Guest Service, which are waited for until running rather than guest-ready.
        public var withoutGuestService: Set<String> = []
        /// Running work, as "<vm>: job j3" or "<vm>: share sync proj"; "<vm>: running work unknown" from an older daemon.
        public var work: [String] = []
        public var isEmpty: Bool { linuxVMs.isEmpty && work.isEmpty }
        public init(linuxVMs: [String] = [], withoutGuestService: Set<String> = [], work: [String] = []) {
            self.linuxVMs = linuxVMs; self.withoutGuestService = withoutGuestService; self.work = work
        }

        public var summary: String {
            var lines: [String] = []
            if !linuxVMs.isEmpty { lines.append("Linux VMs that will be shut down: \(linuxVMs.joined(separator: ", "))") }
            if !work.isEmpty { lines.append("Running work that will be cut off: \(work.joined(separator: "; "))") }
            return lines.joined(separator: "\n")
        }
    }

    /// One step's result: "ready", "updated ...", "skipped (...)" or a failure.
    public struct Outcome: Equatable, Sendable {
        public var vm: String
        public var step: String
        public var result: String
        public var failed = false
        public init(vm: String, step: String, result: String, failed: Bool = false) {
            self.vm = vm; self.step = step; self.result = result; self.failed = failed
        }
    }

    let rpc: RPC
    let log: @Sendable (String) -> Void
    var timeout: Double = 300

    public init(rpc: @escaping RPC, log: @escaping @Sendable (String) -> Void) {
        self.rpc = rpc; self.log = log
    }

    init(rpc: @escaping RPC, log: @escaping @Sendable (String) -> Void, timeout: Double) {
        self.rpc = rpc; self.log = log; self.timeout = timeout
    }

    private func call<R: Decodable>(_ method: String, _ params: some Encodable, as: R.Type = R.self) async throws -> R {
        try await rpc(method, try JSONValue(encoding: params)).decode(R.self)
    }

    private func vms() async throws -> [Results.VMInfo] { try await rpc(Method.vmList, .null).decode(Results.List.self).vms }

    /// Asks the running daemon; throws when none answers.
    public func impact() async throws -> Impact {
        var impact = Impact()
        for vm in try await vms() where vm.state == "running" || vm.state == "installing" {
            if vm.engine == "vz" {
                impact.linuxVMs.append(vm.name)
                if vm.agent?.state == "disabled" { impact.withoutGuestService.insert(vm.name) }
            }
            guard let agent = vm.agent, agent.state != "disabled" else { continue }
            if let busy = agent.busy {
                impact.work += busy.map { "\(vm.name): \($0)" }
            } else if agent.state == "ready" {
                impact.work.append("\(vm.name): running work unknown (the running daemon predates this check)")
            }
        }
        return impact
    }

    /// Stops each VM as `vm stop` does (ACPI, forced after 60 s) while the old daemon still runs it.
    public func stop(_ names: [String]) async -> [Outcome] {
        var outcomes: [Outcome] = []
        for name in names {
            log("Stopping \(name)...")
            do {
                _ = try await rpc(Method.vmStop, try JSONValue(encoding: Params.Stop(vm: name)))
                outcomes.append(.init(vm: name, step: "stop", result: "stopped"))
            } catch {
                // The reload powers it off anyway; say so rather than abandoning the install.
                outcomes.append(.init(vm: name, step: "stop", result: "failed: \(error); the reload powers it off", failed: true))
            }
        }
        return outcomes
    }

    /// Starts the VMs the reload stopped and waits until each is guest-ready.
    public func restart(_ impact: Impact) async -> [Outcome] {
        var outcomes: [Outcome] = []
        for name in impact.linuxVMs {
            log("Starting \(name)...")
            do {
                _ = try await rpc(Method.vmStart, try JSONValue(encoding: Params.VM(vm: name)))
                let state = impact.withoutGuestService.contains(name) ? "running" : "guest-ready"
                _ = try await rpc(Method.vmWait, try JSONValue(encoding: Params.Wait(vm: name, state: state, timeout: timeout)))
                log("Starting \(name)... ready")
                outcomes.append(.init(vm: name, step: "start", result: "ready"))
            } catch {
                log("Starting \(name)... failed: \(error)")
                outcomes.append(.init(vm: name, step: "start", result: "failed: \(error)", failed: true))
            }
        }
        return outcomes
    }

    /// Updates every running VM whose Guest Service is older than the bundled one, unless it is busy; never forced.
    public func updateGuestServices() async -> [Outcome] {
        let running: [Results.VMInfo]
        do { running = try await vms().filter { $0.state == "running" && $0.agent?.state == "ready" } } catch {
            return [.init(vm: "-", step: "guest update", result: "failed: cannot list VMs: \(error)", failed: true)]
        }
        var outcomes: [Outcome] = []
        for vm in running {
            do {
                let check: Results.GuestServiceCheck = try await call(Method.guestServiceCheck, Params.GuestService(vm: vm.name))
                guard check.state == "update-available" else { continue }
                let busy = vm.agent?.busy ?? []
                if !busy.isEmpty {
                    let why = "skipped (\(busy.joined(separator: ", ")) running); run 'glov vm guest update \(vm.name)' later"
                    log("Updating \(vm.name) Guest Service... \(why)")
                    outcomes.append(.init(vm: vm.name, step: "guest update", result: why))
                    continue
                }
                log("Updating \(vm.name) Guest Service...")
                let outcome: GuestServiceUpdate.Outcome = try await call(Method.guestServiceUpdate, Params.GuestService(vm: vm.name))
                let result: String
                switch outcome {
                case .updated(let from, let to): result = "updated \(from) → \(to)"
                case .upToDate(let version): result = "already \(version)"
                }
                log("Updating \(vm.name) Guest Service... \(result)")
                outcomes.append(.init(vm: vm.name, step: "guest update", result: result))
            } catch {
                // The daemon refuses an update over managed work (it is never forced here): report it as skipped.
                let message = "\(error)"
                // The running daemon may be older, so match the words every version's refusal shares.
                if message.contains("use --force to allow interruption") || message.contains("in progress") {
                    let why = "skipped (\(message)); run 'glov vm guest update \(vm.name)' later"
                    log("Updating \(vm.name) Guest Service... \(why)")
                    outcomes.append(.init(vm: vm.name, step: "guest update", result: why))
                } else {
                    log("Updating \(vm.name) Guest Service... failed: \(message)")
                    outcomes.append(.init(vm: vm.name, step: "guest update", result: "failed: \(message)", failed: true))
                }
            }
        }
        return outcomes
    }

    public enum Decision: Equatable { case proceed, declined, refused(String) }

    /// Nothing affected, or `--yes`: proceed. A terminal is asked; without one the reload is refused, naming `--yes`, so an
    /// agent or script never interrupts work it did not decide to.
    public static func decide(_ impact: Impact?, verb: String, yes: Bool, isTerminal: Bool, ask: (String) -> String?) -> Decision {
        guard let impact, !impact.isEmpty, !yes else { return .proceed }
        guard isTerminal else { return .refused("\(impact.summary)\nNot \(verb) without confirmation; rerun with --yes to proceed.") }
        return VMDeletion.isYes(ask("\(impact.summary)\n\(verb.prefix(1).uppercased() + verb.dropFirst()) anyway? [y/N] ")) ? .proceed : .declined
    }

    /// One line for stdout.
    public static func summary(_ outcomes: [Outcome]) -> String {
        guard !outcomes.isEmpty else { return "no VMs to restart or update" }
        return outcomes.map { "\($0.vm) \($0.step): \($0.result)" }.joined(separator: "; ")
    }
}
