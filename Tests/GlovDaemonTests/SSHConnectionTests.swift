import GlovCore
import Testing

@testable import GlovDaemonAPI

@Suite struct SSHConnectionTests {
    private func config(_ engine: VMConfig.Engine, sshPort: Int? = nil) -> VMConfig {
        VMConfig(name: "vm1", directory: "/h/vm1", engine: engine, firmwareCode: "/c.fd", sshPort: sshPort)
    }

    @Test func argumentsUseTheVMsKeyAndItsOwnKnownHostsFile() {
        let a = SSHConnection.arguments(vm: "vm1", directory: "/h/vm1", route: .tcp(host: "127.0.0.1", port: 2222), extra: ["hostname"])
        #expect(a == [
            "-i", "/h/vm1/secrets/id_ed25519", "-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile=\"/h/vm1/known_hosts\"",
            "-o", "StrictHostKeyChecking=accept-new", "-o", "HostKeyAlias=vm1", "-l", "glov", "-p", "2222", "--", "127.0.0.1", "hostname",
        ])
    }

    @Test func aDirectoryWithSpacesStaysOneKnownHostsPath() {
        let a = SSHConnection.arguments(vm: "vm1", directory: "/Users/me/Library/Application Support/glov/vm1", route: .tcp(host: "h", port: 22), extra: [])
        #expect(a.contains("UserKnownHostsFile=\"/Users/me/Library/Application Support/glov/vm1/known_hosts\""))
        #expect(a.contains("/Users/me/Library/Application Support/glov/vm1/secrets/id_ed25519"))
        #expect(SSHConnection.arguments(vm: "v", directory: "/a%b", route: .tcp(host: "h", port: 22), extra: []).contains("UserKnownHostsFile=\"/a%%b/known_hosts\""))
    }

    @Test func qemuConnectsToTheForwardedPortAndNeedsOne() throws {
        #expect(try SSHConnection.route(config: config(.qemu, sshPort: 65230), glov: "/bin/glov") == .tcp(host: "127.0.0.1", port: 65230))
        #expect(throws: SSHConnection.Failure.noSSHPort(vm: "vm1")) { try SSHConnection.route(config: config(.qemu), glov: "/bin/glov") }
        #expect("\(SSHConnection.Failure.noSSHPort(vm: "vm1"))".contains("vm create --ssh"))
    }

    @Test func vzGoesThroughGlovSSHRelayOverVsock() throws {
        #expect(try SSHConnection.route(config: config(.vz), glov: "/opt/my tools/glov") == .relay(command: "'/opt/my tools/glov' ssh-relay 'vm1'", isolated: false))
        var isolated = config(.vz)
        isolated.isolated = true
        #expect(try SSHConnection.route(config: isolated, glov: "/b/50%/it's/glov") == .relay(command: "'/b/50%%/it'\\''s/glov' ssh-relay 'vm1'", isolated: true))

        let a = SSHConnection.arguments(vm: "vm1", directory: "/h/vm1", route: .relay(command: "'/g' ssh-relay 'vm1'", isolated: false), extra: ["-L", "80:localhost:80"])
        #expect(a.contains("ProxyCommand='/g' ssh-relay 'vm1'") && a.contains("HostKeyAlias=vm1") && !a.contains("-p"))
        #expect(!a.contains("ClearAllForwardings=yes"))
        #expect(a.suffix(4) == ["-L", "80:localhost:80", "--", "vm1"])
    }

    @Test func anIsolatedVMsSessionCannotOpenAWayBackToTheMac() {
        // ssh keeps an option's first value, so these come before anything the user or ~/.ssh/config sets.
        let a = SSHConnection.arguments(vm: "vm1", directory: "/h/vm1", route: .relay(command: "c", isolated: true), extra: ["-o", "ForwardAgent=yes", "-R", "9000:localhost:22"])
        let forced = ["ClearAllForwardings=yes", "ForwardAgent=no", "ForwardX11=no", "Tunnel=no", "PermitLocalCommand=no", "EnableEscapeCommandline=no"]
        for option in forced { #expect(a.contains(option), "\(option)") }
        #expect(a.firstIndex(of: "ForwardAgent=no")! < a.firstIndex(of: "ForwardAgent=yes")!)
        // -A, -X, -Y and -S set their option outright, so the flags that undo them come after the user's.
        let flags = SSHConnection.arguments(vm: "vm1", directory: "/h/vm1", route: .relay(command: "c", isolated: true), extra: ["-AY", "-S", "/tmp/m", "ls"])
        #expect(flags.suffix(10) == ["-AY", "-S", "/tmp/m", "-a", "-x", "-S", "none", "--", "vm1", "ls"])
        #expect(!SSHConnection.arguments(vm: "vm1", directory: "/h/vm1", route: .relay(command: "c", isolated: false), extra: []).contains("-a"))
    }

    @Test func aVMThatIsNotRunningFailsWithTheReason() {
        #expect(throws: SSHConnection.Failure.notRunning(vm: "vm1", state: "stopped", reason: nil)) {
            try SSHConnection.requireRunning(vm: "vm1", state: "stopped", installFailure: nil)
        }
        let failed = SSHConnection.Failure.notRunning(vm: "vm1", state: "install-failed", reason: "setup timed out")
        #expect(throws: failed) { try SSHConnection.requireRunning(vm: "vm1", state: "install-failed", installFailure: "setup timed out") }
        #expect("\(failed)" == "vm1 is install-failed: setup timed out")
        #expect("\(SSHConnection.Failure.notRunning(vm: "vm1", state: "stopped", reason: nil))".contains("glov vm start vm1"))
        #expect((try? SSHConnection.requireRunning(vm: "vm1", state: "running", installFailure: nil)) != nil)
    }

    @Test func userOptionsGoBeforeTheHostAndTheCommandAfterIt() {
        func tail(_ extra: [String]) -> [String] {
            let a = SSHConnection.arguments(vm: "v", directory: "/d", route: .tcp(host: "192.168.64.5", port: 22), extra: extra)
            return Array(a[(a.firstIndex(of: "22")! + 1)...])
        }
        #expect(tail(["-t", "-L", "80:x:80", "ls", "-l"]) == ["-t", "-L", "80:x:80", "--", "192.168.64.5", "ls", "-l"])
        #expect(tail(["hostname"]) == ["--", "192.168.64.5", "hostname"])
        #expect(tail([]) == ["--", "192.168.64.5"])
        #expect(tail(["-v"]) == ["-v", "--", "192.168.64.5"])
    }
}
