import Foundation

/// The cloud-init NoCloud seed of a Linux guest: a volume labelled `cidata` holding `user-data` and `meta-data`,
/// and optionally the Guest Service binary, which first boot installs.
public struct CloudInitSeed: Sendable, Equatable {
    public static let volumeLabel = "cidata"
    /// The guest account; it signs in with the VM's SSH key only.
    public static let user = "glov"
    /// Name of the Guest Service binary on the seed volume.
    public static let guestServiceFile = "glov-guest"
    /// Where first boot writes the script that installs the Guest Service from the seed.
    public static let installScriptPath = "/usr/local/lib/glov/install-agent.sh"

    public var hostname: String
    public var instanceID: String
    public var sshPublicKey: String
    /// Whether the seed carries `guestServiceFile` for first boot to install.
    public var installGuestService: Bool
    /// An isolated VM (no NIC): writes the egress marker the Guest Service checks and points the system at its proxy.
    public var isolated = false
    /// Isolated VMs: harness profiles whose environment and home-directory files first boot writes.
    public var harnesses: [HarnessProfile] = []
    /// Isolated VMs: the egress CA certificate (PEM), trusted system-wide, so the proxy can substitute secrets for the
    /// hosts they are bound to (docs/networking.md "Secrets").
    public var egressCA: String?

    /// Where the Guest Service of an isolated VM listens for HTTP proxy clients (docs/guest-protocol.md).
    public static let proxyURL = "http://127.0.0.1:3128"
    public static let noProxy = "localhost,127.0.0.1,::1"
    /// Where first boot writes `egressCA` for update-ca-certificates.
    public static let egressCAPath = "/usr/local/share/ca-certificates/glov-egress-ca.crt"
    /// For programs that read a CA file of their own rather than the system's: Node adds the system bundle (which holds
    /// `egressCAPath` and any `glov vm trust` certificate) to its bundled roots; OpenSSL and Python requests read that
    /// bundle too. Mirrored by the Guest Service's `egress.Env`.
    public static let trustEnvironment = ["NODE_EXTRA_CA_CERTS": systemBundlePath, "SSL_CERT_FILE": systemBundlePath, "REQUESTS_CA_BUNDLE": systemBundlePath]
    /// The bundle update-ca-certificates writes.
    public static let systemBundlePath = "/etc/ssl/certs/ca-certificates.crt"
    /// Harness VMs: a getty drop-in that signs `user` in on the console (tty1).
    public static let autologinPath = "/etc/systemd/system/getty@tty1.service.d/glov-autologin.conf"
    /// VMs with a harness whose sandbox needs it (`HarnessProfile.userNamespaces`): lets unprivileged users create user
    /// namespaces, which Ubuntu restricts through AppArmor by default.
    public static let userNamespacesPath = "/etc/sysctl.d/60-glov-userns.conf"
    /// Applies `userNamespacesPath` at once. `-e` skips a key the kernel lacks: Debian has no AppArmor userns restriction,
    /// so the key is missing there and user namespaces are already allowed.
    public static let applyUserNamespaces = ["sysctl", "-e", "-p", userNamespacesPath]

    public init(hostname: String, instanceID: String, sshPublicKey: String, installGuestService: Bool) {
        self.hostname = hostname
        self.instanceID = instanceID
        self.sshPublicKey = sshPublicKey.trimmingCharacters(in: .whitespacesAndNewlines)
        self.installGuestService = installGuestService
    }

    public var metaData: String {
        "instance-id: \(yaml(instanceID))\nlocal-hostname: \(yaml(hostname))\n"
    }

    /// Mounts the seed volume again (cloud-init only mounts it while reading it), copies the Guest Service out and has it install
    /// its systemd service.
    public static let installScript = """
        #!/bin/sh
        # Written by cloud-init on first boot: installs the glov Guest Service from the seed volume.
        set -eu
        dev=$(blkid -L \(volumeLabel) || blkid -L \(volumeLabel.uppercased()))
        mnt=$(mktemp -d)
        mount -o ro "$dev" "$mnt"
        install -m 0755 "$mnt/\(guestServiceFile)" /usr/local/bin/glov-guest
        umount "$mnt"
        rmdir "$mnt"
        /usr/local/bin/glov-guest install

        """

