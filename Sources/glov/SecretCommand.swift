import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct SecretCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "secret", abstract: "Name secrets on this Mac and grant them to isolated VMs.",
        discussion: """
            A secret's value lives in the login Keychain (set) or is the stdout of a command glovd runs when it needs it \
            (source), such as gh auth token, so a token a host CLI already manages is never copied. Each secret is bound to \
            hosts it may be sent to, and a VM may use only the secrets granted to it. No command prints a value.
            """,
        subcommands: [Set.self, Source.self, List.self, Remove.self, Grant.self, Revoke.self, Grants.self, Repos.self, Login.self, Logout.self])

    struct Set: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Store a secret in the Keychain, read from stdin (typed without echo on a terminal), never from the command line.")
        @Argument(help: "Secret name (letters, digits, '.', '_', '-').") var name: String
        @Option(name: .customLong("host"), help: "A host the secret may be sent to: name, *.suffix or IP address, optionally with :port. Repeat for more.")
        var hosts: [String] = []
        @Option(help: "github or gitlab: a git sign-in, so each VM granted it reaches only its repositories on these hosts (glov secret repos).")
        var forge: String?
        func run() async throws {
            try await report {
                let value = try Self.readValue(prompt: "Value for \(name): ")
                let r = try await daemonClient().call(Method.secretSet, Params.Secret(name: name, hosts: hosts, value: value, forge: forge), as: Results.Secret.self)
                print("stored secret \(r.name) for \(r.hosts.joined(separator: ", ")) (\(SecretEntry.describe(length: value.count, ending: SecretEntry.ending(of: value))))")
            }
        }

        /// The value, with leading and trailing whitespace trimmed. At a terminal: typed or pasted with one `*` per
        /// character, then its length and last 4 characters shown for a yes before it is used. Otherwise all of stdin.
        static func readValue(prompt: String) throws -> String {
            guard isatty(STDIN_FILENO) == 1 else {
                let data = FileHandle.standardInput.readDataToEndOfFile()
                return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            }
            let value = try readMasked(prompt: prompt).trimmingCharacters(in: .whitespacesAndNewlines)
            guard !value.isEmpty else { throw ValidationError("nothing was entered") }
            var question = "read \(SecretEntry.describe(length: value.count, ending: SecretEntry.ending(of: value)))"
            let odd = SecretEntry.oddCharacters(value)
            if odd > 0 { question += "; it has \(odd) whitespace or control character\(odd == 1 ? "" : "s") inside, which tokens never have" }
            stderr(question + ". Use it? [y/N] ")
            guard let answer = readLine(), ["y", "yes"].contains(answer.trimmingCharacters(in: .whitespaces).lowercased()) else {
                throw ValidationError("not stored")
            }
            return value
        }

        /// Reads a line from the terminal in raw mode (`SecretEntry`), restoring the terminal however it ends.
        private static func readMasked(prompt: String) throws -> String {
            var saved = termios()
            guard tcgetattr(STDIN_FILENO, &saved) == 0 else { throw ValidationError("cannot read the terminal settings") }
            var raw = saved
            // No line editing, no echo, and Ctrl-C as a byte, so the terminal is put back before glov exits.
            raw.c_lflag &= ~tcflag_t(ICANON | ECHO | ISIG | IEXTEN)
            raw.c_iflag &= ~tcflag_t(ICRNL | IXON)
            withUnsafeMutablePointer(to: &raw.c_cc) { cc in
                cc.withMemoryRebound(to: cc_t.self, capacity: Int(NCCS)) { $0[Int(VMIN)] = 1; $0[Int(VTIME)] = 0 }
            }
            guard tcsetattr(STDIN_FILENO, TCSAFLUSH, &raw) == 0 else { throw ValidationError("cannot set the terminal to raw mode") }
            defer {
                tcsetattr(STDIN_FILENO, TCSAFLUSH, &saved)
                stderr("\n")
            }
            stderr(prompt)
            var entry = SecretEntry()
            var byte: UInt8 = 0
            while true {
                let n = read(STDIN_FILENO, &byte, 1)
                if n < 0, errno == EINTR { continue }
                guard n == 1 else { throw ValidationError("the terminal closed") }
                switch entry.feed(byte) {
                case .echo(let text): if !text.isEmpty { stderr(text) }
                case .done(let value): return value
                case .cancelled: throw ValidationError("cancelled")
                case .tooLong: throw ValidationError("over \(SecretEntry.maxBytes >> 10) KiB at the prompt; pipe the value on stdin instead")
                }
            }
        }

        private static func stderr(_ text: String) { FileHandle.standardError.write(Data(text.utf8)) }
    }

    struct Source: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Take a secret's value from a command's stdout, run by glovd when it needs it.",
            discussion: """
                Example: glov secret source github --forge github --host github.com --host api.github.com --host uploads.github.com \
                -- gh auth token --hostname github.com. \
                glovd runs the command with stdin closed and stderr discarded, waits at most 10 s, and keeps the value in memory \
                for a minute. A command name is resolved on this Mac's PATH; with --remote, give an absolute path.
                """)
        @Argument(help: "Secret name (letters, digits, '.', '_', '-').") var name: String
        @Option(name: .customLong("host"), help: "A host the secret may be sent to: name, *.suffix or IP address, optionally with :port. Repeat for more.")
        var hosts: [String] = []
        @Option(help: "github or gitlab: a git sign-in, so each VM granted it reaches only its repositories on these hosts (glov secret repos).")
        var forge: String?
        @Argument(parsing: .postTerminator, help: "The command after --.") var command: [String] = []
        func run() async throws {
            try await report {
                guard !command.isEmpty else { throw ValidationError("give the command after --") }
                let argv = [try Self.resolve(command[0])] + command.dropFirst()
                let r = try await daemonClient().call(
                    Method.secretSource, Params.Secret(name: name, hosts: hosts, command: argv, forge: forge), as: Results.Secret.self)
                print("secret \(r.name) comes from \(argv[0]) for \(r.hosts.joined(separator: ", "))")
            }
        }

        /// glovd runs under launchd with a short PATH, so a bare name is resolved here, on the PATH the user has.
        static func resolve(_ program: String) throws -> String {
            if program.hasPrefix("/") { return program }
            guard remoteDaemon == nil else { throw ValidationError("with --remote, give \(program) as an absolute path on the remote Mac") }
            if program.contains("/") { return URL(fileURLWithPath: program).standardizedFileURL.path }
            for dir in (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":") {
                let path = "\(dir)/\(program)"
                if FileManager.default.isExecutableFile(atPath: path) { return path }
            }
            throw ValidationError("\(program) is not on PATH")
        }
    }

    struct List: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Print each secret's name, source and hosts (never a value).", aliases: ["ls"])
        @Flag(help: "Print JSON.") var json = false
        func run() async throws {
            try await report {
                let r = try await daemonClient().call(Method.secretList, Params.Secret(), as: Results.Secrets.self)
                if json {
                    let encoder = JSONEncoder()
                    encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
                    return print(String(decoding: try encoder.encode(r), as: UTF8.self))
                }
                if r.secrets.isEmpty { return print("no secrets defined") }
                for s in r.secrets {
                    let source = (s.command.map { "command: " + $0.joined(separator: " ") } ?? s.source) + (s.forge.map { " (\($0) sign-in)" } ?? "")
                    let value = s.command != nil ? "" : "\t" + (s.length.map { SecretEntry.describe(length: $0, ending: s.ending) } ?? "length not recorded: set it again")
                    print("\(s.name)\t\(source)\t\(s.hosts.joined(separator: ", "))\(value)")
                }
            }
        }
    }

    struct Remove: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Delete a secret, its Keychain value and every VM's grant of it.", aliases: ["rm"])
        @Argument(help: "Secret name.") var name: String
        func run() async throws {
            try await report {
                _ = try await daemonClient().call(Method.secretRemove, Params.Secret(name: name), as: JSONValue.self)
                print("removed secret \(name)")
            }
        }
    }

    struct Grant: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Let an isolated VM use secrets, and print its grants.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Secret names.") var names: [String]
        @Flag(help: "Grant a secret that a harness of the VM takes as a real value in tmpfs even though the VM is open (vm configure --egress open), so any host could receive it. Without a terminal this is the only way; with one, glov asks.")
        var allowPlainSecrets = false
        func run() async throws {
            try await report {
                let client = daemonClient()
                printGrants(try await confirmingPlainSecrets(allow: allowPlainSecrets) { allow in
                    try await client.call(Method.secretGrant, Params.SecretGrant(vm: vm, names: names, allowPlainSecrets: allow ? true : nil), as: Results.SecretGrants.self)
                })
            }
        }
    }

    struct Revoke: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Stop an isolated VM using secrets (from its next request), and print its grants.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Secret names.") var names: [String]
        func run() async throws {
            try await report { printGrants(try await daemonClient().call(Method.secretRevoke, Params.SecretGrant(vm: vm, names: names), as: Results.SecretGrants.self)) }
        }
    }

    struct Grants: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Print the secrets an isolated VM may use.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report { printGrants(try await daemonClient().call(Method.secretGrants, Params.SecretGrant(vm: vm), as: Results.SecretGrants.self)) }
        }
    }
}

