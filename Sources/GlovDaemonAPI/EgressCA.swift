import Crypto
import Foundation
import X509

/// The per-home CA the egress proxy signs host certificates with, for the hosts a granted secret is bound to
/// (docs/networking.md "Secrets"). `<home>/egress-ca/` holds `ca.pem`, which isolated VMs trust, and `ca.key` (0600, in
/// a 0700 directory), which never leaves the Mac.
public struct EgressCA: Sendable {
    public static let commonName = "glov egress CA"
    /// How long a host certificate is valid; the proxy makes a new one well before that.
    public static let leafLifetime: TimeInterval = 7 * 86400

    public let certificatePEM: String
    private let certificate: Certificate
    private let key: P256.Signing.PrivateKey

    public static func directory(_ home: GlovHome) -> String { home.path + "/egress-ca" }
    public static func certificatePath(_ home: GlovHome) -> String { directory(home) + "/ca.pem" }

    /// The CA in `home`, made on first use. Two processes making it at once both end up with the one that won the rename.
    public static func loadOrCreate(home: GlovHome, now: Date = Date()) throws -> EgressCA {
        let dir = directory(home)
        if let ca = try load(dir) { return ca }
        try FileManager.default.createDirectory(atPath: home.path, withIntermediateDirectories: true)
        let temp = home.path + "/.egress-ca-\(UUID().uuidString)"
        try FileManager.default.createDirectory(atPath: temp, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(atPath: temp) }
        let key = P256.Signing.PrivateKey()
        let name = try DistinguishedName { CommonName(commonName) }
        let certificate = try Certificate(
            version: .v3, serialNumber: .init(), publicKey: .init(key.publicKey),
            notValidBefore: now - 86400, notValidAfter: now + 20 * 365 * 86400, issuer: name, subject: name,
            signatureAlgorithm: .ecdsaWithSHA256,
            extensions: try Certificate.Extensions {
                Critical(BasicConstraints.isCertificateAuthority(maxPathLength: 0))
                Critical(KeyUsage(keyCertSign: true, cRLSign: true))
                SubjectKeyIdentifier(hash: .init(key.publicKey))
            },
            issuerPrivateKey: .init(key))
        guard FileManager.default.createFile(atPath: temp + "/ca.key", contents: Data(key.pemRepresentation.utf8), attributes: [.posixPermissions: 0o600]),
            FileManager.default.createFile(atPath: temp + "/ca.pem", contents: Data(try certificate.serializeAsPEM().pemString.utf8), attributes: [.posixPermissions: 0o644])
        else { throw CAError("cannot write the egress CA in \(temp)") }
        if rename(temp, dir) != 0 && errno != EEXIST && errno != ENOTEMPTY { throw CAError("cannot create \(dir): \(String(cString: strerror(errno)))") }
        guard let ca = try load(dir) else { throw CAError("\(dir) has no CA") }
        return ca
    }

    private static func load(_ dir: String) throws -> EgressCA? {
        guard FileManager.default.fileExists(atPath: dir + "/ca.pem") else { return nil }
        do {
            let pem = try String(contentsOfFile: dir + "/ca.pem", encoding: .utf8)
            let key = try P256.Signing.PrivateKey(pemRepresentation: String(contentsOfFile: dir + "/ca.key", encoding: .utf8))
            return EgressCA(certificatePEM: pem, certificate: try Certificate(pemEncoded: pem), key: key)
        } catch {
            // Never the error's text: it could quote the key file.
            throw CAError("cannot read the egress CA in \(dir); remove the directory to make a new one, then recreate isolated VMs")
        }
    }

    /// A certificate for the DNS name `host` signed by this CA, with its private key, both PEM.
    public func leaf(for host: String, now: Date = Date()) throws -> (certificatePEM: String, keyPEM: String) {
        let key = P256.Signing.PrivateKey()
        let certificate = try Certificate(
            version: .v3, serialNumber: .init(), publicKey: .init(key.publicKey),
            notValidBefore: now - 3600, notValidAfter: now + Self.leafLifetime, issuer: self.certificate.subject,
            subject: try DistinguishedName { CommonName(String(host.prefix(64))) },
            signatureAlgorithm: .ecdsaWithSHA256,
            extensions: try Certificate.Extensions {
                Critical(BasicConstraints.notCertificateAuthority)
                Critical(KeyUsage(digitalSignature: true))
                try ExtendedKeyUsage([.serverAuth])
                SubjectAlternativeNames([.dnsName(host)])
                AuthorityKeyIdentifier(keyIdentifier: try self.certificate.extensions.subjectKeyIdentifier?.keyIdentifier)
            },
            issuerPrivateKey: .init(self.key))
        return (try certificate.serializeAsPEM().pemString, key.pemRepresentation)
    }

    public struct CAError: Error, CustomStringConvertible {
        public var description: String
        init(_ description: String) { self.description = description }
    }
}
