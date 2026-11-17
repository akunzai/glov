import Foundation
import Testing

@testable import GlovDaemonAPI

@Suite struct MCPTests {
    @Test(.timeLimit(.minutes(1)), arguments: ["glov"])
    func handshakeListsTheToolsAndReportsAMissingDaemonAsAToolError(executable: String) async throws {
        let home = GlovHome(path: try shortTempDir())
        defer { try? FileManager.default.removeItem(atPath: home.path) }
        let session = try MCPSession(home: home, executable: executable)
        defer { session.terminate() }

        let hello = try await session.initialize()
        #expect((hello["serverInfo"] as? [String: Any])?["name"] as? String == "glov")

        let listed = try await session.request("tools/list")
        let tools = try #require(listed["tools"] as? [[String: Any]])
        #expect(Set(tools.compactMap { $0["name"] as? String }) == [
            "vm_list", "vm_status", "vm_configure", "vm_start", "vm_stop", "vm_reboot", "vm_wait", "screenshot", "click", "move", "drag", "type", "key", "scroll",
            "exec", "exec_start", "exec_read", "exec_kill", "file_read", "file_write", "copy_to_vm", "copy_from_vm",
            "snapshot_list", "snapshot_create", "snapshot_revert", "snapshot_delete", "share_sync",
            "egress_list", "egress_allow", "egress_remove", "egress_log",
        ])
        func description(_ name: String) throws -> String { try #require(tools.first { $0["name"] as? String == name }?["description"] as? String) }
        #expect(try description("vm_configure").contains("stopped") && description("vm_configure").contains("never shrinks"))
        #expect(try description("exec").contains("powershell.exe") && description("exec").contains("30"))
        #expect(try description("copy_to_vm").contains("allowlist") && description("copy_from_vm").contains("allowlist"))
        #expect(try description("vm_wait").contains("guest-ready"))
        #expect(try description("egress_allow").contains("isolated") && description("egress_allow").contains("443"))
        #expect(try description("egress_allow").contains("next connection") && description("egress_log").contains("cursor"))
        #expect(try description("vm_reboot").contains("bootId") && description("vm_reboot").contains("force") && description("vm_reboot").contains("screenshot"))
        #expect(try description("share_sync").contains("SHA-256") && description("share_sync").contains("in use"))
        #expect(try description("snapshot_revert").contains("UEFI") && description("snapshot_create").contains("live"))
        let noDaemon = try await session.request("tools/call", ["name": "vm_list", "arguments": [String: Any]()])
        #expect(noDaemon["isError"] as? Bool == true)
        #expect(((noDaemon["content"] as? [[String: Any]])?.first?["text"] as? String)?.contains("glovd is not running") == true)
        for name in ["click", "move", "drag", "scroll"] {
            let description = try #require(tools.first { $0["name"] as? String == name }?["description"] as? String)
            #expect(description.contains("screenshot"), "\(name) must tell the Guest Service to take a screenshot first")
        }

        let result = try await session.request("tools/call", ["name": "vm_list", "arguments": [String: Any]()])
        #expect(result["isError"] as? Bool == true)
        let text = (result["content"] as? [[String: Any]])?.first?["text"] as? String
        #expect(text?.contains("glovd is not running") == true)
    }
}
