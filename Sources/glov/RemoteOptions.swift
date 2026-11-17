import ArgumentParser
import Foundation

struct RemoteOptions: ParsableArguments {
    @Option(help: "Connect to an existing daemon through this system SSH host or alias.") var remote: String?
    @Option(help: "Absolute glov root on the remote Mac (default: its user's standard root).") var remoteHome: String?

    func validate() throws {
        if let remote, remote.isEmpty || remote.hasPrefix("-") || remote.unicodeScalars.contains(where: { CharacterSet.whitespacesAndNewlines.contains($0) || CharacterSet.controlCharacters.contains($0) }) {
            throw ValidationError("--remote must be an SSH host or alias, without whitespace or a leading '-'")
        }
        if let remoteHome {
            guard remote != nil, remoteHome.hasPrefix("/"), !remoteHome.contains(":"), !remoteHome.contains("\n"), !remoteHome.contains("\r") else {
                throw ValidationError("--remote-home requires --remote and an absolute remote path without ':' or newlines")
            }
        }
    }
}

/// Extract global connection options before dispatching the synchronous AppKit entry point.
struct ClientInvocation {
    let options: RemoteOptions
    let arguments: [String]

    init(_ input: [String]) throws {
        var options: [String] = [], arguments: [String] = []
        var i = 0
        while i < input.count {
            let arg = input[i]
            if arg == "--" { arguments += input[i...]; break }
            if arg == "--remote" || arg == "--remote-home" {
                guard i + 1 < input.count, !input[i + 1].hasPrefix("--") else { throw ValidationError("missing value for \(arg)") }
                options += [arg, input[i + 1]]
                i += 2
            } else if arg.hasPrefix("--remote=") || arg.hasPrefix("--remote-home=") {
                options.append(arg)
                i += 1
            } else {
                arguments.append(arg)
                i += 1
            }
        }
        self.options = try RemoteOptions.parse(options)
        self.arguments = arguments
        let commandArguments = arguments.prefix { $0 != "--" }
        if self.options.remote != nil, !commandArguments.contains("--help"), !commandArguments.contains("-h"), commandArguments.first != "help" {
            let command = arguments.first ?? ""
            let operation = arguments.dropFirst().first ?? ""
            let supported = ["view", "mcp", "exec", "cp", "snapshot", "version"].contains(command)
                || (command == "vm" && ["list", "ls", "status", "start", "stop", "wait", "screenshot", "guest"].contains(operation))
                || (command == "share" && ["sync", "status"].contains(operation))
            guard supported else { throw ValidationError("\(command) \(operation) is not supported in remote mode; run configuration/installation and guest SSH commands on the remote Mac") }
            if command == "view", commandArguments.contains("--home") || commandArguments.contains(where: { $0.hasPrefix("--home=") }) {
                throw ValidationError("use --remote-home for the remote daemon root, not --home")
            }
        }
    }
}
