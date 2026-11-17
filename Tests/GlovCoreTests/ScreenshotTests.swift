import CoreGraphics
import Foundation
import ImageIO
import Testing
import UniformTypeIdentifiers

@testable import GlovCore

func makePNG(width: Int, height: Int) -> Data {
    let ctx = CGContext(
        data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
        space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)!
    ctx.setFillColor(red: 1, green: 0, blue: 0, alpha: 1)
    ctx.fill(CGRect(x: 0, y: 0, width: width, height: height))
    let out = NSMutableData()
    let d = CGImageDestinationCreateWithData(out, UTType.png.identifier as CFString, 1, nil)!
    CGImageDestinationAddImage(d, ctx.makeImage()!, nil)
    CGImageDestinationFinalize(d)
    return out as Data
}

@Suite struct ScreenshotTests {
    @Test func downscalesWideImagesKeepingAspect() throws {
        let s = try ImageProcessing.process(image: makePNG(width: 2560, height: 1440), maxWidth: 1280, region: nil)
        #expect(s.size == PixelSize(width: 1280, height: 720))
        #expect(s.frame == s.size)
        #expect(s.png.prefix(4) == Data([0x89, 0x50, 0x4e, 0x47]))
    }

    @Test func neverUpscales() throws {
        let s = try ImageProcessing.process(image: makePNG(width: 800, height: 600), maxWidth: 1280, region: nil)
        #expect(s.size == PixelSize(width: 800, height: 600))
    }

    @Test func cropsInScaledCoordinatesAndKeepsTheFrameSize() throws {
        let s = try ImageProcessing.process(
            image: makePNG(width: 2560, height: 1440), maxWidth: 1280, region: PixelRect(x: 100, y: 50, width: 200, height: 100))
        #expect(s.size == PixelSize(width: 200, height: 100))
        #expect(s.frame == PixelSize(width: 1280, height: 720))
    }

    @Test func clampsRegionToTheFrameAndRejectsOutsideOnes() throws {
        let png = makePNG(width: 800, height: 600)
        let s = try ImageProcessing.process(image: png, maxWidth: 1280, region: PixelRect(x: 700, y: 500, width: 500, height: 500))
        #expect(s.size == PixelSize(width: 100, height: 100))
        #expect(throws: GlovError.self) {
            try ImageProcessing.process(image: png, maxWidth: 1280, region: PixelRect(x: 900, y: 0, width: 10, height: 10))
        }
    }

    @Test func decodesQEMUsPPMIncludingAHeaderComment() throws {
        var ppm = Data("P6\n# QEMU\n3 2\n255\n".utf8)
        ppm.append(contentsOf: [UInt8](repeating: 0, count: 3 * 2 * 3))
        ppm[ppm.count - 3] = 255  // bottom-right pixel red
        let s = try ImageProcessing.process(image: ppm, maxWidth: 1280, region: nil)
        #expect(s.size == PixelSize(width: 3, height: 2))
        let decoded = CGImageSourceCreateImageAtIndex(CGImageSourceCreateWithData(s.png as CFData, nil)!, 0, nil)!
        #expect(decoded.width == 3 && decoded.height == 2)
    }

    @Test func rejectsTruncatedOrNon8BitPPM() {
        #expect(throws: GlovError.self) { try ImageProcessing.process(image: Data("P6\n3 2\n255\n\0\0".utf8), maxWidth: 1280, region: nil) }
        #expect(throws: GlovError.self) { try ImageProcessing.process(image: Data("P6\n1 1\n65535\n\0\0\0\0\0\0".utf8), maxWidth: 1280, region: nil) }
        #expect(throws: GlovError.self) { try ImageProcessing.process(image: Data("P6\nx".utf8), maxWidth: 1280, region: nil) }
    }

    @Test func rejectsNonImages() {
        #expect(throws: GlovError.self) { try ImageProcessing.process(image: Data("nope".utf8), maxWidth: 1280, region: nil) }
    }
}
