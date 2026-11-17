import AppKit

/// The Dock icon of the viewer: the glov viewer mark, badged for the guest OS. The image and badges are embedded in the
/// binary (see `Package.swift`), so the icon needs no resource bundle beside it.
enum AppIcon {
    enum Variant: Equatable { case plain, windows, linux }

    /// Windows runs on the `qemu` engine and Linux on `vz`; anything else gets the plain mark.
    static func variant(engine: String?) -> Variant {
        switch engine {
        case "qemu": .windows
        case "vz": .linux
        default: .plain
        }
    }

    static func image(_ variant: Variant) -> NSImage? {
        guard let base = NSImage(data: Data(PackageResources.icon_png)), base.size.width > 0 else { return nil }
        let badge: [UInt8]
        switch variant {
        case .plain: return base
        case .windows: badge = PackageResources.badge_windows_svg
        case .linux: badge = PackageResources.badge_linux_svg
        }
        // The badge SVGs share the mark's 1024-unit canvas, so drawing both over the same rect lines them up.
        guard let overlay = NSImage(data: Data(badge)) else { return nil }
        let size = base.size
        return NSImage(size: size, flipped: false) { rect in
            base.draw(in: rect)
            overlay.draw(in: rect)
            return true
        }
    }
}
