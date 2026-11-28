import Foundation
import Testing

@testable import GlovDaemonAPI

@Suite struct WindowsCatalogTests {
    @Test func picksTheARM64FileForALanguageAndEdition() throws {
        let catalog = try WindowsCatalog(xml: Data(productsXMLFixture.utf8))
        let esd = try catalog.select(language: "zh-TW", edition: "professional")
        #expect(esd.fileName == "26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_zh-tw.esd")
        #expect(esd.language == "zh-tw" && esd.edition == "Professional" && esd.architecture == "ARM64")
        #expect(esd.size == 4_670_731_279)
        #expect(esd.sha1 == "d2bd29d2e839ada7c25dbb8befb17dcabc645adb")
        #expect(esd.url.absoluteString == "http://dl.delivery.mp.microsoft.com/filestreamingservice/files/5c6e1f2f-087e-45c4-a3c6-59745c776a74/26100.4349.250607-1500.ge_release_svc_refresh_CLIENTCONSUMER_RET_A64FRE_zh-tw.esd")
        #expect(try catalog.select(language: "en-us", edition: "Professional").sha1 == "0000000000000000000000000000000000000002")
    }

    @Test func anUnknownLanguageOrEditionNamesWhatTheCatalogHas() throws {
        let catalog = try WindowsCatalog(xml: Data(productsXMLFixture.utf8))
        #expect(throws: MediaError("no ARM64 media for language fr-fr in the catalog (it has en-us, zh-tw)")) {
            try catalog.select(language: "fr-fr", edition: "Professional")
        }
        #expect(throws: MediaError("no ARM64 Education media for zh-tw in the catalog (editions: Core, Professional)")) {
            try catalog.select(language: "zh-tw", edition: "Education")
        }
    }

    @Test func aCatalogWithoutFilesIsRejected() {
        #expect(throws: MediaError.self) { try WindowsCatalog(xml: Data("<html>not a catalog</html>".utf8)) }
    }
}

@Suite struct WindowsMediaTests {
    /// A tiny stand-in for the ESD, with the SHA1 its catalog entry claims.
    static let esdBytes = Data("not really an ESD".utf8)

    let cache: String
    let esd: WindowsCatalog.ESD
    let calls = ToolCalls()

    init() throws {
        cache = try shortTempDir()
        esd = .init(
            fileName: "26100.4349_A64FRE_zh-tw.esd", language: "zh-tw", edition: "Professional", architecture: "ARM64",
            size: UInt64(Self.esdBytes.count), sha1: "671d649ef58837ee2657f7a7643c01aacc3a81d6",
            url: URL(string: "http://dl.delivery.mp.microsoft.com/filestreamingservice/files/x/26100.4349_A64FRE_zh-tw.esd")!)
    }

    /// Fakes the network and the tools: downloads append `bytes`, wimlib and hdiutil create the files they would.
    func media(serving bytes: Data = WindowsMediaTests.esdBytes, files: [String: Data] = [:], failDownload: Bool = false) -> WindowsMedia {
        var m = WindowsMedia(cache: cache)
        let calls = calls
        m.findTool = { "/fake/" + $0 }
        m.log = { _ in }
        m.download = { url, path in
            calls.add("download", [url.absoluteString, path, FileManager.default.fileExists(atPath: path) ? "resume" : "new"])
            if failDownload { throw MediaError("connection reset") }
            if !FileManager.default.fileExists(atPath: path) { FileManager.default.createFile(atPath: path, contents: nil) }
            let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
            let have = try h.seekToEnd()
            try h.write(contentsOf: (files[url.lastPathComponent] ?? bytes).dropFirst(Int(have)))
            try h.close()
        }
        m.run = { exe, args in
            calls.add(URL(fileURLWithPath: exe).lastPathComponent, args)
            let fm = FileManager.default
            switch (URL(fileURLWithPath: exe).lastPathComponent, args.first) {
            case ("wimlib-imagex", "info"): return (0, wimInfoFixture)
            case ("wimlib-imagex", "apply"):
                try fm.createDirectory(atPath: args[3] + "/efi/microsoft/boot", withIntermediateDirectories: true)
                fm.createFile(atPath: args[3] + "/efi/microsoft/boot/efisys.bin", contents: Data(count: 1_720_320))
                try fm.createDirectory(atPath: args[3] + "/sources", withIntermediateDirectories: true)
            case ("wimlib-imagex", "export"): fm.createFile(atPath: args[3], contents: Data("wim".utf8))
            case ("tar", _):  // tar -xf <rpm> -C <dir> <member>
                let out = args[3] + "/" + args[4]
                try fm.createDirectory(atPath: (out as NSString).deletingLastPathComponent, withIntermediateDirectories: true)
                fm.createFile(atPath: out, contents: Data("virtio iso".utf8))
            case ("hdiutil", _): fm.createFile(atPath: args[args.firstIndex(of: "-o")! + 1], contents: Data("iso".utf8))
            default: break
            }
            return (0, "")
        }
        return m
    }

