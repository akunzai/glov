/// Linux distribution for automatic VZ cloud-image provisioning.
public enum LinuxDistro: String, Codable, Sendable, CaseIterable {
    case ubuntu
    case debian

    public func image(cacheRoot: String? = nil) throws -> String {
        switch self {
        case .ubuntu:
            return try (cacheRoot.map { UbuntuImage(cache: $0 + "/ubuntu") } ?? UbuntuImage()).image()
        case .debian:
            return try (cacheRoot.map { DebianImage(cache: $0 + "/debian") } ?? DebianImage()).image()
        }
    }
}
