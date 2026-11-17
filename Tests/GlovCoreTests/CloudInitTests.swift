import Foundation
import Testing

@testable import GlovCore

@Suite struct CloudInitSeedTests {
    let key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleOnlyNotARealKey glov@ubuntu\n"

    @Test func userDataCreatesTheGlovUserWithTheKeyAndNoPassword() {
        let seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: false)
        let u = seed.userData
        #expect(u.hasPrefix("#cloud-config\n"))
        #expect(u.contains("  - name: glov\n"))
        #expect(u.contains("    groups: [sudo]\n"))
        #expect(u.contains("    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n"))
        #expect(u.contains("    lock_passwd: true\n"))
        #expect(u.contains("ssh_pwauth: false\n"))
        #expect(u.contains("      - \"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleOnlyNotARealKey glov@ubuntu\"\n"))
        #expect(!u.contains("glov-guest"))
        #expect(u.contains("/dev/hvc0"))
        #expect(seed.metaData == "instance-id: \"i-1\"\nlocal-hostname: \"ubuntu\"\n")
    }

    @Test func withTheGuestServiceFirstBootInstallsItFromTheSeedVolume() {
        let u = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true).userData
        #expect(u.contains("write_files:\n  - path: /usr/local/lib/glov/install-agent.sh\n"))
        #expect(!u.contains("apk") && !u.contains("bootcmd"))
        #expect(u.contains("      dev=$(blkid -L cidata || blkid -L CIDATA)\n"))
        #expect(u.contains("      install -m 0755 \"$mnt/glov-guest\" /usr/local/bin/glov-guest\n"))
        #expect(u.contains("      /usr/local/bin/glov-guest install\n"))
        #expect(u.contains("runcmd:\n  - [sh, /usr/local/lib/glov/install-agent.sh]\n"))
    }

    @Test func anIsolatedGuestGetsTheEgressMarkerAndSystemWideProxySettings() {
        var seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true)
        seed.isolated = true
        let u = seed.userData
        #expect(u.components(separatedBy: "write_files:").count == 2)
        #expect(u.contains("""
              - path: /etc/glov/egress-proxy
                permissions: "0644"
                content: "glov-egress-v1\\n"
            """))
        #expect(u.contains("""
              - path: /etc/environment
                append: true
                content: |
                  HTTP_PROXY=http://127.0.0.1:3128
                  HTTPS_PROXY=http://127.0.0.1:3128
                  http_proxy=http://127.0.0.1:3128
                  https_proxy=http://127.0.0.1:3128
                  NO_PROXY=localhost,127.0.0.1,::1
                  no_proxy=localhost,127.0.0.1,::1
            """))
        #expect(u.contains("""
              - path: /etc/apt/apt.conf.d/90glov-proxy
                permissions: "0644"
                content: |
                  Acquire::http::Proxy "http://127.0.0.1:3128";
                  Acquire::https::Proxy "http://127.0.0.1:3128";
            """))
        #expect(u.contains("  - [sh, /usr/local/lib/glov/install-agent.sh]\n"))
        // cloud-init waits for the network (2 minutes without a NIC) on every boot whose user data has a bootcmd.
        #expect(!u.contains("bootcmd"))
        #expect(!CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true).userData.contains("egress"))
    }

    @Test func anIsolatedGuestTrustsTheEgressCASystemWideAndInNode() {
        var seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true)
        seed.isolated = true
        #expect(!seed.userData.contains("ca-certificates"))
        seed.egressCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
        let u = seed.userData
        #expect(u.contains("""
                  no_proxy=localhost,127.0.0.1,::1
                  NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
                  REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
                  SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

            """))
        #expect(u.contains("""
              - path: /usr/local/share/ca-certificates/glov-egress-ca.crt
                permissions: "0644"
                content: |
                  -----BEGIN CERTIFICATE-----
                  MIIB
                  -----END CERTIFICATE-----

            """))
        #expect(u.contains("runcmd:\n  - [update-ca-certificates]\n"))
    }

    @Test func harnessesSetTheirEnvironmentAndWriteTheirFilesIntoTheUsersHome() throws {
        var seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true)
        seed.isolated = true
        seed.harnesses = [try HarnessProfile.named("claude"), try HarnessProfile.named("codex")]
        let u = seed.userData
        #expect(u.contains("""
                  no_proxy=localhost,127.0.0.1,::1
                  ANTHROPIC_API_KEY=glov-secret:anthropic
                  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
                  CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1
                  CODEX_API_KEY=glov-secret:openai

            """))
        // Deferred to the final stage: the user, and so the owner, does not exist yet when write_files first runs.
        #expect(u.contains("""
              - path: /home/glov/.claude/settings.json
                owner: glov:glov
                permissions: "0600"
                defer: true
                content: |
                  {
                    "permissions" : {
                      "defaultMode" : "auto"
                    },
                    "skipWebFetchPreflight" : true
                  }
              - path: /home/glov/.codex/config.toml
            """))
        #expect(u.contains("      base_url = \"https://api.openai.com/v1\"\n"))
        // The directories write_files creates for them are root's until runcmd hands them over.
        #expect(u.contains("\n  - [chown, -R, \"glov:glov\", /home/glov/.claude.json, /home/glov/.claude, /home/glov/.codex]\n"))
        #expect(!CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true).userData.contains("chown"))
    }

    @Test func aHarnessVMSignsTheUserInOnTheConsoleSoGuestCommandsCanRunAsThem() throws {
        var seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true)
        seed.isolated = true
        seed.harnesses = [try HarnessProfile.named("codex")]
        let u = seed.userData
        #expect(u.contains("""
              - path: /etc/systemd/system/getty@tty1.service.d/glov-autologin.conf
                permissions: "0644"
                content: |
                  [Service]
                  ExecStart=
                  ExecStart=-/sbin/agetty --autologin glov --noclear %I $TERM
            """))
        // The getty already runs when write_files adds the drop-in.
        #expect(u.contains("  - [systemctl, daemon-reload]\n  - [systemctl, restart, getty@tty1.service]\n"))
        // codex's sandbox needs unprivileged user namespaces, now and after a reboot.
        #expect(u.contains("  - path: /etc/sysctl.d/60-glov-userns.conf\n    permissions: \"0644\"\n    content: |\n      kernel.apparmor_restrict_unprivileged_userns = 0\n"))
        // Applied at once; a kernel without the key (Debian, which does not restrict them) is no error.
        #expect(u.contains("  - [sysctl, -e, -p, /etc/sysctl.d/60-glov-userns.conf]\n"))
        // Only a harness whose sandbox needs them gets user namespaces.
        seed.harnesses = [try HarnessProfile.named("claude"), try HarnessProfile.named("antigravity")]
        #expect(seed.userData.contains("autologin") && !seed.userData.contains("userns"))
        seed.harnesses = []
        #expect(!seed.userData.contains("autologin"))
    }

    @Test func aTmpfsSecretIsLinkedFromTheHomeAndAPlaceholderGoesIntoItsFile() throws {
        let profile = try JSONDecoder().decode(HarnessProfile.self, from: Data("""
            {"name": "t", "title": "T", "command": "t", "verified": {"version": "1", "date": "2026-11-03"}, "docs": [],
             "secrets": [{"name": "c", "delivery": "tmpfs", "file": ".t/a b/cred.json", "hosts": ["h.example"]},
                         {"name": "k", "delivery": "placeholder", "file": ".t/s.json", "json": ["key"], "hosts": ["h.example"]},
                         {"name": "p", "delivery": "tmpfs", "env": "T_CRED", "hosts": ["h.example"]}],
             "environment": {}, "files": [], "runtime": [], "install": [], "installScript": "", "userNamespaces": false, "notes": []}
            """.utf8))
        var seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true)
        seed.isolated = true
        seed.harnesses = [profile]
        let u = seed.userData
        #expect(u.contains("      T_CRED=/run/glov/secrets/p\n"))
        #expect(u.contains("  - path: /home/glov/.t/s.json\n    owner: glov:glov\n    permissions: \"0600\"\n    defer: true\n    content: |\n      {\n        \"key\" : \"glov-secret:k\"\n      }\n"))
        // After the chown, as the user: the link dangles until the Guest Service writes the value.
        let link = "  - [runuser, -u, glov, --, sh, -c, 'mkdir -p \"$(dirname \"$1\")\" && ln -sfn \"$2\" \"$1\"', sh, \"/home/glov/.t/a b/cred.json\", /run/glov/secrets/c]\n"
        #expect(u.contains(link))
        #expect(u.range(of: "[chown, -R")!.lowerBound < u.range(of: link)!.lowerBound)
        #expect(!u.contains("/run/glov/secrets/c\n    "), "never a file holding the value")
    }

    @Test func anIsolatedSeedHasANetworkConfigThatWaitsForNoLink() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        var seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: false)
        try seed.write(to: dir + "/plain", guestServiceBinary: nil)
        #expect(!FileManager.default.fileExists(atPath: dir + "/plain/network-config"))
        seed.isolated = true
        try seed.write(to: dir + "/isolated", guestServiceBinary: nil)
        // An optional interface only, so netplan does not have network-online.target wait (2 minutes without a NIC).
        #expect(try String(contentsOfFile: dir + "/isolated/network-config", encoding: .utf8) == """
            network:
              version: 2
              ethernets:
                lo:
                  addresses: [127.0.0.1/8]
                  optional: true

            """)
    }

    @Test func writesTheSeedFilesAndTheGuestService() throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        FileManager.default.createFile(atPath: dir + "/agent-bin", contents: Data("ELF".utf8))
        let seed = CloudInitSeed(hostname: "ubuntu", instanceID: "i-1", sshPublicKey: key, installGuestService: true)
        try seed.write(to: dir + "/seed", guestServiceBinary: dir + "/agent-bin")
        #expect(try String(contentsOfFile: dir + "/seed/user-data", encoding: .utf8) == seed.userData)
        #expect(try String(contentsOfFile: dir + "/seed/meta-data", encoding: .utf8) == seed.metaData)
        #expect(FileManager.default.contents(atPath: dir + "/seed/glov-guest") == Data("ELF".utf8))
    }
}
