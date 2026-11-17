import Foundation
import GlovCore

public enum RemoteDaemonError: Error, CustomStringConvertible {
    case connection(String)
    case incompatible
    case outcomeUnknown(method: String, reason: String)
    case staleInput

    public var description: String {
        switch self {
        case .connection(let reason): "Remote connection failed: \(reason). Check SSH access and that the selected daemon is running; no operation was sent."
        case .incompatible: "Incompatible remote glovd protocol; install a compatible build on the remote Mac manually. The daemon was not restarted."
        case .outcomeUnknown(let method, let reason): "Remote connection lost during \(method): \(reason). The operation may have completed; inspect remote state before retrying. Running VMs and jobs are not stopped."
        case .staleInput: "Connection changed; discarded input from the previous frame."
        }
    }
}

/// One SSH connection with Unix socket forwarding. Individual RPCs use separate channels, so frames do not serialize input.
public actor RemoteDaemon {
    public nonisolated let host: String
    private let home: String?
    private let sshExecutable: String
    private var tunnel: SSHTunnel?
    private var connecting: (id: UUID, task: Task<SSHTunnel, Error>)?

    public init(host: String, home: String? = nil, sshExecutable: String = "/usr/bin/ssh") {
        self.host = host; self.home = home; self.sshExecutable = sshExecutable
    }

    public func call(_ method: String, _ params: JSONValue?, expectedConnectionID: String? = nil) async throws -> (result: JSONValue, connectionID: String?) {
        if [Method.copyToVM, Method.copyFromVM].contains(method), case .string(let path)? = params?["hostPath"], !path.hasPrefix("/") {
            throw RPCError(code: RPCError.invalidParams, message: "host path must be an absolute path on the remote Mac")
        }
        if [Method.guestServiceCheck, Method.guestServiceUpdate].contains(method), case .string(let path)? = params?["binary"], !path.hasPrefix("/") {
            throw RPCError(code: RPCError.invalidParams, message: "Guest Service binary must be an absolute path on the remote Mac")
        }
        let transport: SSHTunnel
        do {
            transport = try await connection()
        } catch {
            throw RemoteDaemonError.connection("\(error)")
        }
        do {
            return try await transport.client.callOnSocket(method, params, remoteConnectionID: transport.id.uuidString, expectedConnectionID: expectedConnectionID)
        } catch let error as RPCError {
            throw error
        } catch let error as RemoteDaemonError {
            switch error {
            case .connection, .outcomeUnknown: if tunnel === transport { tunnel = nil }
            case .incompatible, .staleInput: break
            }
            throw error
        } catch {
            if tunnel === transport { tunnel = nil }
            throw RemoteDaemonError.connection("\(error)")
        }
    }

    private func connection() async throws -> SSHTunnel {
        if let tunnel {
            if tunnel.process.isRunning { return tunnel }
            self.tunnel = nil
            throw SSHFailure(message: "SSH connection ended; the next request can reconnect")
        }
        if connecting == nil {
            let host = host, home = home, executable = sshExecutable
            connecting = (UUID(), Task { try await SSHTunnel.open(host: host, home: home, executable: executable) })
        }
        let attempt = connecting!
        do {
            let transport = try await attempt.task.value
            if connecting?.id == attempt.id { tunnel = transport; connecting = nil }
            return transport
        } catch {
            if connecting?.id == attempt.id { connecting = nil }
            throw error
        }
    }
}

/// Process is started before publication; only Foundation's process-lifetime operations are used afterwards.
private final class SSHTunnel: @unchecked Sendable {
    let id = UUID()
    let process: Process
    let client: DaemonClient
    private let directory: String
    private let input: Pipe

    init(process: Process, client: DaemonClient, directory: String, input: Pipe) {
        self.process = process; self.client = client; self.directory = directory; self.input = input
    }

    deinit {
        try? input.fileHandleForWriting.close()
        if process.isRunning { process.terminate() }
        try? FileManager.default.removeItem(atPath: directory)
    }

    static func open(host: String, home: String?, executable: String) async throws -> SSHTunnel {
        try await Blocking.run {
            var template = Array("/tmp/glov-remote.XXXXXX".utf8CString)
            guard let path = mkdtemp(&template) else { throw POSIXError(.EIO) }
            let directory = String(cString: path)
            var retained = false
            defer { if !retained { try? FileManager.default.removeItem(atPath: directory) } }
            let logPath = directory + "/ssh.log"
            FileManager.default.createFile(atPath: logPath, contents: nil, attributes: [.posixPermissions: 0o600])
            let log = try FileHandle(forWritingTo: URL(fileURLWithPath: logPath))
            defer { try? log.close() }
            var options = ["-T", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "-o", "ControlPath=none", "-o", "LogLevel=ERROR"]
            if isatty(STDIN_FILENO) == 0 { options += ["-o", "BatchMode=yes"] }
            let root: String
            if let home { root = home } else {
                let query = Process(), output = Pipe()
                query.executableURL = URL(fileURLWithPath: executable)
                query.arguments = options + ["--", host, "printf '%s/Library/Application Support/glov\\n' \"$HOME\""]
                query.standardInput = FileHandle.nullDevice
                query.standardOutput = output
                query.standardError = log
                try query.run()
                let data = output.fileHandleForReading.readDataToEndOfFile()
                query.waitUntilExit()
                guard query.terminationStatus == 0 else { throw SSHFailure(message: diagnostics(logPath)) }
                root = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .newlines)
            }
            guard root.hasPrefix("/"), !root.contains(":"), !root.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else {
                throw SSHFailure(message: "SSH did not return an absolute remote root; use --remote-home with its absolute path")
            }
            let remoteSocket = root + "/glovd.sock", localSocket = directory + "/daemon.sock"
            guard remoteSocket.utf8.count < 104 else { throw SSHFailure(message: "remote daemon socket path exceeds 103 bytes") }
            let process = Process(), input = Pipe()
            process.executableURL = URL(fileURLWithPath: executable)
            // The remote cat exits on parent EOF, so abrupt client exit does not leave an ssh -N process behind.
            process.arguments = options + ["-o", "ExitOnForwardFailure=yes", "-L", localSocket + ":" + remoteSocket, "--", host, "exec /bin/cat >/dev/null"]
            process.standardInput = input
            process.standardOutput = FileHandle.nullDevice
            process.standardError = log
            try process.run()
            let transport = SSHTunnel(process: process, client: DaemonClient(socketPath: localSocket), directory: directory, input: input)
            for _ in 0..<600 {
                if FileManager.default.fileExists(atPath: localSocket) { retained = true; return transport }
                if !process.isRunning { throw SSHFailure(message: diagnostics(logPath)) }
                Thread.sleep(forTimeInterval: 0.05)
            }
            throw SSHFailure(message: "SSH forwarding did not become ready within 30 seconds; check SSH authentication")
        }
    }

    private static func diagnostics(_ path: String) -> String {
        guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)), !data.isEmpty else { return "SSH exited before the forward became ready" }
        return String(decoding: data.suffix(4096), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

private struct SSHFailure: Error, CustomStringConvertible {
    let message: String
    var description: String { message }
}
