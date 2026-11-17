import Foundation
import GlovCore

/// What `glov ssh` hands to `/usr/bin/ssh`, and the checks before it: the VM must run, QEMU needs an `sshPort`;
/// a VZ guest is reached over vsock through `glov ssh-relay`, so it needs no address.
public enum SSHConnection {
    public enum Failure: Error, Equatable, CustomStringConvertible {
        case notRunning(vm: String, state: String, reason: String?)
        case noSSHPort(vm: String)
        case noKey(vm: String)

        public var description: String {
            switch self {
            case .notRunning(let vm, let state, let reason):
                "\(vm) is \(state)" + (reason.map { ": \($0)" } ?? "; start it with `glov vm start \(vm)`")
            case .noSSHPort(let vm): "\(vm) has no SSH port; create it with `glov vm create --ssh` (or add \"sshPort\" to its vm.json)"
            case .noKey(let vm): "\(vm) has no SSH key in its secrets directory; it was created without --ssh"
            }
        }
    }

    public static let user = "glov"

    public static func keyPath(directory: String) -> String { directory + "/secrets/id_ed25519" }
    public static func knownHostsPath(directory: String) -> String { directory + "/known_hosts" }

    /// Fails with the reason unless `state` (a `Results.VMInfo.state`) is one where the guest may answer SSH.
    public static func requireRunning(vm: String, state: String, installFailure: String?) throws {
        guard state == "running" || state == "installing" else {
            throw Failure.notRunning(vm: vm, state: state, reason: installFailure)
        }
    }

    /// How ssh reaches the guest's sshd.
    public enum Route: Equatable, Sendable {
        /// QEMU: the forwarded `sshPort` on loopback.
        case tcp(host: String, port: Int)
        /// VZ: `command` (a `ProxyCommand`) pipes ssh to the guest's sshd over vsock. An isolated VM's session is kept from
        /// opening any way back to the Mac.
        case relay(command: String, isolated: Bool)
    }

    /// The route for `config`'s VM; `glov` is the absolute path of the running glov, which the relay command runs.
    public static func route(config: VMConfig, glov: String) throws -> Route {
        switch config.engine {
        case .qemu:
            guard let port = config.sshPort else { throw Failure.noSSHPort(vm: config.name) }
            return .tcp(host: "127.0.0.1", port: port)
        case .vz:
            // ssh hands ProxyCommand to a shell after expanding `%` tokens.
            func word(_ text: String) -> String { "'" + text.replacingOccurrences(of: "'", with: "'\\''") + "'" }
            return .relay(command: word(glov.replacingOccurrences(of: "%", with: "%%")) + " ssh-relay " + word(config.name), isolated: config.isolated)
        }
    }

    /// Forced on an isolated VM's session: no forwarding of ports, agent, X11 or tunnels, no local commands and no
    /// `~C` command line, any of which would give the guest a way to the Mac.
    static let isolatedOptions = [
        "ClearAllForwardings=yes", "ForwardAgent=no", "ForwardX11=no", "Tunnel=no", "PermitLocalCommand=no", "EnableEscapeCommandline=no",
    ]
    /// `-A`, `-X`, `-Y` and `-S` set their option outright, whatever came first, so the last of each wins instead: these
    /// go after the user's options, and `-S none` keeps a master connection opened without the options above out.
    static let isolatedFlags = ["-a", "-x", "-S", "none"]

    /// The `ssh` argument vector (without argv[0]). `extra` is what the user wrote after `--`: its leading ssh options go
    /// before our `--`, the rest is the remote command. The user is passed as `-l` and the host comes last after `--`, so a
    /// host can never be read as an option. Host keys live in the VM directory only, under the VM's name, so a changing
    /// address never reads as a changed key. ssh keeps an option's first value, so ours win over `extra` and ~/.ssh/config.
    public static func arguments(vm: String, directory: String, route: Route, extra: [String]) -> [String] {
        var a = [
            "-i", keyPath(directory: directory),
            "-o", "IdentitiesOnly=yes",
            "-o", "UserKnownHostsFile=" + quotedOptionValue(knownHostsPath(directory: directory)),
            "-o", "StrictHostKeyChecking=accept-new",
            "-o", "HostKeyAlias=" + vm,
            "-l", user,
        ]
        let host: String
        var last: [String] = []
        switch route {
        case .tcp(let address, let port):
            host = address
            a += ["-p", String(port)]
        case .relay(let command, let isolated):
            host = vm
            a += ["-o", "ProxyCommand=" + command]
            if isolated {
                a += isolatedOptions.flatMap { ["-o", $0] }
                last = isolatedFlags
            }
        }
        let (options, command) = splitOptions(extra)
        return a + options + last + ["--", host] + command
    }

    /// ssh options that take a separate value when written as exactly `-X`.
    private static let valueFlags = Set("bcDEeFIiJLlmOopQRSWw")

    /// The leading ssh options of `words` (with their values) and the remote command after them.
    static func splitOptions(_ words: [String]) -> (options: [String], command: [String]) {
        var i = 0
        while i < words.count, words[i].hasPrefix("-"), words[i] != "-" {
            if words[i] == "--" { return (Array(words[..<i]), Array(words[(i + 1)...])) }
            let w = words[i]
            i += (w.count == 2 && valueFlags.contains(w.last!)) ? 2 : 1
        }
        i = min(i, words.count)
        return (Array(words[..<i]), Array(words[i...]))
    }

    /// ssh splits a file option's value at spaces (the default home is `~/Library/Application Support/glov`) and expands
    /// `%` tokens in it, so the path goes in double quotes with `%` doubled.
    static func quotedOptionValue(_ path: String) -> String {
        "\"" + path.replacingOccurrences(of: "%", with: "%%").replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }
}
