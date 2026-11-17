import Foundation
import Testing

@testable import GlovCore

@Suite struct ShareTests {
    private func vm(_ shares: [VMConfig.Share] = []) -> VMConfig {
        var c = VMConfig(name: "win11", directory: "/vm/win11", firmwareCode: "/fw/code.fd")
        c.shares = shares
        return c
    }

    @Test func aVMHasNoSharesUntilOneIsAddedAndOlderVMJsonStillLoads() throws {
        #expect(vm().shares.isEmpty)
        let old = #"{"name":"w","directory":"/vm/w","cpus":2,"memory":"4G","firmwareCode":"/c.fd","firmwareVars":"/vm/w/vars.fd","tpm":false,"extraDrives":[]}"#
        #expect(try JSONDecoder().decode(VMConfig.self, from: Data(old.utf8)).shares.isEmpty)
    }

    @Test func sharesRoundTripAndAnEmptyExcludeIsLeftOut() throws {
        let c = vm([
            .init(name: "src", hostPath: "/Users/me/src", guestPath: "C:\\work\\src", mode: .rw, exclude: ["bin/", "/root-only"]),
            .init(name: "docs", hostPath: "/Users/me/docs", guestPath: "C:\\docs", mode: .ro),
        ])
        let data = try JSONEncoder().encode(c)
        #expect(try JSONDecoder().decode(VMConfig.self, from: data) == c)
        let json = try #require(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        let shares = try #require(json["shares"] as? [[String: Any]])
        #expect(shares[0]["mode"] as? String == "rw" && shares[0]["exclude"] as? [String] == ["bin/", "/root-only"])
        #expect(shares[1]["mode"] as? String == "ro" && shares[1]["exclude"] == nil)
    }

    @Test func sharesJoinTheCopyAllowlistByMode() {
        let c = vm([
            .init(name: "rw", hostPath: "/h/rw", guestPath: "C:\\rw", mode: .rw),
            .init(name: "ro", hostPath: "/h/ro", guestPath: "C:\\ro", mode: .ro),
        ])
        #expect(c.copyAllowlist(hostWrites: false).shares == ["/h/rw", "/h/ro"])
        #expect(c.copyAllowlist(hostWrites: true).shares == ["/h/rw"])
        #expect(c.copyAllowlist(hostWrites: true).configured == c.hostPathAllowlist)
    }

    @Test func shareNamesAreSafeFileNames() {
        for ok in ["src", "my-proj_2", "a.b"] { #expect(VMConfig.Share.isValid(name: ok), "\(ok)") }
        for bad in ["", ".hidden", "a/b", "a b", "a\\b", "ü"] { #expect(!VMConfig.Share.isValid(name: bad), "\(bad)") }
    }

    @Test func guestPathsAreAbsoluteWindowsPathsOutsideTheSystemFolders() throws {
        #expect(try VMConfig.Share.windowsGuestPath("c:/work/proj/") == "C:\\work\\proj")
        #expect(try VMConfig.Share.windowsGuestPath("D:\\src") == "D:\\src")
        #expect(try VMConfig.Share.windowsGuestPath("C:\\Users\\glov\\src") == "C:\\Users\\glov\\src")
        for bad in [
            "C:\\", "C:", "work\\proj", "\\\\server\\share\\x", "C:\\Windows", "c:\\windows\\temp\\x", "C:\\Program Files\\x",
            "C:\\Program Files (x86)", "C:\\ProgramData\\glov", "C:\\Users", "C:\\Users\\glov", "C:\\a\\..\\Windows", "C:\\a:b",
        ] {
            #expect(throws: VMConfig.ShareError.self, "\(bad)") { try VMConfig.Share.windowsGuestPath(bad) }
        }
    }

    @Test func aShareMayNotOverlapAnotherOne() throws {
        let c = vm([.init(name: "a", hostPath: "/h/a", guestPath: "C:\\work\\a", mode: .rw)])
        #expect(throws: Never.self) { try c.adding(.init(name: "b", hostPath: "/h/b", guestPath: "C:\\work\\ab", mode: .rw)) }
        for (name, guest) in [("a", "C:\\other"), ("b", "c:\\WORK\\A"), ("b", "C:\\work\\a\\sub"), ("b", "C:\\work")] {
            #expect(throws: VMConfig.ShareError.self, "\(name) at \(guest)") {
                try c.adding(.init(name: name, hostPath: "/h/b", guestPath: guest, mode: .rw))
            }
        }
    }

    /// A `rw` share around a `ro` one would let `copy_from_vm` write into the `ro` folder, so host folders may not
    /// overlap either.
    @Test func aShareMayNotOverlapAnotherOnesHostFolder() throws {
        let c = vm([.init(name: "src", hostPath: "/h/src", guestPath: "C:\\work\\src", mode: .rw)])
        #expect(throws: Never.self) { try c.adding(.init(name: "b", hostPath: "/h/src2", guestPath: "C:\\work\\b", mode: .ro)) }
        for host in ["/h/src", "/h/src/vendor", "/h", "/H/SRC/vendor", "/"] {
            #expect(throws: VMConfig.ShareError.self, "\(host)") {
                try c.adding(.init(name: "b", hostPath: host, guestPath: "C:\\work\\b", mode: .ro))
            }
        }
    }
}

