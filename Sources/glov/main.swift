import Dispatch
import Foundation
import GlovCore
import GlovDaemonAPI

let invocation: ClientInvocation
do { invocation = try ClientInvocation(Array(CommandLine.arguments.dropFirst())) }
catch { Glov.exit(withError: error) }
let remoteDaemon = invocation.options.remote.map { RemoteDaemon(host: $0, home: invocation.options.remoteHome) }

// `glov view` is a plain synchronous command so AppKit gets the main thread; everything else is async.
// `glov claude ...` is `glov agent claude ...`, for every harness profile's command or name.
var arguments = invocation.arguments
if let word = arguments.first, ProjectAgent.harness(for: word) != nil,
    !Glov.configuration.subcommands.contains(where: { $0.configuration.commandName == word }) {
    arguments.insert("agent", at: 0)
}
if arguments.first == "view" {
    View.main(Array(arguments.dropFirst()))
} else {
    Task {
        await Glov.main(arguments)  // exits the process itself on failure or --help
        exit(0)
    }
    dispatchMain()
}