    @Test func downloadsVerifiesAndConvertsIntoACacheKeyedBySHA1AndEdition() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let iso = try media().makeISO(esd)
        let dir = cache + "/671d649ef58837ee2657f7a7643c01aacc3a81d6"
        #expect(iso == dir + "/Professional.iso")
        #expect(FileManager.default.contents(atPath: iso) == Data("iso".utf8))
        #expect(FileManager.default.contents(atPath: dir + "/26100.4349_A64FRE_zh-tw.esd") == Self.esdBytes)
        #expect(try FileManager.default.contentsOfDirectory(atPath: dir).sorted() == ["26100.4349_A64FRE_zh-tw.esd", "Professional.iso"])

        let esdPath = dir + "/26100.4349_A64FRE_zh-tw.esd", stage = dir + "/Professional.stage"
        #expect(calls.list.map(\.1) == [
            [esd.url.absoluteString, esdPath + ".partial", "new"],
            ["info", esdPath],
            ["apply", esdPath, "1", stage],
            ["export", esdPath, "2", stage + "/sources/boot.wim", "--compress=LZX"],
            ["export", esdPath, "3", stage + "/sources/boot.wim", "--boot"],
            ["export", esdPath, "5", stage + "/sources/install.esd", "--solid"],
            ["makehybrid", "-quiet", "-iso", "-joliet", "-udf", "-default-volume-name", "CCCOMA_A64FRE_ZH-TW",
             "-eltorito-boot", stage + "/efi/microsoft/boot/efisys.bin", "-no-emul-boot", "-boot-load-size", "3360",
             "-eltorito-platform", "239", "-o", dir + "/Professional.partial.iso", stage],
        ])
    }

    @Test func aSecondRunUsesTheCacheWithoutDownloadingOrConverting() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let first = try media().makeISO(esd)
        let before = calls.list.count
        #expect(try media().makeISO(esd) == first)
        #expect(calls.list.count == before)
    }

    @Test func anotherEditionReusesTheCachedESD() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        _ = try media().makeISO(esd)
        var home = esd
        home.edition = "Core"
        let iso = try media().makeISO(home)
        #expect(iso.hasSuffix("/671d649ef58837ee2657f7a7643c01aacc3a81d6/Core.iso"))
        #expect(calls.list.filter { $0.0 == "download" }.count == 1)
        #expect(calls.list.contains { $0.1.prefix(3) == ["export", cache + "/671d649ef58837ee2657f7a7643c01aacc3a81d6/26100.4349_A64FRE_zh-tw.esd", "4"] })
    }

    @Test func aSHA1MismatchIsRefusedAndNothingIsConverted() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        #expect(throws: MediaError("SHA1 mismatch for 26100.4349_A64FRE_zh-tw.esd: expected 671d649ef58837ee2657f7a7643c01aacc3a81d6, got 59cc1773abb751be595758f684c76575c78d7816; deleted the download")) {
            try media(serving: Data("not really an ESX".utf8)).makeISO(esd)
        }
        #expect(!calls.list.contains { $0.0 != "download" })
        #expect(try FileManager.default.contentsOfDirectory(atPath: cache + "/671d649ef58837ee2657f7a7643c01aacc3a81d6").isEmpty)
    }

    @Test func anInterruptedDownloadKeepsItsPartialFileAndTheNextRunResumesIt() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        let partial = cache + "/671d649ef58837ee2657f7a7643c01aacc3a81d6/26100.4349_A64FRE_zh-tw.esd.partial"
        #expect(throws: MediaError("connection reset")) { try media(failDownload: true).makeISO(esd) }
        try FileManager.default.createDirectory(atPath: cache + "/671d649ef58837ee2657f7a7643c01aacc3a81d6", withIntermediateDirectories: true)
        FileManager.default.createFile(atPath: partial, contents: Self.esdBytes.prefix(5))  // what the dropped connection left
        _ = try media().makeISO(esd)
        #expect(calls.list.filter { $0.0 == "download" }.map { $0.1[2] } == ["new", "resume"])
        #expect(!FileManager.default.fileExists(atPath: partial))
    }

    @Test func mediaFromOutsideMicrosoftIsRefused() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        var mirror = esd
        mirror.url = URL(string: "http://mirror.example.com/26100.4349_A64FRE_zh-tw.esd")!
        #expect(throws: MediaError("refusing to download from mirror.example.com: only Microsoft URLs are used")) { try media().makeISO(mirror) }
        #expect(calls.list.isEmpty)
    }

    @Test func anEditionTheESDDoesNotHoldIsRefusedBeforeConverting() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        var n = esd
        n.edition = "ProfessionalN"
        #expect(throws: MediaError("26100.4349_A64FRE_zh-tw.esd has no ProfessionalN image (it has Core, Professional)")) { try media().makeISO(n) }
        #expect(!calls.list.contains { $0.1.first == "apply" })
    }

    @Test func tooLittleFreeSpaceIsRefusedBeforeDownloading() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        var m = media()
        m.freeSpace = { _ in 10 }
        #expect(throws: MediaError.self) { try m.makeISO(esd) }
        #expect(calls.list.isEmpty)
    }

    @Test func missingWimlibIsRefusedBeforeDownloadingButNotWhenISOIsCached() throws {
        defer { try? FileManager.default.removeItem(atPath: cache) }
        var missing = media()
        missing.findTool = { name in throw MediaError("\(name) not found (brew install wimlib)") }
        #expect(throws: MediaError("wimlib-imagex not found (brew install wimlib)")) { try missing.makeISO(esd) }
        #expect(calls.list.isEmpty, "missing wimlib-imagex must fail before downloading the ESD")

        let cached = try media().makeISO(esd)
        missing.findTool = { name in throw MediaError("\(name) not found (brew install wimlib)") }
        #expect(try missing.makeISO(esd) == cached)
    }

    @Test func theCacheFollowsGlovCache() {
        #expect(WindowsMedia.defaultCache(environment: ["GLOV_CACHE": "/Volumes/X/cache"]) == "/Volumes/X/cache/windows")
        #expect(WindowsMedia.defaultCache(environment: [:]).hasSuffix("/Library/Caches/glov/windows"))
    }
}

