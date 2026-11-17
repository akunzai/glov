import ArgumentParser
import Foundation

/// Prints the `glov` agent skill built into this binary (from `skills/glov/` via symlinks in `Guide/`).
/// It never writes skill configuration; installing the skill stays `skills add akunzai/glov --skill glov`.
struct Guide: ParsableCommand {
    static let configuration = CommandConfiguration(
        abstract: "Print the glov agent skill: cross-command workflows and gotchas that --help does not cover.",
        discussion: """
            With no topic it prints the skill (SKILL.md). Topics: \(GuideTopic.allCases.map(\.rawValue).joined(separator: ", ")). \
            It only prints; install the skill with `skills add akunzai/glov --skill glov`.
            """)
    @Argument(help: "troubleshooting (what to capture and how to recover); omit for the skill itself.")
    var topic: String?

    func run() throws {
        let text: String
        if let topic {
            guard let known = GuideTopic(rawValue: topic) else {
                throw ValidationError("unknown topic \"\(topic)\"; topics: \(GuideTopic.allCases.map(\.rawValue).joined(separator: ", "))")
            }
            text = known.text
        } else {
            text = String(decoding: PackageResources.SKILL_md, as: UTF8.self)
        }
        FileHandle.standardOutput.write(Data(text.utf8))
    }
}

enum GuideTopic: String, CaseIterable {
    case troubleshooting

    var text: String {
        switch self {
        case .troubleshooting: String(decoding: PackageResources.troubleshooting_md, as: UTF8.self)
        }
    }
}
