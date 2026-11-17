import CryptoKit
import Foundation
import GlovCore

/// The Media Creation Tool catalog (`products.xml` inside `products.cab`): one ESD per language, architecture and
/// edition. Several editions share one ESD, which holds all of them as separate images.
public struct WindowsCatalog {
    public struct ESD: Equatable, Sendable {
        public var fileName: String
        /// Lowercase, e.g. `zh-tw`.
        public var language: String
        /// As the catalog spells it, e.g. `Professional`; also the image's edition ID inside the ESD.
        public var edition: String
        public var architecture: String
        public var size: UInt64
        /// Lowercase hex.
        public var sha1: String
        public var url: URL
    }

    public var files: [ESD]

    public init(xml: Data) throws {
        let collector = FileCollector()
        let parser = XMLParser(data: xml)
        parser.delegate = collector
        guard parser.parse() else { throw MediaError("cannot read the catalog: \(parser.parserError.map { "\($0)" } ?? "invalid XML")") }
        files = collector.files.compactMap { f in
            guard let name = f["FileName"], let lang = f["LanguageCode"], let edition = f["Edition"], let arch = f["Architecture"],
                let size = f["Size"].flatMap({ UInt64($0) }), let sha1 = f["Sha1"], let url = f["FilePath"].flatMap({ URL(string: $0) })
            else { return nil }
            return ESD(fileName: name, language: lang.lowercased(), edition: edition, architecture: arch, size: size, sha1: sha1.lowercased(), url: url)
        }
        guard !files.isEmpty else { throw MediaError("the catalog lists no media files") }
    }

    /// The ARM64 ESD for `language` (e.g. `zh-TW`) and `edition` (e.g. `Professional`), both case-insensitive.
    public func select(language: String, edition: String) throws -> ESD {
        let arm = files.filter { $0.architecture.caseInsensitiveCompare("ARM64") == .orderedSame }
        let inLanguage = arm.filter { $0.language == language.lowercased() }
        guard !inLanguage.isEmpty else {
            let all = Set(arm.map(\.language)).sorted().joined(separator: ", ")
            throw MediaError("no ARM64 media for language \(language.lowercased()) in the catalog (it has \(all))")
        }
        guard let esd = inLanguage.first(where: { $0.edition.caseInsensitiveCompare(edition) == .orderedSame }) else {
            let all = Set(inLanguage.map(\.edition)).sorted().joined(separator: ", ")
            throw MediaError("no ARM64 \(edition) media for \(language.lowercased()) in the catalog (editions: \(all))")
        }
        return esd
    }

    /// Collects the child elements of each `<File>` as name/text pairs.
    private final class FileCollector: NSObject, XMLParserDelegate {
        var files: [[String: String]] = []
        private var current: [String: String]?
        private var text = ""

        func parser(_ parser: XMLParser, didStartElement name: String, namespaceURI: String?, qualifiedName: String?, attributes: [String: String] = [:]) {
            if name == "File" { current = [:] }
            text = ""
        }

        func parser(_ parser: XMLParser, foundCharacters string: String) { text += string }

        func parser(_ parser: XMLParser, didEndElement name: String, namespaceURI: String?, qualifiedName: String?) {
            if name == "File", let current {
                files.append(current)
                self.current = nil
            } else if current != nil {
                current?[name] = text.trimmingCharacters(in: .whitespacesAndNewlines)
            }
        }
    }
}

/// Windows 11 Arm64 install media from Microsoft's Media Creation Tool catalog: downloads one ESD (resuming an
/// interrupted download), refuses it unless its SHA1 matches the catalog, and rebuilds it into a UEFI-bootable ISO
/// with `wimlib-imagex` and `hdiutil makehybrid`. glov never hosts or mirrors the media.
///
/// Cache layout, keyed by the ESD's SHA1 (which fixes build and language) and the edition:
/// `<cache>/<sha1>/<esd file name>` and `<cache>/<sha1>/<Edition>.iso`. Both appear only when complete, so a second
/// VM finds them and neither downloads nor converts again.
public struct WindowsMedia: MediaDownloads {
    /// The Windows 11 Media Creation Tool catalog (`products.cab`, 24H2: newer media come from Windows Update).
    public static let catalogURL = URL(string: "https://go.microsoft.com/fwlink/?LinkId=2156292")!

    /// `$GLOV_CACHE/windows`, else `~/Library/Caches/glov/windows`.
    public static func defaultCache(environment: [String: String] = ProcessInfo.processInfo.environment) -> String {
        MediaCache.root(environment: environment) + "/windows"
    }

