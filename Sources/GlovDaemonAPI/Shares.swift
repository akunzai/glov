import Foundation
import GlovCore

/// `glov share add|remove` and `vm create --share`: they edit vm.json from glov's process, like `vm create`, through
/// `update`, so an edit glovd makes meanwhile is kept. The daemon reads vm.json on each sync and copy, so a change
/// applies at once, even while the VM runs.
extension GlovHome {
    /// Validates `share` for VM `reference` (its host folder must exist; it is stored resolved) and saves it.
    @discardableResult
    public func addShare(vm reference: String, _ share: VMConfig.Share) throws -> VMConfig {
        try update(try resolve(reference)) { config in
            config = try config.adding(try checkShare(share, engine: config.engine))
        }
    }

    @discardableResult
    public func removeShare(vm reference: String, _ shareName: String) throws -> VMConfig {
        try update(try resolve(reference)) { config in
            _ = try config.share(shareName)
            config.shares.removeAll { $0.name == shareName }
        }
    }

    /// `share` with its host path resolved and its guest path normalised, or why it cannot be a share of an `engine` VM.
    public func checkShare(_ share: VMConfig.Share, engine: VMConfig.Engine) throws -> VMConfig.Share {
        var s = share
        if engine == .vz {
            guard s.exclude.isEmpty else { throw VMConfig.ShareError.invalid("--exclude: " + VMConfig.Share.liveMount) }
            guard s.name.utf8.count <= ShareMount.maxTagBytes else {
                throw VMConfig.ShareError.invalid("share name \(s.name) is longer than \(ShareMount.maxTagBytes) bytes, the limit of a virtiofs tag")
            }
        } else if s.guestPath.isEmpty {
            throw VMConfig.ShareError.invalid("a Windows guest needs a guest path, e.g. C:\\work\\proj")
        }
        var isDir = ObjCBool(false)
        guard s.hostPath.hasPrefix("/"), FileManager.default.fileExists(atPath: s.hostPath, isDirectory: &isDir), isDir.boolValue,
            let real = HostPath.realpath(s.hostPath)
        else { throw VMConfig.ShareError.invalid("host path \(s.hostPath) is not an existing directory") }
        // A share is readable by copy_to_vm: it must not expose the VMs' own directories (disks, secrets).
        let home = HostPath.realpath(path) ?? path
        // Except the conversation records share glov makes itself, which is a VM's own folder of `<home>/.sessions`.
        let records = home + "/" + HarnessSessions.folder + "/"
        let isRecords = s.name == HarnessSessions.shareName && real.hasPrefix(records) && !real.dropFirst(records.count).contains("/")
            && GlovHome.isValid(name: String(real.dropFirst(records.count)))
        if !isRecords, home == real || home.hasPrefix(real + "/") || real.hasPrefix(home + "/") {
            throw VMConfig.ShareError.invalid("host path \(real) is, holds or lies inside the glov home \(home), which holds VM disks and secrets; share a folder of its own")
        }
        s.hostPath = real
        if engine == .vz {
            s.guestPath = try VMConfig.Share.linuxGuestPath(s.guestPath.isEmpty ? VMConfig.Share.defaultLinuxGuestPath(name: s.name) : s.guestPath)
        } else {
            s.guestPath = try VMConfig.Share.windowsGuestPath(s.guestPath)
            _ = try ShareExcludes(s.exclude)
        }
        return s
    }
}

extension VMConfig.Share {
    /// `vm create --share NAME,HOST[,GUEST][,ro][,exclude=PATTERN]...`; a relative host path is taken from `directory`.
    /// A missing GUEST is left empty: `checkShare` gives a VZ share `/mnt/<name>` and refuses it for a Windows guest.
    public init(spec: String, directory: String) throws {
        let parts = spec.split(separator: ",", omittingEmptySubsequences: false).map(String.init)
        guard parts.count >= 2 else {
            throw VMConfig.ShareError.invalid("--share \(spec): write NAME,HOST_PATH[,GUEST_PATH][,ro][,exclude=PATTERN]...")
        }
        func isOption(_ p: String) -> Bool { p == "ro" || p == "rw" || p.hasPrefix("exclude=") }
        // A guest path is absolute, so an option word in the third place is not one.
        let guest = parts.count > 2 && !isOption(parts[2]) ? parts[2] : ""
        var mode = Mode.rw
        var exclude: [String] = []
        for option in parts.dropFirst(guest.isEmpty ? 2 : 3) {
            if option == "ro" || option == "rw" { mode = Mode(rawValue: option)! } else if option.hasPrefix("exclude=") {
                exclude.append(String(option.dropFirst("exclude=".count)))
            } else {
                throw VMConfig.ShareError.invalid("--share \(spec): unknown option \(String(reflecting: option)) (ro, rw or exclude=PATTERN)")
            }
        }
        self.init(name: parts[0], hostPath: Self.absolute(parts[1], directory: directory), guestPath: guest, mode: mode, exclude: exclude)
    }

    public static func absolute(_ path: String, directory: String) -> String {
        let p = (path as NSString).expandingTildeInPath
        return URL(fileURLWithPath: p.hasPrefix("/") ? p : (directory as NSString).appendingPathComponent(p)).standardized.path
    }
}
