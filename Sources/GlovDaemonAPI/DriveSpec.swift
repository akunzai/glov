import GlovCore

public struct DriveSpecError: Error, CustomStringConvertible {
    public var description: String
}

extension VMConfig.Drive {
    /// `path[,cdrom][,removable][,bootindex=N]`
    public init(spec: String) throws {
        let parts = spec.split(separator: ",", omittingEmptySubsequences: false).map(String.init)
        guard let path = parts.first, !path.isEmpty else { throw DriveSpecError(description: "drive needs a path: \(spec)") }
        var drive = VMConfig.Drive(path: path)
        for option in parts.dropFirst() {
            switch option {
            case "cdrom": drive.cdrom = true
            case "removable": drive.removable = true
            case _ where option.hasPrefix("bootindex="):
                guard let n = Int(option.dropFirst("bootindex=".count)), n >= 0 else {
                    throw DriveSpecError(description: "bootindex must be a non-negative integer: \(option)")
                }
                drive.bootIndex = n
            default: throw DriveSpecError(description: "unknown drive option \(String(reflecting: option)) (use cdrom, removable, bootindex=N)")
            }
        }
        self = drive
    }
}

extension VMConfig.PortForward {
    /// `HOST:GUEST`
    public init(spec: String) throws {
        let parts = spec.split(separator: ":", omittingEmptySubsequences: false)
        guard parts.count == 2, let host = Int(parts[0]), let guest = Int(parts[1]), (1...65535).contains(host), (1...65535).contains(guest)
        else { throw DriveSpecError(description: "port forward must be HOST:GUEST with ports 1-65535: \(spec)") }
        self.init(host: host, guest: guest)
    }
}
