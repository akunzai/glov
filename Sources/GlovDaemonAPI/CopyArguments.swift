import Foundation

/// The `src dst` pair of `glov cp`, where the guest side is written `vm:path`.
public enum CopyDirection: Equatable {
    case toVM(vm: String, hostPath: String, guestPath: String)
    case fromVM(vm: String, guestPath: String, hostPath: String)

    public struct Invalid: Error, CustomStringConvertible {
        public var description: String
    }

    /// `vm:path` when the text before the first colon is a VM name of two or more characters (so `C:\dir` stays a
    /// path); anything else is a host path. Host paths are made absolute against `directory`.
    public static func parse(_ source: String, _ destination: String, directory: String, remoteHost: Bool = false) throws -> CopyDirection {
        func guest(_ s: String) -> (vm: String, path: String)? {
            guard let colon = s.firstIndex(of: ":"), s.distance(from: s.startIndex, to: colon) >= 2, s.index(after: colon) < s.endIndex else { return nil }
            let vm = String(s[..<colon])
            return GlovHome.isValid(name: vm) ? (vm, String(s[s.index(after: colon)...])) : nil
        }
        func host(_ s: String) throws -> String {
            if remoteHost {
                guard s.hasPrefix("/") else { throw Invalid(description: "host path must be an absolute path on the remote Mac; use SSH/SCP to transfer laptop files first") }
                return s
            }
            let path = (s as NSString).expandingTildeInPath
            return path.hasPrefix("/") ? path : (directory as NSString).appendingPathComponent(path)
        }
        switch (guest(source), guest(destination)) {
        case (nil, let to?): return .toVM(vm: to.vm, hostPath: try host(source), guestPath: to.path)
        case (let from?, nil): return .fromVM(vm: from.vm, guestPath: from.path, hostPath: try host(destination))
        case (nil, nil): throw Invalid(description: "one side must be a guest path written vm:path, for example win11:C:\\work")
        default: throw Invalid(description: "copying from one VM to another is not supported; copy through the host")
        }
    }
}
