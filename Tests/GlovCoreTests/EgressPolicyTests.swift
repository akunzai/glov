import Testing

@testable import GlovCore

@Suite struct EgressPolicyTests {
    let policy = EgressPolicy(allow: ["api.example.com", "*.ubuntu.com", "registry.example.org:8443", "203.0.113.7", "[2001:db8::7]:8080"])

    @Test func anExactHostIsAllowedOnHTTPSAndHTTPOnly() {
        #expect(policy.allows(host: "api.example.com", port: 443))
        #expect(policy.allows(host: "API.Example.COM.", port: 80))
        #expect(!policy.allows(host: "api.example.com", port: 22))
        #expect(!policy.allows(host: "example.com", port: 443))
        #expect(!policy.allows(host: "evil-api.example.com", port: 443))
        #expect(!policy.allows(host: "api.example.com.evil.net", port: 443))
    }

    @Test func aWildcardMatchesNamesBelowItsSuffixButNotTheSuffix() {
        #expect(policy.allows(host: "archive.ubuntu.com", port: 443))
        #expect(policy.allows(host: "a.b.ubuntu.com", port: 80))
        #expect(!policy.allows(host: "ubuntu.com", port: 443))
        #expect(!policy.allows(host: "notubuntu.com", port: 443))
    }

    @Test func anEntryWithAPortAllowsThatPortOnly() {
        #expect(policy.allows(host: "registry.example.org", port: 8443))
        #expect(!policy.allows(host: "registry.example.org", port: 443))
        #expect(policy.allows(host: "2001:db8::7", port: 8080))
        #expect(!policy.allows(host: "2001:db8::7", port: 443))
    }

    @Test func anIPLiteralIsAllowedOnlyByAnIdenticalEntry() {
        #expect(policy.allows(host: "203.0.113.7", port: 443))
        #expect(!policy.allows(host: "203.0.113.8", port: 443))
        #expect(!EgressPolicy(allow: ["*.0.113.7"]).allows(host: "203.0.113.7", port: 443))
    }

    @Test func nothingIsAllowedByDefault() {
        #expect(!EgressPolicy(allow: []).allows(host: "example.com", port: 443))
        #expect(!policy.allows(host: "", port: 443))
        #expect(!policy.allows(host: "api.example.com", port: 0))
    }

    @Test func onlyPublicAddressesMayBeReached() {
        for address in ["93.184.215.14", "2606:2800:21f:cb07:6820:80da:af6b:8b2c", "8.8.8.8", "64:ff9b::808:808", "2002:808:808::1", "2001:30::1"] {
            #expect(EgressPolicy.isReachable(address, local: []), "\(address)")
        }
        for address in [
            "127.0.0.1", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.64.1", "169.254.169.254", "100.64.0.1",
            "0.0.0.0", "224.0.0.1", "255.255.255.255", "240.0.0.1", "::1", "::", "fe80::1", "fd00::1", "ff02::1",
            "::ffff:192.168.1.1", "::ffff:127.0.0.1", "not an address",
            "192.0.2.1", "198.51.100.1", "203.0.113.1", "192.88.99.1", "2001:db8::1", "2001:0:4136:e378::1",
            "64:ff9b::a00:1", "64:ff9b:1::808:808", "fec0::1", "100::1", "4000::1", "2001:10::1", "2001:2f::1", "3fff::1",
            "2002:a00:1::1",
        ] {
            #expect(!EgressPolicy.isReachable(address, local: []), "\(address)")
        }
        #expect(!EgressPolicy.isReachable("93.184.215.14", local: ["93.184.215.14"]))
        #expect(!EgressPolicy.isReachable("2606:2800::1", local: ["2606:2800:0::1"]))
    }