@Suite struct LinuxShareTests {
    @Test func linuxGuestPathsAreNormalisedAndLimitedToTheMountSubtrees() throws {
        #expect(try VMConfig.Share.linuxGuestPath("/mnt/proj") == "/mnt/proj")
        #expect(try VMConfig.Share.linuxGuestPath("/srv//a/b/") == "/srv/a/b")
        #expect(try VMConfig.Share.linuxGuestPath("/media/x") == "/media/x")
        #expect(try VMConfig.Share.linuxGuestPath("/opt/x") == "/opt/x")
        for bad in ["", "mnt/x", "C:\\work", "/", "//", "/mnt/../etc", "/mnt/./x", "/mnt/a\nb", "/etc", "/etc/x", "/usr/local", "/bin", "/boot/efi",
            "/proc", "/sys/fs", "/dev/shm", "/var/lib", "/root", "/root/x", "/run/x", "/lib64", "/home", "/home/glov/x", "/mnt", "/srv", "/tmp",
            "/tmp/x", "/Mnt/x", "/mntx/y"]
        {
            #expect(throws: VMConfig.ShareError.self, "\(bad)") { try VMConfig.Share.linuxGuestPath(bad) }
        }
    }

    @Test func linuxGuestPathsOverlapCaseSensitively() throws {
        let vm = VMConfig(name: "l", directory: "/d", engine: .vz, firmwareCode: "")
        let one = try vm.adding(.init(name: "a", hostPath: "/h/a", guestPath: "/mnt/a"))
        #expect(throws: VMConfig.ShareError.self) { try one.adding(.init(name: "b", hostPath: "/h/b", guestPath: "/mnt/a/sub")) }
        #expect(throws: VMConfig.ShareError.self) { try one.adding(.init(name: "b", hostPath: "/h/b", guestPath: "/mnt/a")) }
        #expect(throws: VMConfig.ShareError.self) { try one.adding(.init(name: "b", hostPath: "/h/b", guestPath: "/mnt")) }
        #expect(try one.adding(.init(name: "b", hostPath: "/h/b", guestPath: "/mnt/ab")).shares.count == 2)
        #expect(try one.adding(.init(name: "b", hostPath: "/h/b", guestPath: "/mnt/A")).shares.count == 2)
    }

    @Test func theMountCommandsAreArgumentVectorsWithoutAShell() {
        let rw = VMConfig.Share(name: "proj", hostPath: "/h/p", guestPath: "/mnt/proj", mode: .rw)
        let ro = VMConfig.Share(name: "-ref.1", hostPath: "/h/r", guestPath: "/srv/a b", mode: .ro)
        #expect(ShareMount.isMountedArguments(rw) == ["mountpoint", "-q", "--", "/mnt/proj"])
        #expect(ShareMount.createComponentArguments("/srv/a b") == ["mkdir", "-m", "0755", "--", "/srv/a b"])
        #expect(ShareMount.statArguments("/srv") == ["stat", "-c", "%u %a %F", "--", "/srv"])
        #expect(ShareMount.mountArguments(rw) == ["mount", "-t", "virtiofs", "--", "proj", "/mnt/proj"])
        #expect(ShareMount.mountArguments(ro) == ["mount", "-t", "virtiofs", "-o", "ro", "--", "-ref.1", "/srv/a b"])
    }

    @Test func everyComponentFromTheRootIsChecked() {
        #expect(ShareMount.components(of: "/mnt/proj") == ["/", "/mnt", "/mnt/proj"])
        #expect(ShareMount.components(of: "/srv/a/b") == ["/", "/srv", "/srv/a", "/srv/a/b"])
    }

    @Test func onlyARootOwnedRealDirectoryNobodyElseCanWriteIsSafe() {
        func problem(_ out: String) -> String? { ShareMount.directoryProblem("/mnt/x", statOutput: out) }
        #expect(problem("0 755 directory\n") == nil)
        #expect(problem("0 700 directory") == nil)
        #expect(problem("0 1755 directory") == nil, "the sticky bit does not make it writable")
        #expect(problem("0 777 symbolic link")?.contains("not a real directory") == true)
        #expect(problem("0 755 regular file")?.contains("not a real directory") == true)
        #expect(problem("1000 755 directory")?.contains("owned by uid 1000") == true)
        #expect(problem("0 775 directory")?.contains("writable by group or others") == true)
        #expect(problem("0 757 directory")?.contains("writable by group or others") == true)
        #expect(problem("0 1777 directory")?.contains("writable") == true)
        #expect(problem("")?.contains("cannot read") == true)
        #expect(problem("stat: cannot statx")?.contains("cannot read") == true)
    }
}