@Suite struct VirtioWinTests {
    static let rpm = Data("not really an RPM".utf8)
    /// The archive's CHECKSUM file, with the noarch RPM's MD5 replaced by that of `rpm`.
    static let checksum = """
        214fbf58eadce468bea3380067f3d6a8  virtio-win-0.1.302-1.noarch.rpm
        09c1acd2ff72263c16a6afbf7a5f2e69  virtio-win-0.1.302-1.src.rpm
        214fbf58eadce468bea3380067f3d6a8  virtio-win.noarch.rpm
        09c1acd2ff72263c16a6afbf7a5f2e69  virtio-win.src.rpm

        """
    let tests: WindowsMediaTests

    init() throws { tests = try WindowsMediaTests() }

    @Test func checksumLinesMapFilesToDigests() {
        let sums = WindowsMedia.checksums(Self.checksum)
        #expect(sums["virtio-win-0.1.302-1.noarch.rpm"] == "214fbf58eadce468bea3380067f3d6a8")
        #expect(sums["virtio-win.src.rpm"] == "09c1acd2ff72263c16a6afbf7a5f2e69")
        #expect(sums.count == 4)
    }

    @Test func fetchesTheRPMVerifiesItAndKeepsOnlyTheISO() throws {
        defer { try? FileManager.default.removeItem(atPath: tests.cache) }
        let m = tests.media(files: ["CHECKSUM": Data(Self.checksum.utf8), "virtio-win-0.1.302-1.noarch.rpm": Self.rpm])
        let iso = try m.virtioISO()
        #expect(iso == tests.cache + "/virtio-win-0.1.302.iso")
        #expect(FileManager.default.contents(atPath: iso) == Data("virtio iso".utf8))
        let base = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.302-1/"
        #expect(tests.calls.list.filter { $0.0 == "download" }.map { $0.1[0] } == [base + "CHECKSUM", base + "virtio-win-0.1.302-1.noarch.rpm"])
        #expect(try FileManager.default.contentsOfDirectory(atPath: tests.cache).filter { $0.hasPrefix("virtio") } == ["virtio-win-0.1.302.iso"])

        let before = tests.calls.list.count
        #expect(try m.virtioISO() == iso)
        #expect(tests.calls.list.count == before)
    }

