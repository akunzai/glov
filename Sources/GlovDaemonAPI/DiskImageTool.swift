import Foundation

/// `glov-image`, the Go helper `tools/build.sh` builds beside glov and glovd: it converts a qcow2 cloud image to a raw
/// disk and checks Ubuntu's signature on `SHA256SUMS`, so Linux guests need neither `qemu-img` nor `gpgv`.
public enum DiskImageTool {
    public static let name = "glov-image"

    /// The helper beside the running executable, symlinks resolved (`~/.local/bin/glov` points into the build), else the
    /// copy `glov host install` put in `<home>/bin`, for a glov copied away from its build.
    public static func locate(
        home: GlovHome = .resolve(), executable: String = Bundle.main.executablePath ?? CommandLine.arguments[0]
    ) throws -> String {
        let me = URL(fileURLWithPath: executable).resolvingSymlinksInPath()
        let beside = me.deletingLastPathComponent().appendingPathComponent(name).path, installed = home.path + "/bin/" + name
        for path in [beside, installed] where FileManager.default.isExecutableFile(atPath: path) { return path }
        throw ProvisionError("\(name) not found next to \(me.path) or in \(home.path)/bin; run tools/build.sh and glov host install")
    }

    /// Copies `sourceBin/glov-image` into `destinationBin` under a temporary name and renames it into place, so a
    /// running copy keeps its file.
    public static func install(from sourceBin: String, to destinationBin: String) throws {
        let fm = FileManager.default
        let source = sourceBin + "/" + name, target = destinationBin + "/" + name, staged = target + ".new-" + UUID().uuidString
        guard fm.isExecutableFile(atPath: source) else {
            throw ProvisionError("\(name) not found in \(sourceBin); run tools/build.sh")
        }
        try fm.createDirectory(atPath: destinationBin, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        defer { try? fm.removeItem(atPath: staged) }
        try fm.copyItem(atPath: source, toPath: staged)
        guard rename(staged, target) == 0 else {
            throw ProvisionError("cannot move \(staged) to \(target): \(String(cString: strerror(errno)))")
        }
    }
}