extension SecretCommand {
    struct Repos: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Change or print the repositories an isolated VM may reach with its git secrets.",
            discussion: """
                On the hosts of a git secret (set or source with --forge) granted to the VM, git, REST and GraphQL requests \
                must name one of these repositories; a request naming another is refused with 403 and an egress.log line. \
                Give <host>/<owner>/<repo>, or <host>/<group>/* for every repository under a group. Requests that name no \
                repository may only read. With no repository named, the VM reaches none.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "add, remove or list.") var action: String = "list"
        @Argument(help: "Repositories, as <host>/<owner>/<repo> or <host>/<group>/*.") var repos: [String] = []
        func run() async throws {
            try await report {
                var params = Params.SecretRepos(vm: vm)
                switch action {
                case "add": params.add = repos
                case "remove", "rm": params.remove = repos
                case "list", "ls": guard repos.isEmpty else { throw ValidationError("list takes no repositories") }
                default: throw ValidationError("give add, remove or list, then the repositories")
                }
                if action != "list" && action != "ls" && repos.isEmpty { throw ValidationError("name at least one repository") }
                let r = try await daemonClient().call(Method.secretRepos, params, as: Results.SecretRepos.self)
                if r.repos.isEmpty { return print("\(r.vm): no repositories") }
                for repo in r.repos { print(repo) }
            }
        }
    }

    struct Login: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Sign a harness in an isolated VM in to your subscription instead of an API key.",
            discussion: """
                The tokens stay on this Mac as secrets <vm>.<harness>.<name>, granted to the VM; the guest holds placeholders. \
                claude: run claude setup-token anywhere and paste the token it prints. codex and antigravity: the sign-in runs \
                in the VM in this terminal, and the egress proxy keeps the tokens of each sign-in and refresh. The harness is \
                switched to the subscription now if the VM runs, else when it next starts. glov secret logout undoes it.
                """)
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Harness: claude, codex or antigravity.") var harness: String
        func run() async throws {
            try await report {
                guard let sub = HarnessProfile.all.first(where: { $0.name == harness })?.subscription else {
                    throw ValidationError("harness \(harness) has no subscription sign-in; those that do: \(HarnessProfile.all.filter { $0.subscription != nil }.map(\.name).joined(separator: ", "))")
                }
                var value: String?
                if sub.signIn == nil {
                    if let hint = sub.pasteHint { FileHandle.standardError.write(Data("\(hint)\n".utf8)) }
                    let pasted = try Set.readValue(prompt: "Token for \(harness): ")
                    guard !pasted.isEmpty else { throw ValidationError("no token was pasted") }
                    value = pasted
                }
                let client = daemonClient()
                let r = try await client.call(Method.secretLogin, Params.SecretLogin(vm: vm, harness: harness, value: value), as: Results.SecretLogin.self)
                guard let command = r.command else { return print("\(r.vm): \(harness) signed in to the subscription") }
                guard isatty(STDIN_FILENO) == 1 else {
                    return print("\(r.vm): \(harness) switched to the subscription; sign in with glov ssh \(r.vm) -- -t \(shellQuote(command))")
                }
                let code = try await runInGuest(r.vm, command, terminal: true)
                let grants = try await client.call(Method.secretGrants, Params.SecretGrant(vm: r.vm), as: Results.SecretGrants.self)
                let missing = r.missing.filter { grants.missing.contains($0) || !grants.names.contains($0) }
                guard missing.isEmpty else {
                    throw ValidationError("""
                        the sign-in \(code == 0 ? "ended" : "failed (exit \(code))") without storing \(missing.joined(separator: ", ")); \
                        run glov secret login \(r.vm) \(harness) again, or glov secret logout \(r.vm) \(harness) to go back to the API key
                        """)
                }
                print("\(r.vm): \(harness) signed in to the subscription")
            }
        }
    }

    struct Logout: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Sign a harness in an isolated VM out of your subscription and back to its API key.",
            discussion: "Signs out in the guest first when the VM runs, then removes the tokens from this Mac and the VM's grants.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Harness: claude, codex or antigravity.") var harness: String
        func run() async throws {
            try await report {
                let client = daemonClient()
                let info = try await client.call(Method.vmStatus, Params.VM(vm: vm), as: Results.VMInfo.self)
                if let command = HarnessProfile.all.first(where: { $0.name == harness })?.subscription?.signOut, info.state == "running",
                   try await client.call(Method.secretGrants, Params.SecretGrant(vm: info.name), as: Results.SecretGrants.self).names
                       .contains(where: { $0.hasPrefix("\(info.name).\(harness).") }) {
                    let code = try await runInGuest(info.name, command, terminal: false)
                    if code != 0 { FileHandle.standardError.write(Data("glov: \(command.joined(separator: " ")) in the VM failed (exit \(code)); signing out on the Mac anyway\n".utf8)) }
                }
                let r = try await client.call(Method.secretLogout, Params.SecretLogin(vm: info.name, harness: harness), as: Results.SecretLogin.self)
                print("\(r.vm): \(harness) signed out of the subscription; it uses its API key again")
            }
        }
    }
}

/// Runs `command` in the VM as user glov over SSH in a login shell, and returns its exit code. ssh stays in this
/// process group (Foundation's Process would start a new one, and ssh, then in the background, would be stopped at
/// its first terminal access), with the signal mask and dispositions reset as `glov ssh` does before exec.
private func runInGuest(_ vm: String, _ command: [String], terminal: Bool) async throws -> Int32 {
    let argv = try await SSH.argv(vm: vm, extra: (terminal ? ["-t"] : []) + ["bash -lc \(shellQuote([shellQuote(command)]))"])
    var attributes = posix_spawnattr_t(nil as OpaquePointer?)
    posix_spawnattr_init(&attributes)
    defer { posix_spawnattr_destroy(&attributes) }
    var none = sigset_t(), all = sigset_t()
    sigemptyset(&none)
    sigfillset(&all)
    posix_spawnattr_setsigmask(&attributes, &none)
    posix_spawnattr_setsigdefault(&attributes, &all)
    posix_spawnattr_setflags(&attributes, Int16(POSIX_SPAWN_SETSIGMASK | POSIX_SPAWN_SETSIGDEF))
    let cArgs = argv.map { strdup($0) } + [nil]
    defer { for a in cArgs { free(a) } }
    var pid: pid_t = 0
    let spawned = posix_spawn(&pid, "/usr/bin/ssh", nil, &attributes, cArgs, environ)
    guard spawned == 0 else { throw ValidationError("cannot run /usr/bin/ssh: \(String(cString: strerror(spawned)))") }
    // Ctrl-C reaches ssh (same process group); this process waits for it instead of dying first.
    signal(SIGINT, SIG_IGN)
    defer { signal(SIGINT, SIG_DFL) }
    var status: Int32 = 0
    while waitpid(pid, &status, 0) == -1 && errno == EINTR {}
    return (status & 0x7f) == 0 ? (status >> 8) & 0xff : 128 + (status & 0x7f)
}

private func shellQuote(_ argv: [String]) -> String {
    argv.map { $0.allSatisfy { $0.isLetter || $0.isNumber || "-_./:=@".contains($0) } ? $0 : "'" + $0.replacingOccurrences(of: "'", with: "'\\''") + "'" }
        .joined(separator: " ")
}

private func printGrants(_ r: Results.SecretGrants) {
    if r.names.isEmpty { return print("\(r.vm): no secrets granted") }
    for name in r.names { print(r.missing.contains(name) ? "\(name)\t(no longer defined)" : name) }
}
