import Foundation

/// `glov claude --herdr`: a project VM's harness sessions in herdr. The Mac's herdr-core (`hc`) reaches the VM as a
/// machine through `glov ssh <vm> --` and runs the harness in a terminal there, which herdr-app shows in a space of
/// that machine. Pure parts, so they are tested; the CLI runs `hc` and `herdr-app`.
public enum HerdrView {
    /// What the guest's hc can make the Mac's hc and herdr-app do once the VM is a machine (herdr-core trusts a
    /// machine it adds), printed each time.
    public static let warning = """
        warning: with --herdr the VM is a herdr-core machine, and herdr-core trusts its machines. A hostile guest can
          - type text and Enter into any Mac terminal that started a terminal in the VM (hc run --on <vm>): glov's
            own sessions start without a parent, but do not run hc run --on <vm> from a Mac shell or agent;
          - make herdr-app open spaces, tabs and panes, show statuses, sounds and toasts;
          - write the Mac's clipboard (herdr-app setting clipboard_write: false stops it);
          - claim it needs a login: never type a Mac password or code for this machine.
        hc's notes = false keeps it from typing notes into agents that are parents. See docs/isolated-harness-vms.md#in-herdr.
        """

    /// The `hc machine add --via` command: this glov's `ssh <vm> --`, with the glov home when it is not the default.
    public static func via(glov: String, home: String?, vm: String) -> String {
        let q = ProjectAgent.shellQuote
        return (home.map { "env GLOV_HOME=\(q($0)) " } ?? "") + "\(q(glov)) ssh \(q(vm)) --"
    }

    /// Whether an `hc machine ls` entry is this VM's: named after it and reached through glov's ssh to it.
    public static func isVM(name: String, target: String, vm: String) -> Bool {
        name == vm && target.hasSuffix(" ssh \(ProjectAgent.shellQuote(vm)) --")
    }

    public struct Machine: Decodable, Equatable, Sendable {
        public var name: String
        public var target: String
    }

    /// `hc machine ls --json`.
    public static func machines(_ json: Data) -> [Machine] {
        struct List: Decodable { var machines: [Machine] }
        return (try? JSONDecoder().decode(List.self, from: json))?.machines ?? []
    }

    /// The names of the live terminals on machine `vm` in `hc ls --json`.
    public static func liveNames(_ json: Data, vm: String) -> Set<String> {
        struct Terminal: Decodable { var address: String; var name: String?; var state: String }
        struct List: Decodable { var terminals: [Terminal] }
        let all = (try? JSONDecoder().decode(List.self, from: json))?.terminals ?? []
        return Set(all.filter { $0.state == "live" && $0.address.hasPrefix(vm + "/") }.compactMap(\.name))
    }

    /// What a harness session's terminal runs now. A VM restart replaces the harness glov started with a shell (hc's
    /// `--keep`; herdr-app always keeps its terminals), and herdr-app's tabs also drop to a shell when the harness
    /// exits, so a live terminal of the harness's name is not proof that the harness runs there.
    public enum Occupant: Equatable, Sendable {
        /// The harness, or something glov cannot tell apart from it: attach as before.
        case harness
        /// A shell at its prompt: nothing to lose, so glov replaces it with the harness.
        case shell
        /// Another program someone started in the kept shell (its name): shown as it is, never ended.
        case other(String)
    }

    /// From `hc get <terminal> --json`: its foreground program, else the program it was started with. Anything glov
    /// cannot read counts as the harness, so a session is never ended on a guess.
    public static func occupant(_ json: Data, command: String) -> Occupant {
        struct Program: Decodable { var argv: [String] }
        struct Terminal: Decodable { var argv: [String]; var foreground: Program? }
        guard let t = try? JSONDecoder().decode(Terminal.self, from: json) else { return .harness }
        let argv = t.foreground?.argv ?? t.argv
        guard let first = argv.first else { return .harness }
        func base(_ s: String) -> String { String((s as NSString).lastPathComponent.drop { $0 == "-" }) }
        let shells: Set = ["sh", "bash", "zsh", "dash", "ksh", "fish"]
        // Options only: an interactive or login shell. herdr-app's wrapper (`sh -c '...' sh claude`) still starts one.
        if shells.contains(base(first)) && argv.dropFirst().allSatisfy({ $0.hasPrefix("-") && $0 != "-c" }) { return .shell }
        if t.foreground != nil, base(first) != command, !shells.contains(base(first)) {
            // The guest names it, and glov prints it on the Mac's terminal: no escape sequences.
            let name = String(base(first).filter { $0.isASCII && ($0.isLetter || $0.isNumber || "._+-".contains($0)) }.prefix(32))
            return .other(name.isEmpty ? "a program" : name)
        }
        return .harness
    }

    /// From `hc get <terminal> --json`: no program and not finished, as hc reports a kept terminal from the VM's restart
    /// until it has started its new shell (argv still the harness, pid null). Its foreground is not known yet.
    public static func restoring(_ json: Data) -> Bool {
        guard let object = try? JSONSerialization.jsonObject(with: json) as? [String: Any] else { return false }
        func absent(_ key: String) -> Bool { object[key] == nil || object[key] is NSNull }
        return absent("pid") && absent("finished_ms")
    }

    /// The line said when a session's terminal is left at a shell and glov starts the harness in its place.
    public static func restarting(_ session: String, vm: String, command: String) -> String {
        "\(vm)/\(session) was left at a shell (the VM restarted, or \(command) exited); starting \(command) again"
    }

    /// The line said when someone runs `program` in the session's kept shell: glov shows it as it is, since ending it
    /// could lose their work, and the run after they quit it starts the harness.
    public static func leftAlone(_ session: String, vm: String, program: String, command: String) -> String {
        "\(vm)/\(session) runs \(program), not \(command); showing it as it is (quit \(program) and run again to start \(command))"
    }

    /// Where herdr-app shows machine `vm`: its space for `folder`, the names of that space's tabs, and the terminal and
    /// id of the tab named `tab` there.
    public struct Place: Equatable, Sendable {
        public var space: String?
        public var tabs: Set<String> = []
        public var terminal: String?
        public var tab: String?
    }

    /// From `herdr-app ls`.
    public static func place(_ json: Data, vm: String, folder: String, tab: String) -> Place {
        struct Terminal: Decodable { var terminal: String }
        struct Tab: Decodable { var id: String?; var name: String?; var terminals: [Terminal] }
        struct Space: Decodable { var id: String; var folder: String?; var machine: String?; var tabs: [Tab] }
        struct List: Decodable { var spaces: [Space] }
        let spaces = (try? JSONDecoder().decode(List.self, from: json))?.spaces ?? []
        guard let space = spaces.first(where: { $0.machine == vm && $0.folder == folder }) else { return Place() }
        let named = space.tabs.first { $0.name == tab }
        return Place(space: space.id, tabs: Set(space.tabs.compactMap(\.name)), terminal: named?.terminals.first?.terminal, tab: named?.id)
    }
}
