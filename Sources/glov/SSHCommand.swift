import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct SSH: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "ssh", abstract: "Open a shell in a VM over SSH, or run a command there.",
        discussion: """
            Replaces itself with /usr/bin/ssh as user glov, with the VM's key. Host keys go to <vm>/known_hosts, never \
            ~/.ssh/known_hosts. A QEMU VM needs `vm create --ssh`; a VZ VM is reached over vsock (glov ssh-relay), with or without a network device. \
            Write ssh options or the remote command after --: `glov ssh win11 -- hostname`.
            """)
    @Argument(help: "VM name or unique prefix.") var vm: String
    @Argument(parsing: .postTerminator, help: "ssh options or the remote command.") var extra: [String] = []

    func run() async throws {
        let argv: [String]
        do {
            argv = try await prepare()
        } catch let e as ExitCode { throw e } catch {
            FileHandle.standardError.write(Data("glov: \(error is CocoaError ? error.localizedDescription : "\(error)")\n".utf8))
            throw ExitCode.failure
        }
        try Self.replaceProcess(with: argv)
    }

    /// Replaces this process with /usr/bin/ssh run as `argv`.
    static func replaceProcess(with argv: [String], path: String = "/usr/bin/ssh") throws -> Never {
        let cArgs = argv.map { strdup($0) } + [nil]
        // A blocked signal stays blocked across exec, and this process blocks some (SIGWINCH among them): ssh would never
        // hear of a resize. A signal ignored here would stay ignored too, so ssh starts from the defaults.
        var none = sigset_t()
        sigemptyset(&none)
        sigprocmask(SIG_SETMASK, &none, nil)
        for signal in [SIGWINCH, SIGINT, SIGTERM, SIGHUP, SIGPIPE, SIGQUIT, SIGTSTP] { Darwin.signal(signal, SIG_DFL) }
        execv(path, cArgs)
        FileHandle.standardError.write(Data("glov: cannot run \(path): \(String(cString: strerror(errno)))\n".utf8))
        throw ExitCode.failure
    }

    /// Runs `path` as `argv` on this terminal and waits for it; its exit code (128 + the signal when one ended it).
    /// It starts from the default signals, as `replaceProcess` does; glov ignores the terminal's interrupt and quit
    /// meanwhile, which are the child's to handle.
    static func runChild(_ argv: [String], path: String = "/usr/bin/ssh") throws -> Int32 {
        var attr = posix_spawnattr_t(nil as OpaquePointer?)
        posix_spawnattr_init(&attr)
        defer { posix_spawnattr_destroy(&attr) }
        var none = sigset_t(), all = sigset_t()
        sigemptyset(&none)
        sigfillset(&all)
        posix_spawnattr_setsigmask(&attr, &none)
        posix_spawnattr_setsigdefault(&attr, &all)
        posix_spawnattr_setflags(&attr, Int16(POSIX_SPAWN_SETSIGMASK | POSIX_SPAWN_SETSIGDEF))
        let previous = [SIGINT, SIGQUIT].map { ($0, Darwin.signal($0, SIG_IGN)) }
        defer { for (sig, handler) in previous { Darwin.signal(sig, handler) } }
        let cArgs = argv.map { strdup($0) } + [nil]
        defer { for a in cArgs { free(a) } }
        var pid = pid_t()
        let rc = posix_spawn(&pid, path, nil, &attr, cArgs, environ)
        guard rc == 0 else { throw ValidationError("cannot run \(path): \(String(cString: strerror(rc)))") }
        var status: Int32 = 0
        while true {
            guard waitpid(pid, &status, WUNTRACED) != -1 else {
                if errno == EINTR { continue }
                throw ValidationError("waiting for \(path): \(String(cString: strerror(errno)))")
            }
            // Stopped (ssh's ~^Z): stop glov too, so the shell gets its job back, and carry ssh on once resumed.
            guard status & 0xff == 0x7f else { break }
            kill(getpid(), SIGSTOP)
            kill(pid, SIGCONT)
        }
        let signal = status & 0x7f
        return signal == 0 ? (status >> 8) & 0xff : 128 + signal
    }

    private func prepare() async throws -> [String] { try await Self.argv(vm: vm, extra: extra) }

    /// The full argv for ssh, after the checks that the VM runs and can be reached.
    static func argv(vm: String, extra: [String]) async throws -> [String] {
        let home = GlovHome.resolve()
        let client = daemonClient()
        let info = try await client.call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self)
        try SSHConnection.requireRunning(vm: info.name, state: info.state, installFailure: info.installFailure)
        let config = try home.loadConfig(info.name)
        guard FileManager.default.fileExists(atPath: SSHConnection.keyPath(directory: config.directory)) else {
            throw SSHConnection.Failure.noKey(vm: info.name)
        }
        let glov = Bundle.main.executableURL?.resolvingSymlinksInPath().path ?? CommandLine.arguments[0]
        let route = try SSHConnection.route(config: config, glov: glov)
        return ["ssh"] + SSHConnection.arguments(vm: info.name, directory: config.directory, route: route, extra: extra)
    }
}

/// ssh's ProxyCommand for a VZ VM (`glov ssh` sets it): pipes stdin and stdout to the guest's sshd over vsock,
/// through the local daemon (`SSHRelay`).
struct SSHRelayCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "ssh-relay", abstract: "Pipe stdin and stdout to a VZ VM's sshd over vsock (ssh's ProxyCommand).",
        shouldDisplay: false)
    @Argument(help: "VM name or unique prefix.") var vm: String

    func run() throws {
        let fd: Int32
        do { fd = try UnixSocket.connect(path: GlovHome.resolve().socketPath) } catch {
            throw fail("cannot reach glovd: \(error)")
        }
        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        guard Self.writeAll(fd, Array("\(SSHRelay.header) \(vm)\n".utf8)) else { throw fail("glovd closed the connection") }
        // Byte by byte, so ssh's first bytes from the guest stay for the pipe below.
        let answer = SSHRelay.readLine(fd) ?? ""
        guard answer == "ok" else {
            throw fail(answer.hasPrefix("error: ") ? String(answer.dropFirst(7)) : "glovd answered: \(answer.isEmpty ? "nothing" : answer)")
        }
        let done = DispatchSemaphore(value: 0)
        Thread.detachNewThread {
            Self.copy(STDIN_FILENO, fd)
            shutdown(fd, SHUT_WR)
        }
        Thread.detachNewThread {
            Self.copy(fd, STDOUT_FILENO)
            done.signal()
        }
        // The guest's side ending ends the session, whether or not ssh closed stdin.
        done.wait()
    }

    private func fail(_ message: String) -> ExitCode {
        FileHandle.standardError.write(Data("glov: \(message)\n".utf8))
        return ExitCode.failure
    }

    static func copy(_ source: Int32, _ destination: Int32) {
        var buffer = [UInt8](repeating: 0, count: 65536)
        while true {
            let n = buffer.withUnsafeMutableBytes { UnixSocket.readWhenReadable(source, $0) }
            if n < 0 && errno == EINTR { continue }
            guard n > 0, writeAll(destination, Array(buffer.prefix(n))) else { return }
        }
    }

    static func writeAll(_ fd: Int32, _ bytes: [UInt8]) -> Bool {
        var offset = 0
        while offset < bytes.count {
            let n = bytes[offset...].withUnsafeBytes { write(fd, $0.baseAddress!, $0.count) }
            if n < 0 && errno == EINTR { continue }
            guard n > 0 else { return false }
            offset += n
        }
        return true
    }
}
