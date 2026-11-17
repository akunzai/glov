import Foundation
import GlovCore

/// `glov vm delete`: removes a VM's directory (disk, UEFI variables, TPM state, secrets, snapshots). CLI only.
public enum VMDeletion {
    public enum Outcome: Equatable {
        case deleted
        case declined
    }

    /// Deletes `<home>/<name>` once `isRunning` says the VM is not running and `confirm` (given the question) agrees.
    /// `name` must be the exact name: a prefix is not enough for something that cannot be undone.
    public static func delete(
        _ name: String, in home: GlovHome, isRunning: (String) async throws -> Bool, confirm: (String) -> Bool
    ) async throws -> Outcome {
        guard home.vmNames().contains(name) else { throw HomeError.noSuchVM(name, known: home.vmNames()) }
        if try await isRunning(name) { throw ProvisionError("VM \(name) is running: stop it first (glov vm stop \(name))") }
        let dir = home.directory(of: name)
        guard confirm("Delete VM \(name) and everything in \(dir) (disk, secrets, snapshots)? This cannot be undone. [y/N] ") else { return .declined }
        try FileManager.default.removeItem(atPath: dir)
        return .deleted
    }

    /// Asks the daemon; without one, a QEMU VM may still run on its own, so its `qemu.pid` is checked.
    public static func isRunning(_ name: String, in home: GlovHome) async throws -> Bool {
        do {
            let info = try await DaemonClient(socketPath: home.socketPath).call(Method.vmStatus, Params.VM(vm: name), as: Results.VMInfo.self)
            // A QEMU VM runs while it has a pid, whatever its install state says; a VZ VM has none and reports "running".
            return info.state == "running" || info.pid != nil
        } catch is DaemonClientError {
            return VirtualMachine.liveQEMUPid(pidFile: home.directory(of: name) + "/qemu.pid") != nil
        }
    }

    /// "y" or "yes", any case.
    public static func isYes(_ answer: String?) -> Bool {
        ["y", "yes"].contains(answer?.trimmingCharacters(in: .whitespaces).lowercased() ?? "")
    }
}
