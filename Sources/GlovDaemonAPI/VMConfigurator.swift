import Foundation
import GlovCore

/// Reconfigures a stopped VM's CPU count, memory size, virtual disk size (grow-only) and balloon device.
public struct VMConfigurator {
    public static let windowsResizePartitionScript =
        "$s=(Get-PartitionSupportedSize -DriveLetter C).SizeMax; if ((Get-Partition -DriveLetter C).Size -lt $s) { Resize-Partition -DriveLetter C -Size $s }"

    public static func pendingPartitionResizePath(_ directory: String) -> String {
        directory + "/resize-partition"
    }

    var run: (String, [String]) throws -> (status: Int32, output: String) = LinuxProvisioner.runTool
    var findTool: (String) throws -> String = { $0 == "qemu-img" ? try VirtualMachine.findQEMUImg() : try VirtualMachine.find($0) }

    public init() {}

    /// Validates and applies `cpus`, `memory`, `diskSize` and `balloon` to `config` and its system disk on disk.
    public func configure(
        _ config: VMConfig, cpus: Int? = nil, memory: String? = nil, diskSize: String? = nil, balloon: Bool? = nil,
        requireChange: Bool = true
    ) throws -> VMConfig {
        if requireChange, cpus == nil, memory == nil, diskSize == nil, balloon == nil {
            throw ProvisionError("pass at least one of --cpus, --memory, --disk-size or --balloon")
        }
        var updated = config
        if let balloon {
            guard config.engine == .qemu else {
                throw ProvisionError("--balloon is for QEMU (Windows) VMs; a VZ VM always has a balloon device")
            }
            updated.balloon = balloon
        }
        if let cpus {
            guard cpus >= 1 else { throw ProvisionError("--cpus must be at least 1") }
            updated.cpus = cpus
        }
        if let memory {
            let trimmed = memory.trimmingCharacters(in: .whitespaces)
            guard VMConfig.bytes(trimmed) != nil else { throw ProvisionError("cannot read --memory \(memory)") }
            updated.memory = trimmed
        }
        if let diskSize {
            guard let targetBytes = VMConfig.bytes(diskSize) else { throw ProvisionError("cannot read --disk-size \(diskSize)") }
            try growDisk(of: updated, to: targetBytes)
        }
        return updated
    }

    private func growDisk(of config: VMConfig, to targetBytes: UInt64) throws {
        guard let disk = config.disk, FileManager.default.fileExists(atPath: disk) else {
            throw ProvisionError("VM \(config.name) has no system disk to resize")
        }
        switch config.engine {
        case .vz:
            let current = (try FileManager.default.attributesOfItem(atPath: disk)[.size] as? UInt64) ?? 0
            if targetBytes < current {
                throw ProvisionError("cannot shrink disk of VM \(config.name) from \(current) to \(targetBytes) bytes; --disk-size only grows")
            }
            if targetBytes > current {
                let handle = try FileHandle(forWritingTo: URL(fileURLWithPath: disk))
                try handle.truncate(atOffset: targetBytes)
                try handle.close()
            }
        case .qemu:
            let qemuImg: String
            do { qemuImg = try findTool("qemu-img") } catch {
                throw ProvisionError("\(error) (brew install qemu): needed to inspect and resize the qcow2 disk")
            }
            let info = try run(qemuImg, ["info", "--output=json", disk])
            guard info.status == 0 else {
                throw ProvisionError("qemu-img info failed: \(info.output.trimmingCharacters(in: .whitespacesAndNewlines))")
            }
            let parsed = try JSONDecoder().decode(JSONValue.self, from: Data(info.output.utf8))
            let current: UInt64
            if case .int(let v)? = parsed["virtual-size"], v >= 0 {
                current = UInt64(v)
            } else if case .double(let v)? = parsed["virtual-size"], v >= 0 {
                current = UInt64(v)
            } else {
                throw ProvisionError("qemu-img info did not report virtual-size for \(disk)")
            }
            if targetBytes < current {
                throw ProvisionError("cannot shrink disk of VM \(config.name) from \(current) to \(targetBytes) bytes; --disk-size only grows")
            }
            if targetBytes > current {
                let snapshots = Snapshots.parse(parsed["snapshots"])
                if !snapshots.isEmpty {
                    throw ProvisionError(
                        "cannot resize qcow2 disk of VM \(config.name) while snapshots exist (\(snapshots.map(\.name).joined(separator: ", "))); delete snapshots first"
                    )
                }
                let resized = try run(qemuImg, ["resize", disk, String(targetBytes)])
                guard resized.status == 0 else {
                    throw ProvisionError("qemu-img resize failed: \(resized.output.trimmingCharacters(in: .whitespacesAndNewlines))")
                }
                FileManager.default.createFile(
                    atPath: Self.pendingPartitionResizePath(config.directory), contents: Data(), attributes: [.posixPermissions: 0o600])
            }
        }
    }
}
