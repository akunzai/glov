import Foundation

/// A short directory: unix socket paths are limited to 103 bytes on macOS.
func shortTempDir() throws -> String {
    var template = Array("/tmp/gp.XXXXXX".utf8CString)
    guard let path = mkdtemp(&template) else { throw CocoaError(.fileWriteUnknown) }
    return String(cString: path)
}
