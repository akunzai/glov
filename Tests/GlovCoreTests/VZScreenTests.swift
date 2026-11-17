import CoreGraphics
import IOSurface
import Testing

@testable import GlovCore

@Suite struct VZScreenTests {
    @Test func screenshotPixelCentersMapToBottomUpViewPoints() {
        let view = CGSize(width: 1280, height: 800)
        let full = PixelSize(width: 1280, height: 800)
        #expect(VZInput.viewPoint(x: 0, y: 0, frame: full, view: view) == CGPoint(x: 0.5, y: 799.5))
        #expect(VZInput.viewPoint(x: 1279, y: 799, frame: full, view: view) == CGPoint(x: 1279.5, y: 0.5))
        // A screenshot scaled to 640 wide addresses the same spot of the guest screen.
        #expect(VZInput.viewPoint(x: 320, y: 200, frame: PixelSize(width: 640, height: 400), view: view) == CGPoint(x: 641, y: 399))
        // Off-screen coordinates are clamped to the edge, as the QEMU tablet does.
        #expect(VZInput.viewPoint(x: -5, y: 5000, frame: full, view: view) == CGPoint(x: 0, y: 0))
    }

    @Test func aChordPressesModifiersFirstAndReleasesInReverse() throws {
        let ctrl = VZInput.ctrl, alt = VZInput.alt
        #expect(try VZInput.strokes(["ctrl", "alt", "delete"]) == [
            .init(keyCode: 0x3B, down: true, isModifier: true, flags: ctrl),
            .init(keyCode: 0x3A, down: true, isModifier: true, flags: ctrl | alt),
            .init(keyCode: 0x75, down: true, isModifier: false, flags: ctrl | alt),
            .init(keyCode: 0x75, down: false, isModifier: false, flags: ctrl | alt),
            .init(keyCode: 0x3A, down: false, isModifier: true, flags: ctrl),
            .init(keyCode: 0x3B, down: false, isModifier: true, flags: 0),
        ])
    }

    @Test func aRepeatedKeyInAComboIsPressedOnce() throws {
        // "shift+A" is ["shift", "shift", "a"] in qcodes.
        #expect(try VZInput.strokes(["shift", "shift", "a"]).map(\.keyCode) == [0x38, 0x00, 0x00, 0x38])
    }

    @Test func modifierFlagsCarryTheLeftHandDeviceBitVZReads() {
        // NSEvent.ModifierFlags.shift plus NX_DEVICELSHIFTKEYMASK; without the device bit VZ ignores the modifier.
        #expect(VZInput.shift == 0x20002)
        #expect(VZInput.ctrl == 0x40001)
        #expect(VZInput.alt == 0x80020)
        #expect(VZInput.command == 0x100008)
    }

    @Test func everyKeyAConsoleCanSendHasAMacKeyCode() throws {
        for v in 0x20...0x7e { _ = try VZInput.strokes(try Input.qcodes(for: Character(UnicodeScalar(UInt8(v))))) }
        for name in Input.keyNames.keys { _ = try VZInput.strokes(try Input.qcodes(forCombo: name)) }
        for n in 1...12 { _ = try VZInput.strokes(try Input.qcodes(forCombo: "f\(n)")) }
        #expect(try VZInput.strokes(["ret"]).first?.keyCode == 0x24)
        #expect(try VZInput.strokes(["meta_l"]).first?.flags == VZInput.command)
        #expect(throws: GlovError.unknownKey("bogus")) { try VZInput.strokes(["bogus"]) }
    }

    @Test func aWheelButtonScrollsOneNotchOf120Pixels() {
        // VZ passes pixel deltas through as 1/120-notch wheel units (measured: 120 px is one REL_WHEEL step in a Linux guest),
        // and its horizontal axis is positive to the right.
        #expect(VZInput.wheel("wheel-up")! == (dy: 120, dx: 0))
        #expect(VZInput.wheel("wheel-down")! == (dy: -120, dx: 0))
        #expect(VZInput.wheel("wheel-left")! == (dy: 0, dx: -120))
        #expect(VZInput.wheel("wheel-right")! == (dy: 0, dx: 120))
        #expect(VZInput.wheel("left") == nil)
    }

    @Test func aBGRAFramebufferSurfaceBecomesAnImage() throws {
        let (w, h) = (4, 2)
        let surface = IOSurfaceCreate([
            kIOSurfaceWidth: w, kIOSurfaceHeight: h, kIOSurfaceBytesPerElement: 4,
            kIOSurfacePixelFormat: 0x4247_5241,  // 'BGRA'
        ] as CFDictionary)!
        IOSurfaceLock(surface, [], nil)
        let base = IOSurfaceGetBaseAddress(surface).assumingMemoryBound(to: UInt8.self)
        let bpr = IOSurfaceGetBytesPerRow(surface)
        for y in 0..<h { for x in 0..<w { base.advanced(by: y * bpr + x * 4).initialize(from: [0, 0, 0, 255], count: 4) } }
        base.advanced(by: bpr + 3 * 4).initialize(from: [0, 0, 255, 255], count: 4)  // bottom-right pixel red (B, G, R, A)
        IOSurfaceUnlock(surface, [], nil)

        let shot = try ImageProcessing.process(image: try VZScreen.image(from: surface), maxWidth: 1280, region: nil)
        #expect(shot.size == PixelSize(width: 4, height: 2))
        let decoded = try ImageProcessing.decode(shot.png)
        let ctx = CGContext(
            data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4,
            space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        ctx.draw(decoded, in: CGRect(x: 0, y: 0, width: w, height: h))
        let px = ctx.data!.assumingMemoryBound(to: UInt8.self)
        #expect(Array(UnsafeBufferPointer(start: px + (w * 4 + 3 * 4), count: 3)) == [255, 0, 0])
        #expect(Array(UnsafeBufferPointer(start: px, count: 3)) == [0, 0, 0])
    }

    @Test func aMissingFramebufferLongAfterStartPointsAtMacOSRatherThanTheGuest() {
        #expect(VZScreen.noFramebuffer(after: .seconds(5)).description == "screenshot failed: the guest has not drawn its screen yet")
        let late = VZScreen.noFramebuffer(after: .seconds(45)).description
        #expect(late.contains("after 45 s") && late.contains("macOS update") && late.contains("VZScreen.swift"))
    }

    @Test func otherPixelFormatsAreRefused() {
        let surface = IOSurfaceCreate([
            kIOSurfaceWidth: 2, kIOSurfaceHeight: 2, kIOSurfaceBytesPerElement: 4, kIOSurfacePixelFormat: 0x6C31_3072,  // 'l10r'
        ] as CFDictionary)!
        #expect(throws: GlovError.self) { try VZScreen.image(from: surface) }
    }
}