    @Test func aPrivateEntryAlsoReachesPrivateNetworksButNotTheMacOrLinkLocal() {
        for address in [
            "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.20", "100.64.0.1", "100.127.255.254", "fd00::1",
            "fd7a:115c:a1e0::1", "::ffff:192.168.1.1", "93.184.215.14", "100.128.0.1", "172.32.0.1",
        ] {
            #expect(EgressPolicy.isReachable(address, local: [], privateNetworks: true), "\(address)")
        }
        for address in [
            "127.0.0.1", "169.254.169.254", "0.0.0.0", "224.0.0.1", "255.255.255.255", "::1", "::", "fe80::1", "ff02::1",
            "::ffff:127.0.0.1", "192.0.2.1", "198.18.0.1", "64:ff9b::a00:1", "2002:a00:1::1",
        ] {
            #expect(!EgressPolicy.isReachable(address, local: [], privateNetworks: true), "\(address)")
        }
        #expect(!EgressPolicy.isReachable("192.168.1.20", local: ["192.168.1.20"], privateNetworks: true))
        #expect(!EgressPolicy.isReachable("fd00::1", local: ["fd00::1"], privateNetworks: true))
        // The Mac's own address in IPv4-mapped form, either way round.
        #expect(!EgressPolicy.isReachable("::ffff:192.168.1.20", local: ["192.168.1.20"], privateNetworks: true))
        #expect(!EgressPolicy.isReachable("192.168.1.20", local: ["::ffff:192.168.1.20"], privateNetworks: true))
        #expect(!EgressPolicy.isReachable("::ffff:93.184.215.14", local: ["93.184.215.14"]))
    }
}

@Suite struct EgressAllowlistEditTests {
    @Test func entriesAreStoredInCanonicalFormOnceEach() throws {
        let edited = try VMConfig.Egress(allow: ["api.example.com"]).allowing(
            ["API.Example.com.", "*.Ubuntu.COM", "registry.example.org:8443", "2001:0db8::0007", "[2001:db8::7]:8080", "api.example.com:443"])
        #expect(edited.allow == ["api.example.com", "*.ubuntu.com", "registry.example.org:8443", "2001:db8::7", "[2001:db8::7]:8080", "api.example.com:443"])
    }

    @Test func anInvalidEntryChangesNothingAndIsNamed() {
        let egress = VMConfig.Egress(allow: ["api.example.com"])
        for bad in ["", "*", "*.", "a*.example.com", "example.com:0", "example.com:99999", "example.com:http", "[::1", "a b.com", "https://example.com"] {
            #expect(throws: EgressError.invalidEntries([bad])) { try egress.allowing(["ok.example.com", bad]) }
        }
    }

    @Test func removingMatchesCanonicalFormsAndNamesWhatIsMissing() throws {
        let egress = VMConfig.Egress(allow: ["api.example.com", "*.ubuntu.com", "2001:db8::7"])
        #expect(try egress.removing(["API.example.com.", "2001:0db8:0::7"]).allow == ["*.ubuntu.com"])
        #expect(throws: EgressError.notListed(["other.example.com"])) { try egress.removing(["*.ubuntu.com", "other.example.com"]) }
    }

    @Test func privateEntriesAreExactAndMarkedOnce() throws {
        let egress = try VMConfig.Egress(allow: ["api.example.com"]).allowing(["GitLab.corp.example", "10.1.2.3:8443"], privateNetworks: true)
        #expect(egress.allow == ["api.example.com", "gitlab.corp.example", "10.1.2.3:8443"])
        #expect(egress.privateAllow == ["gitlab.corp.example", "10.1.2.3:8443"])
        #expect(try egress.allowing(["gitlab.corp.example"], privateNetworks: true) == egress)
        #expect(try egress.allowing(["gitlab.corp.example"]) == egress)
        #expect(egress.reachesPrivateNetworks(host: "gitlab.corp.example", port: 443))
        #expect(egress.reachesPrivateNetworks(host: "10.1.2.3", port: 8443))
        #expect(!egress.reachesPrivateNetworks(host: "10.1.2.3", port: 443))
        #expect(!egress.reachesPrivateNetworks(host: "api.example.com", port: 443))
        #expect(throws: EgressError.privateSuffix(["*.corp.example"])) { try egress.allowing(["ok.example.com", "*.corp.example"], privateNetworks: true) }

        let upgraded = try egress.allowing(["api.example.com"], privateNetworks: true)
        #expect(upgraded.allow == egress.allow)
        #expect(upgraded.privateAllow == ["gitlab.corp.example", "10.1.2.3:8443", "api.example.com"])
        let removed = try egress.removing(["GITLAB.corp.example", "10.1.2.3:8443"])
        #expect(removed.allow == ["api.example.com"])
        #expect(removed.privateAllow == nil)
    }

    @Test func errorsSayWhatToDo() {
        #expect(EgressError.invalidEntries(["a b"]).description.contains("*.suffix"))
        #expect(EgressError.notListed(["x.com"]).description == "not in the allowlist: x.com")
    }
}
