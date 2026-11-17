import AppKit
import Testing

@testable import GlovViewer

@Suite struct AppIconTests {
    @Test func picksTheBadgeFromTheEngine() {
        #expect(AppIcon.variant(engine: "qemu") == .windows)
        #expect(AppIcon.variant(engine: "vz") == .linux)
        #expect(AppIcon.variant(engine: nil) == .plain)
        #expect(AppIcon.variant(engine: "something-new") == .plain)
    }

    @Test func eachVariantLoadsAsAnImage() {
        for v in [AppIcon.Variant.plain, .windows, .linux] {
            let image = AppIcon.image(v)
            #expect(image != nil, "\(v)")
            #expect(image.map { $0.size.width > 0 && $0.size.height > 0 } == true, "\(v)")
            #expect(image?.tiffRepresentation?.isEmpty == false, "\(v)")
        }
    }

    @Test func badgedIconsDifferFromThePlainOne() {
        let plain = AppIcon.image(.plain)?.tiffRepresentation
        #expect(AppIcon.image(.windows)?.tiffRepresentation != plain)
        #expect(AppIcon.image(.linux)?.tiffRepresentation != plain)
        #expect(AppIcon.image(.windows)?.tiffRepresentation != AppIcon.image(.linux)?.tiffRepresentation)
    }
}
