import Foundation
import GlovCore
import Testing

@testable import GlovDaemon
@testable import GlovDaemonAPI

@Suite struct TrustTests {
    func makeHome() throws -> GlovHome {
        let home = GlovHome(path: try shortTempDir())
        for (name, isolated) in [("box", true), ("plain", false)] {
            try home.create(VMConfig(
                name: name, directory: home.directory(of: name), engine: .vz, firmwareCode: "", tpm: false,
                isolated: isolated, egress: isolated ? .init() : nil))
        }
        return home
    }

    func call(_ d: Dispatcher, _ method: String, _ params: some Encodable) async throws -> RPCResponse {
        await d.handle(.init(id: 1, method: method, params: try JSONValue(encoding: params)))
    }

    /// A CA certificate, as `EgressCA` makes one.
    func caPEM() throws -> String {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        return try EgressCA.loadOrCreate(home: home).certificatePEM
    }

    @Test func addListReplaceAndRemoveKeepTrustInVMJson() async throws {
        let home = try makeHome()
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let d = Dispatcher(home: home)
        let pem = try caPEM(), other = try caPEM()
        let added = try await call(d, Method.trustAdd, Params.Trust(vm: "bo", name: "corp", certificate: "leading text\n" + pem))
        let summary = try #require(try added.result?.decode(Results.Trust.self))
        #expect(summary.vm == "box" && summary.certificates.map(\.name) == ["corp"])
        #expect(summary.certificates.first?.subject.contains("glov egress CA") == true)
        #expect(summary.certificates.first?.authority == true && summary.certificates.first?.sha256.count == 64)
        let stored = try #require(try home.loadConfig("box").egress?.trust?.first)
        #expect(stored.certificate.hasPrefix("-----BEGIN CERTIFICATE-----\n") && stored.certificate.hasSuffix("-----END CERTIFICATE-----\n"))

        _ = try await call(d, Method.trustAdd, Params.Trust(vm: "box", name: "corp", certificate: other))
        _ = try await call(d, Method.trustAdd, Params.Trust(vm: "box", name: "lab", certificate: pem))
        let listed = try await call(d, Method.trustList, Params.Trust(vm: "box"))
        #expect(try listed.result?.decode(Results.Trust.self).certificates.map(\.name) == ["corp", "lab"])
        #expect(try home.loadConfig("box").egress?.trust?.first?.certificate == TrustedCertificate.make(name: "corp", pem: other).certificate)

        _ = try await call(d, Method.trustRemove, Params.Trust(vm: "box", name: "corp"))
        _ = try await call(d, Method.trustRemove, Params.Trust(vm: "box", name: "lab"))
        #expect(try home.loadConfig("box").egress?.trust == nil)
        let missing = try await call(d, Method.trustRemove, Params.Trust(vm: "box", name: "lab"))
        #expect(missing.error?.message == "VM box has no trusted certificate named lab: see glov vm trust list box")
        let plain = try await call(d, Method.trustAdd, Params.Trust(vm: "plain", name: "corp", certificate: pem))
        #expect(plain.error != nil)
    }

    @Test func onlyOneReadableCertificateWithASafeNameIsAccepted() throws {
        let pem = try caPEM()
        for bad in ["", "a/b", "..", "a b", String(repeating: "x", count: 65)] {
            #expect(throws: TrustedCertificate.Problem.badName(bad)) { try TrustedCertificate.make(name: bad, pem: pem) }
        }
        #expect(throws: TrustedCertificate.Problem.notOneCertificate(0)) { try TrustedCertificate.make(name: "corp", pem: "hello") }
        #expect(throws: TrustedCertificate.Problem.notOneCertificate(2)) { try TrustedCertificate.make(name: "corp", pem: pem + pem) }
        let garbled = "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
        #expect(throws: (any Error).self) { try TrustedCertificate.make(name: "corp", pem: garbled) }
        #expect(try TrustedCertificate.make(name: "corp-1.ca_x", pem: pem).name == "corp-1.ca_x")
    }

    @Test func anOldEnvironmentGetsNodePointedAtTheSystemBundleOnce() async throws {
        let dir = try shortTempDir()
        defer { try? FileManager.default.removeItem(atPath: dir) }
        func run(_ env: String) async throws -> String {
            let path = dir + "/environment"
            try env.write(toFile: path, atomically: true, encoding: .utf8)
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/bin/sh")
            p.arguments = ["-c", "set -e\n" + VMGuestService.nodeTrustFix(environment: path)]
            try await runToExit(p)
            #expect(p.terminationStatus == 0)
            return try String(contentsOfFile: path, encoding: .utf8)
        }
        let old = "HTTPS_PROXY=http://127.0.0.1:3128\nNODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/glov-egress-ca.crt\nSSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt\n"
        #expect(try await run(old) == "HTTPS_PROXY=http://127.0.0.1:3128\nNODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt\nSSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt\n")
        // Already right, or no egress CA: unchanged.
        let current = "NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt\n", none = "HTTPS_PROXY=http://127.0.0.1:3128\n"
        #expect(try await run(current) == current)
        #expect(try await run(none) == none)
    }

    @Test func theGuestScriptWritesEachCertificateAndRebuildsOnlyOnAChange() {
        let script = VMGuestService.trustScript([.init(name: "corp", certificate: "-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n")])
        #expect(script.hasPrefix("set -e\n" + VMGuestService.nodeTrustFix()))
        #expect(script.contains("d=/usr/local/share/ca-certificates/glov-trust;"))
        #expect(script.contains("cat > \"$t/corp.crt\" <<'GLOV_PEM'\n-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\nGLOV_PEM\n"))
        #expect(script.contains("diff -r -q") && script.contains("update-ca-certificates --fresh"))
    }
}