    public var cache: String
    var run: (String, [String]) throws -> (status: Int32, output: String) = LinuxProvisioner.runTool
    /// Fetches `url` into `path`, continuing from the end of an existing file.
    var download: (URL, String) throws -> Void = WindowsMedia.curl
    var findTool: (String) throws -> String = { name in
        do { return try VirtualMachine.find(name) } catch { throw MediaError("\(name) not found (brew install wimlib)") }
    }
    var freeSpace: (String) -> UInt64? = WindowsMedia.defaultFreeSpace
    var log: (String) -> Void = WindowsMedia.defaultLog

    public init(cache: String = WindowsMedia.defaultCache()) { self.cache = cache }

    public func directory(of esd: WindowsCatalog.ESD) -> String { cache + "/" + esd.sha1 }
    public func esdPath(_ esd: WindowsCatalog.ESD) -> String { directory(of: esd) + "/" + esd.fileName }
    public func isoPath(_ esd: WindowsCatalog.ESD) -> String { directory(of: esd) + "/" + esd.edition + ".iso" }

    /// Downloads the catalog and parses its `products.xml`.
    public func catalog(from url: URL = WindowsMedia.catalogURL) throws -> WindowsCatalog {
        let dir = cache + "/catalog"
        try? FileManager.default.removeItem(atPath: dir)
        try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        log("downloading the Windows catalog from \(url.host ?? "")")
        try download(url, dir + "/products.cab")
        let r = try run("/usr/bin/tar", ["-xf", dir + "/products.cab", "-C", dir, "products.xml"])
        guard r.status == 0 else { throw MediaError("cannot extract products.xml from the catalog: \(r.output)") }
        return try WindowsCatalog(xml: Data(contentsOf: URL(fileURLWithPath: dir + "/products.xml")))
    }

    /// The cached ISO for `esd`'s edition, building it (and fetching the ESD) first when missing.
    public func makeISO(_ esd: WindowsCatalog.ESD) throws -> String {
        let iso = isoPath(esd)
        if FileManager.default.fileExists(atPath: iso) {
            log("cached: \(iso)")
            return iso
        }
        let source = try fetch(esd)
        try convert(source, esd: esd, to: iso)
        return iso
    }

    /// The verified ESD in the cache, downloading it when missing.
    public func fetch(_ esd: WindowsCatalog.ESD) throws -> String {
        let fm = FileManager.default
        let path = esdPath(esd)
        if fm.fileExists(atPath: path) { return path }
        guard esd.url.host?.hasSuffix(".microsoft.com") == true else {
            throw MediaError("refusing to download from \(esd.url.host ?? esd.url.absoluteString): only Microsoft URLs are used")
        }
        try fm.createDirectory(atPath: directory(of: esd), withIntermediateDirectories: true)
        let have = (try? fm.attributesOfItem(atPath: path + ".partial")[.size] as? UInt64) ?? 0
        try requireSpace(esd.size - min(have, esd.size), for: "the download")
        try downloadVerified(esd.url, to: path, expected: esd.sha1, Insecure.SHA1.self, "SHA1")
        return path
    }

    /// virtio-win, pinned to the version whose driver paths `WindowsAssets.autounattendTemplate` lists. The archive's CHECKSUM file lists the RPMs
    /// (MD5), not the ISO, so this downloads the noarch RPM (smaller than the ISO), checks it, and extracts the ISO.
    public static let virtioVersion = "0.1.302"
    static let virtioArchive = URL(string: "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-\(virtioVersion)-1/")!
    public var virtioPath: String { cache + "/virtio-win-\(Self.virtioVersion).iso" }

