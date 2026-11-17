import Foundation
import GlovCore

/// A project folder shared at the same path in a Linux guest, as `glov agent` mounts it.
public enum ProjectShare {
    /// Canonical explicit roots only; no home-directory default and no VM secrets.
    public static func samePath(_ input: String, name: String, home: GlovHome, userHome: String = FileManager.default.homeDirectoryForCurrentUser.path) throws -> VMConfig.Share {
        guard let real = HostPath.realpath(input), real == URL(fileURLWithPath: input).standardized.path else {
            throw ProvisionError("A project share must be an existing canonical directory without symlink components")
        }
        var isDirectory = ObjCBool(false)
        guard FileManager.default.fileExists(atPath: real, isDirectory: &isDirectory), isDirectory.boolValue else { throw ProvisionError("A project share is not a directory") }
        let parts = real.split(separator: "/")
        guard parts.count >= 3, parts.first == "Users" || parts.first == "Volumes", !parts.contains("secrets"),
            !real.unicodeScalars.contains(where: { $0.value < 32 || $0.value == 127 }) else {
            throw ProvisionError("Project shares must be project directories below /Users/<user> or /Volumes/<volume>, outside secrets directories")
        }
        let ownHome = HostPath.realpath(userHome) ?? userHome
        let vmHome = HostPath.realpath(home.path) ?? URL(fileURLWithPath: home.path).standardized.path
        for protected in [ownHome, vmHome] {
            if real == protected || protected.hasPrefix(real + "/") || (protected == vmHome && real.hasPrefix(protected + "/")) {
                throw ProvisionError("A project share overlaps a protected home; choose a project directory")
            }
        }
        return .init(name: name, hostPath: real, guestPath: real)
    }
}