    @Test func anMD5MismatchIsRefusedAndNothingIsExtracted() throws {
        defer { try? FileManager.default.removeItem(atPath: tests.cache) }
        let m = tests.media(files: ["CHECKSUM": Data(Self.checksum.utf8), "virtio-win-0.1.302-1.noarch.rpm": Data("not really an RPX".utf8)])
        #expect(throws: MediaError("MD5 mismatch for virtio-win-0.1.302-1.noarch.rpm: expected 214fbf58eadce468bea3380067f3d6a8, got fbdbb935c650c8590d3a4705673ee083; deleted the download")) {
            try m.virtioISO()
        }
        #expect(!tests.calls.list.contains { $0.0 == "tar" })
        #expect(try FileManager.default.contentsOfDirectory(atPath: tests.cache).filter { $0.hasPrefix("virtio") }.isEmpty)
    }

    @Test func aChecksumFileWithoutTheRPMIsRefused() throws {
        defer { try? FileManager.default.removeItem(atPath: tests.cache) }
        let m = tests.media(files: ["CHECKSUM": Data("09c1acd2ff72263c16a6afbf7a5f2e69  virtio-win.src.rpm\n".utf8)])
        #expect(throws: MediaError("CHECKSUM lists no virtio-win-0.1.302-1.noarch.rpm")) { try m.virtioISO() }
        #expect(tests.calls.list.filter { $0.0 == "download" }.count == 1)
    }
}

final class ToolCalls: @unchecked Sendable {
    private let lock = NSLock()
    private var _list: [(String, [String])] = []
    var list: [(String, [String])] { lock.withLock { _list } }
    func add(_ exe: String, _ args: [String]) { lock.withLock { _list.append((exe, args)) } }
}

/// The image list of `wimlib-imagex info` on the zh-tw ARM64 ESD, trimmed to the fields that matter.
let wimInfoFixture = """
    WIM Information:
    ----------------
    Path:           zh-tw.esd
    Image Count:    5
    Compression:    LZMS
    Boot Index:     3

    Available Images:
    -----------------
    Index:                  1
    Name:                   Windows Setup Media
    Description:            Windows Setup Media

    Index:                  2
    Name:                   Microsoft Windows PE (arm64)
    Architecture:           ARM64
    Edition ID:             WindowsPE

    Index:                  3
    Name:                   Microsoft Windows Setup (arm64)
    Architecture:           ARM64
    Edition ID:             WindowsPE

    Index:                  4
    Name:                   Windows 11 Home
    Architecture:           ARM64
    Edition ID:             Core

    Index:                  5
    Name:                   Windows 11 Pro
    Architecture:           ARM64
    Edition ID:             Professional

    """

extension WindowsMediaTests {
    @Test func aLongStepSaysItIsStillRunning() async throws {
        final class Lines: @unchecked Sendable {
            let lock = NSLock()
            var all: [String] = []
            let heartbeat = DispatchSemaphore(value: 0)
            func add(_ s: String) {
                lock.withLock { all.append(s) }
                if s.hasPrefix("still") { heartbeat.signal() }
            }
        }
        let lines = Lines()
        var m = media()
        m.log = { lines.add($0) }
        let media = m
        // The step blocks, as hdiutil does, so it runs off the cooperative pool. It ends once the first heartbeat is
        // out, not after a fixed time, which a loaded runner can stretch past the heartbeat.
        let r = try await offPool {
            media.whileRunning("building the ISO", every: .seconds(1)) { _ = lines.heartbeat.wait(timeout: .now() + 30); return 7 }
        }
        try await Task.sleep(for: .seconds(1))
        #expect(r == 7)
        // The elapsed time is the runner's: a loaded one sends the first heartbeat late.
        let seen = lines.lock.withLock { lines.all }
        #expect(seen.count == 2 && seen.first == "building the ISO", "the heartbeat stops with the step: \(seen)")
        #expect(seen.last?.hasPrefix("still building the ISO (0m ") == true, "\(seen)")
    }
}