    /// The cached virtio-win ISO, fetching it when missing.
    public func virtioISO() throws -> String {
        let fm = FileManager.default
        let iso = virtioPath
        if fm.fileExists(atPath: iso) {
            log("cached: \(iso)")
            return iso
        }
        try fm.createDirectory(atPath: cache, withIntermediateDirectories: true)
        let rpmName = "virtio-win-\(Self.virtioVersion)-1.noarch.rpm"
        let sums = cache + "/virtio-win.CHECKSUM"
        try? fm.removeItem(atPath: sums)
        defer { try? fm.removeItem(atPath: sums) }
        try download(Self.virtioArchive.appendingPathComponent("CHECKSUM"), sums)
        guard let md5 = Self.checksums(try String(contentsOfFile: sums, encoding: .utf8))[rpmName] else {
            throw MediaError("CHECKSUM lists no \(rpmName)")
        }
        try requireSpace(1 << 30, for: "virtio-win")
        let rpm = cache + "/" + rpmName
        try downloadVerified(Self.virtioArchive.appendingPathComponent(rpmName), to: rpm, expected: md5, Insecure.MD5.self, "MD5")
        defer { try? fm.removeItem(atPath: rpm) }
        let stage = cache + "/virtio-win.stage", member = "usr/share/virtio-win/virtio-win-\(Self.virtioVersion).iso"
        try? fm.removeItem(atPath: stage)
        defer { try? fm.removeItem(atPath: stage) }
        try fm.createDirectory(atPath: stage, withIntermediateDirectories: true)
        log("extracting virtio-win-\(Self.virtioVersion).iso")
        let r = try run("/usr/bin/tar", ["-xf", rpm, "-C", stage, member])  // bsdtar reads RPMs
        guard r.status == 0 else { throw MediaError("cannot extract \(member) from \(rpmName): \(r.output)") }
        try fm.moveItem(atPath: stage + "/" + member, toPath: iso)
        return iso
    }

    /// Setup media (image 1), `boot.wim` from WinPE and Setup (images 2 and 3, Setup bootable), the edition's image as
    /// `install.esd`, then an ISO that boots `efisys.bin` through El Torito on UEFI (platform 0xEF), like Microsoft's.
    func convert(_ source: String, esd: WindowsCatalog.ESD, to iso: String) throws {
        let wimlib = try findTool("wimlib-imagex")
        let index = try imageIndex(of: esd.edition, in: source, wimlib: wimlib, name: esd.fileName)
        try requireSpace(esd.size * 2, for: "the conversion")
        let fm = FileManager.default
        let base = String(iso.dropLast(".iso".count))
        let stage = base + ".stage", partial = base + ".partial.iso"
        try? fm.removeItem(atPath: stage)
        try? fm.removeItem(atPath: partial)
        defer { try? fm.removeItem(atPath: stage) }
        func tool(_ exe: String, _ args: [String], _ what: String) throws {
            let r = try whileRunning(what) { try run(exe, args) }
            guard r.status == 0 else { throw MediaError("\(URL(fileURLWithPath: exe).lastPathComponent) \(args.first ?? "") failed: \(r.output)") }
        }
        try tool(wimlib, ["apply", source, "1", stage], "extracting setup media")
        try tool(wimlib, ["export", source, "2", stage + "/sources/boot.wim", "--compress=LZX"], "building boot.wim")
        try tool(wimlib, ["export", source, "3", stage + "/sources/boot.wim", "--boot"], "building boot.wim (setup)")
        try tool(wimlib, ["export", source, String(index), stage + "/sources/install.esd", "--solid"], "exporting the \(esd.edition) image")
        let efisys = stage + "/efi/microsoft/boot/efisys.bin"
        guard let size = try? fm.attributesOfItem(atPath: efisys)[.size] as? UInt64 else { throw MediaError("the setup media has no efi/microsoft/boot/efisys.bin") }
        try tool("/usr/bin/hdiutil", [
            "makehybrid", "-quiet", "-iso", "-joliet", "-udf", "-default-volume-name", "CCCOMA_A64FRE_" + esd.language.uppercased(),
            "-eltorito-boot", efisys, "-no-emul-boot", "-boot-load-size", String((size + 511) / 512),
            "-eltorito-platform", "239", "-o", partial, stage,
        ], "building the ISO")
        try fm.moveItem(atPath: partial, toPath: iso)
        log("built \(iso)")
    }

    /// The index of the image whose edition ID is `edition`, from `wimlib-imagex info`.
    func imageIndex(of edition: String, in source: String, wimlib: String, name: String) throws -> Int {
        let r = try run(wimlib, ["info", source])
        guard r.status == 0 else { throw MediaError("wimlib-imagex info failed: \(r.output)") }
        var images: [(index: Int, edition: String)] = []
        var index: Int?
        for line in r.output.split(separator: "\n") {
            let parts = line.split(separator: ":", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
            guard parts.count == 2 else { continue }
            if parts[0] == "Index" { index = Int(parts[1]) }
            if parts[0] == "Edition ID", let i = index, parts[1] != "WindowsPE" { images.append((i, parts[1])) }
        }
        guard let match = images.first(where: { $0.edition.caseInsensitiveCompare(edition) == .orderedSame }) else {
            throw MediaError("\(name) has no \(edition) image (it has \(images.map(\.edition).sorted().joined(separator: ", ")))")
        }
        return match.index
    }
}
