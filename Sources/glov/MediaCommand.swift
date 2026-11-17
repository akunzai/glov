import ArgumentParser
import Foundation
import GlovCore
import GlovDaemonAPI

struct MediaCommand: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "media",
        abstract: "Download Windows 11 Arm64 install media from Microsoft and build a bootable ISO.",
        discussion: """
            Reads Microsoft's Media Creation Tool catalog, downloads the ESD for --language from Microsoft (resuming an \
            interrupted download), refuses it unless its SHA1 matches the catalog, and builds an ISO for --edition with \
            wimlib-imagex (brew install wimlib) and hdiutil. Prints the ISO path. ESD and ISO are \
            cached under $GLOV_CACHE/windows (default ~/Library/Caches/glov/windows), so the next run reuses them. \
            Also fetches virtio-win \(WindowsMedia.virtioVersion) from the Fedora archive into the same cache, checked \
            against its CHECKSUM file. `glov vm create` uses the same cache, filling it first when needed.
            """)
    @Option(help: "Language code from the catalog, e.g. en-US or zh-TW.") var language = WindowsProvisioner.defaultLanguage
    @Option(help: "Edition ID of the image to install, e.g. Professional or Core.") var edition = "Professional"

    func run() async throws {
        try await report {
            // Off the cooperative pool: this downloads and waits for wimlib and hdiutil.
            try await Blocking.run { [self] in
                let media = WindowsMedia()
                let esd = try media.catalog().select(language: language, edition: edition)
                let iso = try media.makeISO(esd)
                FileHandle.standardError.write(Data("virtio-win: \(try media.virtioISO())\n".utf8))
                print(iso)
            }
        }
    }
}
