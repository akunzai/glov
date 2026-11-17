import Foundation

/// The LaunchAgent that keeps glovd running.
public enum LaunchAgent {
    public static let label = "org.glov.daemon"

    public static var plistPath: String { NSHomeDirectory() + "/Library/LaunchAgents/\(label).plist" }

    public static func plist(executable: String, home: GlovHome) throws -> Data {
        let dict: [String: Any] = [
            "Label": label,
            "ProgramArguments": [executable, "--home", home.path],
            "RunAtLoad": true,
            "KeepAlive": true,
            // Never throttled like a background job: the daemon drives vCPU threads.
            "ProcessType": "Interactive",
            // Without this launchd kills qemu when glovd exits, and the daemon could not reattach.
            "AbandonProcessGroup": true,
            "StandardOutPath": home.logPath,
            "StandardErrorPath": home.logPath,
        ]
        return try PropertyListSerialization.data(fromPropertyList: dict, format: .xml, options: 0)
    }

    /// The daemon's own copy of glovd, which the LaunchAgent runs: rebuilding the source tree or `brew upgrade` then
    /// cannot swap the binary of the running daemon or drop its entitlement. `glov host install` refreshes it.
    public static func installedExecutable(home: GlovHome) -> String { home.path + "/bin/glovd" }

    /// The entitlement VZ (Linux) VMs need; the same as `tools/linux-vm/glovd.entitlements`.
    static let entitlements = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
        <plist version="1.0">
        <dict>
        \t<key>com.apple.security.virtualization</key>
        \t<true/>
        </dict>
        </plist>

        """

    /// Copies `source` to `installedExecutable(home:)`, signed ad hoc with `entitlements`, and returns its path. The copy
    /// is signed under a temporary name and renamed into place, so a running daemon keeps its old file.
    public static func installExecutable(from source: String, home: GlovHome) throws -> String {
        try installExecutable(from: source, home: home, run: LinuxProvisioner.runTool)
    }

    static func installExecutable(
        from source: String, home: GlovHome, run: (String, [String]) throws -> (status: Int32, output: String)
    ) throws -> String {
        let fm = FileManager.default
        let target = installedExecutable(home: home)
        let dir = (target as NSString).deletingLastPathComponent
        try fm.createDirectory(atPath: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let staged = target + ".new", plist = dir + "/glovd.entitlements"
        try? fm.removeItem(atPath: staged)
        defer { try? fm.removeItem(atPath: staged); try? fm.removeItem(atPath: plist) }
        try fm.copyItem(atPath: source, toPath: staged)
        try Data(entitlements.utf8).write(to: URL(fileURLWithPath: plist))
        let r = try run("/usr/bin/codesign", ["-s", "-", "--entitlements", plist, "-f", staged])
        guard r.status == 0 else { throw LaunchctlError(output: "codesign \(staged) failed: \(r.output)") }
        guard rename(staged, target) == 0 else { throw LaunchctlError(output: "cannot move \(staged) to \(target): \(String(cString: strerror(errno)))") }
        return target
    }
}

public struct LaunchctlError: Error, CustomStringConvertible {
    public var output: String
    public init(output: String) { self.output = output }
    public var description: String { "launchctl failed: \(output.trimmingCharacters(in: .whitespacesAndNewlines))" }
}
