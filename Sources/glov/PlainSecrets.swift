import Foundation
import GlovDaemonAPI

/// Sends a change with `allowPlainSecrets` as given; when glovd refuses it because a `tmpfs` secret's real value could
/// then reach any host (`RPCError.plainSecrets`) and stdin is a terminal, prints why and asks (default no), then sends
/// it again with the confirmation. Without a terminal the refusal stands: it names the flag to pass.
func confirmingPlainSecrets<T>(
    allow: Bool, isTerminal: Bool = isatty(STDIN_FILENO) == 1,
    ask: (_ warning: String) -> String? = { warning in
        FileHandle.standardError.write(Data("warning: \(warning)\nallow that? [y/N] ".utf8))
        return readLine()
    },
    _ send: (_ allowPlainSecrets: Bool) async throws -> T
) async throws -> T {
    do {
        return try await send(allow)
    } catch let error as RPCError where error.code == RPCError.plainSecrets && !allow && isTerminal {
        guard ["y", "yes"].contains((ask(error.message) ?? "").trimmingCharacters(in: .whitespaces).lowercased()) else { throw error }
        return try await send(true)
    }
}