    public var userData: String {
        var lines = [
            "#cloud-config",
            "hostname: \(yaml(hostname))",
            "ssh_pwauth: false",
        ]
        lines += [
            "users:",
            "  - name: \(Self.user)",
            "    groups: [sudo]",
            "    sudo: \"ALL=(ALL) NOPASSWD:ALL\"",
            "    shell: /bin/bash",
            "    lock_passwd: true",
            "    ssh_authorized_keys:",
            "      - \(yaml(sshPublicKey))",
        ]
        var files: [String] = []
        if isolated {
            // Before the install script, so the Guest Service finds the marker when it first starts.
            files += ["  - path: /etc/glov/egress-proxy", "    permissions: \"0644\"", "    content: \"glov-egress-v1\\n\""]
            files += ["  - path: /etc/environment", "    append: true", "    content: |"]
            files += ["HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"].map { "      \($0)=\(Self.proxyURL)" }
            files += ["NO_PROXY", "no_proxy"].map { "      \($0)=\(Self.noProxy)" }
            if egressCA != nil { files += Self.trustEnvironment.sorted { $0.key < $1.key }.map { "      \($0.key)=\($0.value)" } }
            files += harnesses.flatMap(\.guestEnvironment).sorted { $0.key < $1.key }.map { "      \($0.key)=\($0.value)" }
            files += ["  - path: /etc/apt/apt.conf.d/90glov-proxy", "    permissions: \"0644\"", "    content: |"]
            files += ["http", "https"].map { "      Acquire::\($0)::Proxy \"\(Self.proxyURL)\";" }
            if let egressCA {
                files += ["  - path: \(Self.egressCAPath)", "    permissions: \"0644\"", "    content: |"]
                files += egressCA.split(separator: "\n").map { "      " + $0 }
            }
        }
        let home = "/home/\(Self.user)/"
        for file in harnesses.flatMap(\.guestFiles) {
            // Deferred to the final stage, once the user exists.
            files += ["  - path: \(home + file.path)", "    owner: \(Self.user):\(Self.user)", "    permissions: \"0600\"", "    defer: true", "    content: |"]
            files += file.content.split(separator: "\n", omittingEmptySubsequences: false).dropLast().map { $0.isEmpty ? "" : "      " + $0 }
        }
        if !harnesses.isEmpty {
            // `glov exec --as user` runs in the signed-in user's session, which a headless VM otherwise never has.
            files += ["  - path: \(Self.autologinPath)", "    permissions: \"0644\"", "    content: |"]
            files += ["[Service]", "ExecStart=", "ExecStart=-/sbin/agetty --autologin \(Self.user) --noclear %I $TERM"].map { "      " + $0 }
        }
        if harnesses.contains(where: \.userNamespaces) {
            files += ["  - path: \(Self.userNamespacesPath)", "    permissions: \"0644\"", "    content: |", "      " + HarnessAddition.userNamespacesSetting.dropLast()]
        }
        if installGuestService {
            files += ["  - path: \(Self.installScriptPath)", "    permissions: \"0755\"", "    content: |"]
            files += Self.installScript.split(separator: "\n", omittingEmptySubsequences: false).dropLast().map { $0.isEmpty ? "" : "      " + $0 }
        }
        if !files.isEmpty { lines += ["write_files:"] + files }
        lines.append("runcmd:")
        if isolated && egressCA != nil { lines.append("  - [update-ca-certificates]") }
        // write_files creates the files' directories as root; the harnesses write there too.
        var owned: [String] = []
        for file in harnesses.flatMap(\.guestFiles) {
            let top = home + file.path.prefix { $0 != "/" }
            if !owned.contains(top) { owned.append(top) }
        }
        if !harnesses.isEmpty {
            lines += ["  - [systemctl, daemon-reload]", "  - [systemctl, restart, getty@tty1.service]"]
        }
        if harnesses.contains(where: \.userNamespaces) { lines.append("  - [\(Self.applyUserNamespaces.joined(separator: ", "))]") }
        if !owned.isEmpty { lines.append("  - [chown, -R, \"\(Self.user):\(Self.user)\", \(owned.joined(separator: ", "))]") }
        // As the user, after the chown, so the link and any directory it needs are the user's. The link dangles until the
        // Guest Service writes the secret.
        for link in harnesses.flatMap(\.secretLinks) {
            lines.append("  - [runuser, -u, \(Self.user), --, sh, -c, 'mkdir -p \"$(dirname \"$1\")\" && ln -sfn \"$2\" \"$1\"', sh, \(yaml(home + link.path)), \(link.target)]")
        }
        if installGuestService { lines.append("  - [sh, \(Self.installScriptPath)]") }
        // The kernel console may not be the virtio console, so say it there explicitly: console.log on the host shows it.
        lines.append("  - [sh, -c, 'echo \"glov: first boot done, addresses: $(hostname -I)\" > /dev/hvc0 || true']")
        return lines.joined(separator: "\n") + "\n"
    }

    /// Isolated VMs: netplan with an optional interface only, so `network-online.target` does not wait for a link that
    /// never comes (`systemd-networkd-wait-online` gives up after 2 minutes, on every boot).
    public var networkConfig: String? {
        guard isolated else { return nil }
        return "network:\n  version: 2\n  ethernets:\n    lo:\n      addresses: [127.0.0.1/8]\n      optional: true\n"
    }

    /// Writes `user-data`, `meta-data`, `network-config` and (with `guestServiceBinary`) the Guest Service into `directory`, ready to become the seed volume.
    public func write(to directory: String, guestServiceBinary: String?) throws {
        let fm = FileManager.default
        try fm.createDirectory(atPath: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try Data(userData.utf8).write(to: URL(fileURLWithPath: directory + "/user-data"))
        try Data(metaData.utf8).write(to: URL(fileURLWithPath: directory + "/meta-data"))
        if let networkConfig { try Data(networkConfig.utf8).write(to: URL(fileURLWithPath: directory + "/network-config")) }
        if installGuestService, let guestServiceBinary {
            try fm.copyItem(atPath: guestServiceBinary, toPath: directory + "/" + Self.guestServiceFile)
        }
    }

    /// A double-quoted YAML scalar.
    private func yaml(_ s: String) -> String {
        "\"" + s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") + "\""
    }
}
