import Foundation

/// What `glov vm harness add` does in a running guest for a harness the VM was not created with: what cloud-init does at
/// first boot for the harnesses given to `vm create` (`CloudInitSeed`).
public enum HarnessAddition {
    /// Run as the guest user with the file's path below the home as `$1` and its content on stdin: writes it (mode 0600)
    /// unless something is there already, which it leaves as it is and exits 10.
    public static let writeFileScript = #"p="$HOME/$1"; if [ -e "$p" ] || [ -L "$p" ]; then exit 10; fi; mkdir -p "$(dirname "$p")" && umask 077 && cat > "$p""#

    /// Run as the guest user with the link's path below the home as `$1` and its target as `$2`, as cloud-init does.
    public static let linkScript = #"mkdir -p "$(dirname "$HOME/$1")" && ln -sfn "$2" "$HOME/$1""#

    public static let userNamespacesSetting = "kernel.apparmor_restrict_unprivileged_userns = 0\n"

    /// Why `harness` cannot be added to `vm`, created with `harnesses` (nil or empty: without `--harness`), or nil. Only
    /// `vm create --harness` signs the guest user in on the console, which `exec --as user` needs, so such a VM is
    /// refused before anything changes in it.
    public static func refusal(vm: String, harnesses: [String]?, harness: String) -> String? {
        guard harnesses?.isEmpty ?? true else { return nil }
        return "VM \(vm) was created without --harness, which glov vm harness add cannot make up for: "
            + "create a harness VM with glov vm create <vm> --engine vz --isolated --harness \(harness)"
    }
}
