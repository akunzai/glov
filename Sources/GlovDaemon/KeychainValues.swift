import Foundation
import Security
import GlovDaemonAPI
import Synchronization

/// Keychain values by name (secrets). `glovd` alone reads them: no RPC method returns a value.
protocol ValueStore: Sendable {
    func set(_ name: String, _ value: String) throws
    /// Nil if there is no key by that name.
    func get(_ name: String) throws -> String?
    func names() throws -> [String]
    /// False if there was none.
    func remove(_ name: String) throws -> Bool
}

/// Generic passwords in the login Keychain under one service. `glovd` creates each item, so it reads them back without
/// a prompt (an ad-hoc signed daemon rebuilt since may get one).
struct KeychainValues: ValueStore {
    var service = "glov secrets"

    func query(_ name: String? = nil) -> [String: Any] {
        var q: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service]
        if let name { q[kSecAttrAccount as String] = name }
        return q
    }

    func check(_ status: OSStatus, _ what: String) throws {
        guard status == errSecSuccess else {
            let text = SecCopyErrorMessageString(status, nil) as String? ?? "OSStatus \(status)"
            throw RPCError(code: RPCError.failed, message: "Keychain: cannot \(what): \(text)")
        }
    }

    /// Replaces the item rather than updating it: an update keeps the old item's access list, which trusts only the
    /// daemon build that created it, so after a rebuild `glov secret set` would not make the value readable again.
    func set(_ name: String, _ value: String) throws {
        let deleted = SecItemDelete(query(name) as CFDictionary)
        if deleted != errSecItemNotFound { try check(deleted, "replace \(name)") }
        var add = query(name)
        add[kSecValueData as String] = Data(value.utf8)
        add[kSecAttrLabel as String] = "glov: \(name)"
        // Past the delete the old value is gone; a failed add says so, so the user knows to store the value again.
        let added = SecItemAdd(add as CFDictionary, nil)
        if added != errSecSuccess, deleted == errSecSuccess { try check(added, "store \(name) (the previous value was already removed)") }
        try check(added, "store \(name)")
    }

    func get(_ name: String) throws -> String? {
        var q = query(name)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return nil }
        try check(status, "read \(name)")
        return (out as? Data).map { String(decoding: $0, as: UTF8.self) }
    }

    func names() throws -> [String] {
        var q = query()
        q[kSecReturnAttributes as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitAll
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return [] }
        try check(status, "list \(service)")
        return ((out as? [[String: Any]]) ?? []).compactMap { $0[kSecAttrAccount as String] as? String }.sorted()
    }

    func remove(_ name: String) throws -> Bool {
        let status = SecItemDelete(query(name) as CFDictionary)
        if status == errSecItemNotFound { return false }
        try check(status, "remove \(name)")
        return true
    }
}

/// Values in memory, for tests.
final class MemoryValues: ValueStore {
    private let values = Mutex<[String: String]>([:])

    init(_ initial: [String: String] = [:]) { values.withLock { $0 = initial } }

    func set(_ name: String, _ value: String) { values.withLock { $0[name] = value } }
    func get(_ name: String) -> String? { values.withLock { $0[name] } }
    func names() -> [String] { values.withLock { $0.keys.sorted() } }
    func remove(_ name: String) -> Bool { values.withLock { $0.removeValue(forKey: name) != nil } }
}
