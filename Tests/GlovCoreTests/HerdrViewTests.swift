import Foundation
import Testing

@testable import GlovCore

@Suite struct HerdrViewTests {
    @Test func theMachineIsReachedThroughThisGlovsSSH() {
        let via = HerdrView.via(glov: "/opt/glov/bin/glov", home: "/Users/me/my home", vm: "app-1a2b3c")
        #expect(via == "env GLOV_HOME='/Users/me/my home' /opt/glov/bin/glov ssh app-1a2b3c --")
        #expect(HerdrView.via(glov: "/g", home: nil, vm: "v") == "/g ssh v --")
        #expect(HerdrView.isVM(name: "app-1a2b3c", target: via, vm: "app-1a2b3c"))
        #expect(!HerdrView.isVM(name: "app-1a2b3c", target: "app-1a2b3c", vm: "app-1a2b3c"), "an ssh host of that name is not the VM")
        #expect(!HerdrView.isVM(name: "other", target: via, vm: "app-1a2b3c"))
    }

    @Test func hcAndHerdrAppListingsAreRead() {
        let machines = Data(#"{"machines":[{"name":"v","target":"/g ssh v --","state":"connected","via":true}]}"#.utf8)
        #expect(HerdrView.machines(machines) == [.init(name: "v", target: "/g ssh v --")])
        let ls = Data(#"{"terminals":[{"address":"v/t1","name":"claude","state":"live"},{"address":"v/t2","name":"claude-2","state":"finished"},{"address":"t3","name":"codex","state":"live"}]}"#.utf8)
        #expect(HerdrView.liveNames(ls, vm: "v") == ["claude"])
        let app = Data(#"""
            {"spaces":[{"id":"sp1","folder":"/p","machine":null,"tabs":[{"name":"claude","terminals":[{"terminal":"t9"}]}]},
                       {"id":"sp2","folder":"/p","machine":"v","tabs":[{"id":"v/tb4","name":"claude","terminals":[{"terminal":"v/t4"}]},{"name":null,"terminals":[]}]}]}
            """#.utf8)
        #expect(HerdrView.place(app, vm: "v", folder: "/p", tab: "claude") == .init(space: "sp2", tabs: ["claude"], terminal: "v/t4", tab: "v/tb4"))
        #expect(HerdrView.place(app, vm: "v", folder: "/q", tab: "claude") == .init())
        #expect(HerdrView.place(Data("not json".utf8), vm: "v", folder: "/p", tab: "claude") == .init())
    }

    /// `hc get --json` as hc 0.1.19 reports each case, seen on a VZ guest across `glov vm reboot`.
    @Test func aSessionLeftAtAShellIsToldFromTheHarness() {
        func occupant(_ json: String) -> HerdrView.Occupant { HerdrView.occupant(Data(json.utf8), command: "claude") }
        // hc run --keep: running, then replaced by a shell after the restart.
        #expect(occupant(#"{"argv":["claude","-c"],"foreground":null}"#) == .harness)
        #expect(occupant(#"{"argv":["/bin/bash"],"foreground":{"argv":["/bin/bash"],"name":"bash"}}"#) == .shell)
        // herdr-app's tab: its wrapper runs the harness, then a login shell once it exits.
        let wrapper = #"["sh","-c","shell=${SHELL:-/bin/sh}\n...","sh","claude"]"#
        #expect(occupant(#"{"argv":\#(wrapper),"foreground":null}"#) == .harness)
        #expect(occupant(#"{"argv":\#(wrapper),"foreground":{"argv":["claude"],"name":"2.1.319"}}"#) == .harness)
        #expect(occupant(#"{"argv":\#(wrapper),"foreground":{"argv":["/bin/bash","-li"],"name":"bash"}}"#) == .shell)
        #expect(occupant(#"{"argv":["/bin/bash"],"foreground":{"argv":["-zsh"],"name":"zsh"}}"#) == .shell)
        // Someone typed into the kept shell.
        #expect(occupant(#"{"argv":["/bin/bash"],"foreground":{"argv":["vim","notes.md"],"name":"vim"}}"#) == .other("vim"))
        // A hostile guest names the program: what reaches the Mac's terminal carries no escape sequence.
        #expect(occupant(#"{"argv":["/bin/bash"],"foreground":{"argv":["\u001b]52;c;eA==\u0007x"],"name":"x"}}"#) == .other("52ceAx"))
        #expect(occupant(#"{"argv":["/bin/bash"],"foreground":{"argv":["\u001b\u009b"],"name":"x"}}"#) == .other("a program"))
        #expect(occupant("not json") == .harness)
        // Between the machine coming back and hc starting the kept terminal's shell, hc still names the harness.
        #expect(HerdrView.restoring(Data(#"{"argv":["claude"],"pid":null,"finished_ms":null,"reason":"stopped_by_restart","state":"live"}"#.utf8)))
        #expect(!HerdrView.restoring(Data(#"{"argv":["/bin/bash"],"pid":1515,"finished_ms":null,"foreground":{"argv":["/bin/bash"],"name":"bash"}}"#.utf8)))
        #expect(!HerdrView.restoring(Data(#"{"argv":["claude"],"pid":null,"finished_ms":1794234052785,"exit_code":0}"#.utf8)))
        #expect(!HerdrView.restoring(Data("not json".utf8)))
        #expect(HerdrView.restarting("claude", vm: "v", command: "claude") == "v/claude was left at a shell (the VM restarted, or claude exited); starting claude again")
        #expect(HerdrView.leftAlone("claude", vm: "v", program: "top", command: "claude") == "v/claude runs top, not claude; showing it as it is (quit top and run again to start claude)")
    }

    /// The warning's pointer names a page of this repository that writes the herdr risks up, and its section.
    @Test func theWarningPointsToTheHerdrRisks() throws {
        let link = try #require(HerdrView.warning.firstMatch(of: /See (docs\/[\w.-]+\.md)#([\w-]+)\./))
        let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
        let page = try String(contentsOf: root.appendingPathComponent(String(link.1)), encoding: .utf8)
        // GitHub's anchor of a heading: lowercased, spaces as hyphens.
        let anchors = page.split(separator: "\n").filter { $0.hasPrefix("#") }
            .map { $0.drop { $0 == "#" }.trimmingCharacters(in: .whitespaces).lowercased().replacingOccurrences(of: " ", with: "-") }
        #expect(anchors.contains(String(link.2)), "\(link.1) has no section #\(link.2)")
        #expect(page.localizedCaseInsensitiveContains("herdr"))
    }
}
