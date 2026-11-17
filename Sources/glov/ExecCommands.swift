import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct Exec: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        abstract: "Run a command in the guest, stream its output and exit with its exit code.",
        discussion: """
            No shell is involved: on Windows write `glov exec win11 -- powershell.exe -NoProfile -Command "dir C:\\"`. Stdout and stderr arrive interleaved on stdout.

            --cwd must already exist in the guest; exec does not create it (on Windows a missing one fails as "The directory name is invalid").

            --as user needs someone signed in to the guest console, which after a Windows boot takes up to a minute longer than guest-ready. \
            Until then it fails with Guest Service error 4 and exits \(Exec.noUserExitCode) (EX_TEMPFAIL), so a script can wait and retry on that code \
            instead of matching the message. A guest command that exits \(Exec.noUserExitCode) itself looks the same.

            Cancelling glov exec can leave its guest process running.
            """)
    /// Exit status for `--as user` while nobody is signed in (Guest Service error 4): sysexits' EX_TEMPFAIL, "try again later".
    static let noUserExitCode: Int32 = 75
    @Argument(help: "VM name or unique prefix.") var vm: String
    @Option(help: "Working directory in the guest; it must already exist.") var cwd: String?
    @Option(name: .customLong("as"), help: "system (default) or user (the signed-in console user's session; exits \(Exec.noUserExitCode) until someone has signed in).") var runAs: String?
    @Argument(parsing: .postTerminator, help: "The command and its arguments.") var argv: [String] = []

    func run() async throws {
        try await report {
            guard !argv.isEmpty else { throw ValidationError("give a command after --") }
            let client = daemonClient()
            let job: String
            do {
                job = try await client.call(
                    Method.execStart, Params.Exec(vm: vm, argv: argv, cwd: cwd, as: runAs), as: Results.JobStarted.self).jobId
            } catch let e as RPCError where e.code == RPCError.noUser {
                FileHandle.standardError.write(Data("glov: \(e.message)\n".utf8))
                throw ExitCode(Exec.noUserExitCode)
            }
            if client.remoteHost != nil {
                FileHandle.standardError.write(Data("remote job: \(job); query this job with MCP exec_read after a disconnect (it is not automatically cancelled)\n".utf8))
            }
            let code = try await followJob(client, vm: vm, job: job)
            // The OS keeps only the low 8 bits; do not let a failure turn into 0.
            throw ExitCode(code != 0 && code & 0xFF == 0 ? 1 : Int32(code & 0xFF))
        }
    }
}

/// Streams a guest job's output to stdout until it exits, and returns its exit code.
func followJob(_ client: DaemonClient, vm: String, job: String) async throws -> Int {
    var cursor = 0
    var warnedDropped = false
    while true {
        let r = try await client.call(Method.execRead, Params.ExecRead(vm: vm, jobId: job, cursor: cursor, maxBytes: 256 * 1024), as: Results.ExecRead.self)
        if r.dropped && !warnedDropped {
            warnedDropped = true
            FileHandle.standardError.write(Data("glov: output was too fast to follow; the rest is in \(r.logPath)\n".utf8))
        }
        try? FileHandle.standardOutput.write(contentsOf: Data(r.data.utf8))
        let progressed = r.cursor != cursor
        cursor = r.cursor
        if r.lost {
            FileHandle.standardError.write(Data("glov: lost track of the job when the Guest Service reconnected; output so far is in \(r.logPath)\n".utf8))
            throw ExitCode.failure
        }
        if r.exited && !progressed { return r.code ?? 0 }
        if !progressed { try await Task.sleep(for: .milliseconds(100)) }
    }
}

struct Copy: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "cp", abstract: "Copy a file or directory tree between the host and a guest.",
        discussion: "Write the guest side as vm:path, for example `glov cp ./proj win11:C:\\work\\proj`. A directory's contents land at the destination path. The host side must lie inside the VM's allowlist (its shared folder and the scratch directory by default).")
    @Argument(help: "Source path.") var source: String
    @Argument(help: "Destination path.") var destination: String

    func run() async throws {
        try await report {
            let client = daemonClient()
            let result: Results.Copy
            switch try CopyDirection.parse(source, destination, directory: FileManager.default.currentDirectoryPath, remoteHost: client.remoteHost != nil) {
            case .toVM(let vm, let hostPath, let guestPath):
                result = try await client.call(Method.copyToVM, Params.CopyToVM(vm: vm, hostPath: hostPath, guestPath: guestPath), as: Results.Copy.self)
            case .fromVM(let vm, let guestPath, let hostPath):
                result = try await client.call(Method.copyFromVM, Params.CopyFromVM(vm: vm, guestPath: guestPath, hostPath: hostPath), as: Results.Copy.self)
            }
            print("\(result.files) files, \(result.bytes) bytes")
            for s in result.skipped { FileHandle.standardError.write(Data("skipped \(s)\n".utf8)) }
        }
    }
}
