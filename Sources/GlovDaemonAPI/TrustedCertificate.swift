import Crypto
import Foundation
import GlovCore
import X509

/// Checks and describes the CA certificates `glov vm trust` gives an isolated VM (`VMConfig.Egress.Trust`).
public enum TrustedCertificate {
    public enum Problem: Error, Equatable, CustomStringConvertible {
        case badName(String)
        case notOneCertificate(Int)
        case unreadable(String)

        public var description: String {
            switch self {
            case .badName(let n): "trust name \(String(reflecting: n)): use A-Z a-z 0-9 . _ - (1 to 64 characters)"
            case .notOneCertificate(let n):
                n == 0 ? "no PEM certificate (-----BEGIN CERTIFICATE-----) in the file" : "\(n) certificates in the file: add one CA per name"
            case .unreadable(let e): "not a readable certificate: \(e)"
            }
        }
    }

    public static func isName(_ name: String) -> Bool {
        let allowed = Set("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-")
        return (1...64).contains(name.count) && name.allSatisfy(allowed.contains) && name != "." && name != ".."
    }

    /// `pem` as one certificate, re-encoded so the guest gets nothing but base64 between the PEM lines.
    public static func make(name: String, pem: String) throws(Problem) -> VMConfig.Egress.Trust {
        guard isName(name) else { throw .badName(name) }
        let count = pem.components(separatedBy: "-----BEGIN CERTIFICATE-----").count - 1
        guard count == 1 else { throw .notOneCertificate(count) }
        do {
            return .init(name: name, certificate: try Certificate(pemEncoded: pem).serializeAsPEM().pemString + "\n")
        } catch { throw .unreadable("\(error)") }
    }

    /// What `glov vm trust list` shows of `trust`.
    public static func summary(_ trust: VMConfig.Egress.Trust) -> Results.TrustedCertificate {
        guard let c = try? Certificate(pemEncoded: trust.certificate), let der = try? c.serializeAsPEM().derBytes else {
            return .init(name: trust.name, subject: "(unreadable)", notAfter: nil, sha256: "", authority: false)
        }
        let authority: Bool
        if case .isCertificateAuthority? = (try? c.extensions.basicConstraints) ?? nil { authority = true } else { authority = false }
        return .init(
            name: trust.name, subject: c.subject.description, notAfter: c.notValidAfter,
            sha256: SHA256.hash(data: der).map { String(format: "%02x", $0) }.joined(), authority: authority)
    }
}
