import Foundation
import GlovCore

/// The herdr-core and herdr-app commands `glov claude --herdr` and `glov vm delete` run.
enum HerdrTool {
    /// Runs `path` with `arguments`, its output; throws with its stderr when it fails.
    static func run(_ path: String, _ arguments: [String]) throws -> Data {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = arguments
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        try p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        let errors = err.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            let name = (path as NSString).lastPathComponent
            let detail = String(decoding: errors.isEmpty ? data : errors, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw Failure(description: "\(name) \(arguments.prefix(2).joined(separator: " ")) failed: \(detail)")
        }
        return data
    }

    /// Removes VM `vm` from herdr-core's machines when it was added for it (`HerdrView.isVM`); the line to print, if any.
    static func forget(_ vm: String) -> String? {
        guard let hc = try? SecretCommand.Source.resolve("hc"), let ls = try? run(hc, ["machine", "ls", "--json"]),
              HerdrView.machines(ls).contains(where: { HerdrView.isVM(name: $0.name, target: $0.target, vm: vm) })
        else { return nil }
        do {
            _ = try run(hc, ["machine", "rm", vm])
            return "removed herdr-core machine \(vm)"
        } catch {
            return "warning: herdr-core machine \(vm) is still there (\(error)); hc machine rm \(vm)"
        }
    }

    struct Failure: Error, CustomStringConvertible { let description: String }
}
