import Foundation

/// The conversation records of an isolated VM's harnesses, kept on the Mac for review: a read-write share mounted at
/// `guestPath`, with each harness's record directory (`HarnessProfile.sessions`) a link into it. The host folder is
/// `<home>/.sessions/<vm>`: in the glov home but in no VM's directory (a dot name is never a VM's), so it outlives
/// `glov vm delete`; never under the Mac's own harness directories, so nothing the guest writes is where a harness on
/// the Mac would read it back. It is the one share allowed in the glov home (`GlovHome.checkShare`): a link the guest
/// makes in it resolves outside it, which every host-side copy refuses.
public enum HarnessSessions {
    public static let shareName = "harness-sessions"
    public static let guestPath = "/mnt/glov-sessions"

    public static let folder = ".sessions"

    /// `<home>/.sessions/<vm>`.
    public static func hostPath(home: String, vm: String) -> String {
        var home = home
        while home.count > 1 && home.hasSuffix("/") { home.removeLast() }
        return home + "/" + folder + "/" + vm
    }

    /// The share, for `vm create --harness`.
    public static func share(home: String, vm: String) -> VMConfig.Share {
        VMConfig.Share(name: shareName, hostPath: hostPath(home: home, vm: vm), guestPath: guestPath)
    }

    /// Where `dir` (relative to the guest home) of `harness` goes in the share.
    public static func target(harness: String, dir: String) -> String {
        guestPath + "/" + harness + "/" + (dir as NSString).lastPathComponent
    }

    /// Run as the guest user: moves what `dir` already holds into the share, then makes `dir` a link to it. Fails,
    /// changing nothing, when the share is not mounted.
    public static func linkScript(harness: String, dir: String) -> String {
        let t = target(harness: harness, dir: dir)
        return """
            set -e
            mountpoint -q \(guestPath) || { echo "\(guestPath) is not mounted" >&2; exit 3; }
            t='\(t)'; d="$HOME/\(dir)"
            mkdir -p "$t" "$(dirname "$d")"
            if [ -d "$d" ] && [ ! -L "$d" ]; then cp -a "$d"/. "$t"/; rm -rf "$d"; fi
            ln -sfn "$t" "$d"
            """
    }

    /// Run as the guest user on a VM without the share: a record directory still linked to it becomes a plain one again.
    public static func unlinkScript(dir: String) -> String {
        """
        d="$HOME/\(dir)"
        case "$(readlink "$d" 2>/dev/null)" in \(guestPath)/*) rm -f "$d"; mkdir -p "$d" ;; esac
        """
    }

    /// The bytes and files under `path`, links not followed; nil when it does not exist.
    public static func usage(_ path: String) -> (bytes: Int, files: Int)? {
        let fm = FileManager.default
        guard fm.fileExists(atPath: path), let walker = fm.enumerator(atPath: path) else { return nil }
        var bytes = 0, files = 0
        while let rel = walker.nextObject() as? String {
            guard let a = try? fm.attributesOfItem(atPath: path + "/" + rel), a[.type] as? FileAttributeType == .typeRegular else { continue }
            files += 1
            bytes += (a[.size] as? Int) ?? 0
        }
        return (bytes, files)
    }
}
