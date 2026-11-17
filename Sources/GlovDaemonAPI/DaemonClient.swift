import Foundation
import GlovCore

public enum DaemonClientError: Error, CustomStringConvertible {
    case notRunning(socketPath: String)
    case badReply(String)
    case closed

    public var description: String {
        switch self {
        case .notRunning(let p):
            "glovd is not running (no daemon on \(p)). Start it with `glovd`, or install it as a LaunchAgent with `glov host install`."
        case .badReply(let m): "unreadable reply from glovd: \(m)"
        case .closed: "glovd closed the connection without replying"
        }
    }
}

/// Talks to glovd. Each call uses its own connection, so a client never holds state in the daemon.
public struct DaemonClient: Sendable {
    public let socketPath: String
    private let remote: RemoteDaemon?
    public var remoteHost: String? { remote?.host }
    public init(socketPath: String) { self.socketPath = socketPath; remote = nil }
    public init(remote: RemoteDaemon) { self.remote = remote; socketPath = "" }

    public func call(_ method: String, _ params: JSONValue? = nil, expectedConnectionID: String? = nil) async throws -> JSONValue {
        try await callWithConnectionID(method, params, expectedConnectionID: expectedConnectionID).result
    }

    public func callWithConnectionID(_ method: String, _ params: JSONValue? = nil, expectedConnectionID: String? = nil) async throws -> (result: JSONValue, connectionID: String?) {
        if let remote { return try await remote.call(method, params, expectedConnectionID: expectedConnectionID) }
        return try await callOnSocket(method, params)
    }

    /// Pin the handshake and operation to one accepted socket, even if the daemon restarts between them.
    func callOnSocket(_ method: String, _ params: JSONValue?, remoteConnectionID: String? = nil,
                      expectedConnectionID: String? = nil) async throws -> (result: JSONValue, connectionID: String?) {
        let fd: Int32
        do {
            fd = try await Blocking.run { [socketPath] in try UnixSocket.connect(path: socketPath) }
        } catch QMPError.connectFailed(_, let e) where e == ENOENT || e == ECONNREFUSED {
            throw DaemonClientError.notRunning(socketPath: socketPath)
        }
        let handle = UnixSocket.Handle(fd: fd)
        defer { handle.shutdown() } // the reader closes the fd after its last read
        var replies = UnixSocket.lines(fd: fd, onEnd: { handle.close() }).makeAsyncIterator()
        func exchange(_ method: String, _ params: JSONValue?, id: JSONValue) async throws -> JSONValue {
            let request = try JSONEncoder().encode(RPCRequest(id: id, method: method, params: params)) + Data("\n".utf8)
            guard await handle.send(request), let line = await replies.next() else { throw DaemonClientError.closed }
            guard let response = try? JSONDecoder().decode(RPCResponse.self, from: line), response.jsonrpc == "2.0", response.id == id else {
                throw DaemonClientError.badReply("expected a JSON-RPC 2.0 response with a matching id")
            }
            if let error = response.error { throw error }
            return response.result ?? .null
        }
        var connectionID: String?
        var operationStarted = false
        do {
            if let remoteConnectionID {
                let info: Results.DaemonInfo
                do { info = try await exchange(Method.daemonInfo, nil, id: 0).decode(Results.DaemonInfo.self) }
                catch let error as RPCError where error.code == RPCError.methodNotFound { throw RemoteDaemonError.incompatible }
                guard info.protocolVersion == DaemonProtocol.version else { throw RemoteDaemonError.incompatible }
                connectionID = remoteConnectionID + ":" + info.instanceID
                if let expectedConnectionID, expectedConnectionID != connectionID { throw RemoteDaemonError.staleInput }
            }
            operationStarted = true
            return (try await exchange(method, params, id: 1), connectionID)
        } catch let error as RemoteDaemonError { throw error }
        catch let error as RPCError { throw error }
        catch {
            guard remoteConnectionID != nil else { throw error }
            if operationStarted { throw RemoteDaemonError.outcomeUnknown(method: method, reason: "\(error)") }
            throw RemoteDaemonError.connection("\(error)")
        }
    }

    public func call<P: Encodable, R: Decodable>(_ method: String, _ params: P, as: R.Type = R.self) async throws -> R {
        try await call(method, try JSONValue(encoding: params)).decode(R.self)
    }
}
