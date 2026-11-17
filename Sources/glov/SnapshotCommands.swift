import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct SnapshotCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "snapshot", abstract: "List, create, revert and delete snapshots of a VM's system disk.",
        discussion: "A running VM gets a live snapshot (memory and devices, plus the TPM on qemu); a stopped VM a disk-only one. UEFI variables are reverted on vz VMs only.",
        subcommands: [List.self, Create.self, Revert.self, Delete.self])

    struct List: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "List a VM's snapshots.", aliases: ["ls"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        func run() async throws {
            try await report {
                print(describeSnapshots(try await daemonClient().call(Method.snapshotList, Params.VM(vm: vm), as: Results.SnapshotList.self)))
            }
        }
    }

    struct Create: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Snapshot a VM: live while it runs, disk-only while it is stopped.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Snapshot name: A-Z a-z 0-9 . _ - (at most 64 characters).") var name: String
        func run() async throws {
            try await report {
                _ = try await daemonClient().call(Method.snapshotCreate, Params.Snapshot(vm: vm, name: name), as: Results.SnapshotList.self)
                print("created snapshot \(name)")
            }
        }
    }

    struct Revert: AsyncParsableCommand {
        static let configuration = CommandConfiguration(
            abstract: "Revert a VM to a snapshot.",
            discussion: "A live snapshot resumes the guest where it was (starting a stopped VM); a disk-only snapshot needs the VM stopped and leaves it stopped.")
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Snapshot name.") var name: String
        func run() async throws {
            try await report {
                print(describe(try await daemonClient().call(Method.snapshotRevert, Params.Snapshot(vm: vm, name: name), as: Results.VMInfo.self)))
            }
        }
    }

    struct Delete: AsyncParsableCommand {
        static let configuration = CommandConfiguration(abstract: "Delete a snapshot.", aliases: ["rm"])
        @Argument(help: "VM name or unique prefix.") var vm: String
        @Argument(help: "Snapshot name.") var name: String
        func run() async throws {
            try await report {
                _ = try await daemonClient().call(Method.snapshotDelete, Params.Snapshot(vm: vm, name: name), as: Results.SnapshotList.self)
                print("deleted snapshot \(name)")
            }
        }
    }
}

func describeSnapshots(_ list: Results.SnapshotList) -> String {
    if list.snapshots.isEmpty { return "no snapshots" }
    return list.snapshots.map { "\($0.name)  \($0.created)  " + ($0.live ? "live, \($0.vmStateSize / 1_048_576) MiB state" : "disk only") }.joined(separator: "\n")
}
