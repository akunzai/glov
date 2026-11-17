import GlovDaemonAPI

/// The one-line state `glov vm wait` reports while it waits.
enum WaitProgress {
    static func line(_ info: Results.VMInfo) -> String {
        if let install = info.install {
            return "\(info.name): installing Windows: \(install.phase)" + (install.overdue ? "; \(overdueHint(info.name))" : "")
        }
        let agent = info.agent.map { ", Guest Service \($0.state)" } ?? ""
        return "\(info.name): \(info.state)\(agent)"
    }

    static func overdueHint(_ vm: String) -> String { "longer than usual; see the screen with `glov vm screenshot \(vm)`" }
}
