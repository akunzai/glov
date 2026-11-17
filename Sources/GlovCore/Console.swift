import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

public struct PixelSize: Sendable, Equatable {
    public var width: Int
    public var height: Int
    public init(width: Int, height: Int) { self.width = width; self.height = height }
}

public struct PixelRect: Sendable, Equatable {
    public var x: Int, y: Int, width: Int, height: Int
    public init(x: Int, y: Int, width: Int, height: Int) { self.x = x; self.y = y; self.width = width; self.height = height }
}

public struct Screenshot: Sendable {
    public var png: Data
    /// Size of `png` (the crop when a region was requested).
    public var size: PixelSize
    /// Size of the whole scaled screen; click/move/drag coordinates are relative to this.
    public var frame: PixelSize
}

enum ImageProcessing {
    /// A binary PPM (P6, maxval 255) as QEMU's `screendump` writes it, or nil if `data` is not one.
    /// QEMU writes it in a few milliseconds where its PNG encoder takes hundreds on a busy desktop.
    static func decodePPM(_ data: Data) throws -> CGImage? {
        guard data.starts(with: Array("P6".utf8)) else { return nil }
        var fields: [Int] = []
        var i = data.startIndex + 2
        while fields.count < 3 {
            while i < data.endIndex, data[i] == 0x23 || isspace(Int32(data[i])) != 0 {  // whitespace and # comments
                if data[i] == 0x23 { while i < data.endIndex, data[i] != 0x0A { i += 1 } } else { i += 1 }
            }
            var n = 0, digits = 0
            while i < data.endIndex, data[i] >= 0x30, data[i] <= 0x39, digits < 6 { n = n * 10 + Int(data[i] - 0x30); i += 1; digits += 1 }
            guard digits > 0 else { throw GlovError.screenshotFailed("malformed PPM header") }
            fields.append(n)
        }
        i += 1  // the single whitespace byte before the pixels
        let (w, h) = (fields[0], fields[1])
        guard fields[2] == 255, w > 0, h > 0, data.endIndex - i >= w * h * 3,
            let provider = CGDataProvider(data: data[i..<(i + w * h * 3)] as CFData),
            let image = CGImage(
                width: w, height: h, bitsPerComponent: 8, bitsPerPixel: 24, bytesPerRow: w * 3,
                space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGBitmapInfo(rawValue: CGImageAlphaInfo.none.rawValue),
                provider: provider, decode: nil, shouldInterpolate: false, intent: .defaultIntent)
        else { throw GlovError.screenshotFailed("unsupported PPM (\(fields))") }
        return image
    }

    /// A PPM (see `decodePPM`) or any image ImageIO reads.
    static func decode(_ data: Data) throws -> CGImage {
        guard let image = try decodePPM(data) ?? CGImageSourceCreateWithData(data as CFData, nil).flatMap({ CGImageSourceCreateImageAtIndex($0, 0, nil) })
        else { throw GlovError.screenshotFailed("not a decodable image") }
        return image
    }

    static func process(image data: Data, maxWidth: Int, region: PixelRect?) throws -> Screenshot {
        try process(image: try decode(data), maxWidth: maxWidth, region: region)
    }

    /// Scales to `maxWidth` (never up), then crops `region` given in scaled-frame pixels (clamped).
    static func process(image: CGImage, maxWidth: Int, region: PixelRect?) throws -> Screenshot {
        var image = image
        if image.width > maxWidth {
            let h = max(1, Int((Double(image.height) * Double(maxWidth) / Double(image.width)).rounded()))
            guard let ctx = CGContext(
                data: nil, width: maxWidth, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)
            else { throw GlovError.screenshotFailed("cannot allocate scaled image") }
            ctx.interpolationQuality = .high
            ctx.draw(image, in: CGRect(x: 0, y: 0, width: maxWidth, height: h))
            guard let scaled = ctx.makeImage() else { throw GlovError.screenshotFailed("scaling failed") }
            image = scaled
        }
        let frame = PixelSize(width: image.width, height: image.height)
        if let r = region {
            let rect = CGRect(x: r.x, y: r.y, width: r.width, height: r.height)
                .intersection(CGRect(x: 0, y: 0, width: frame.width, height: frame.height))
            guard !rect.isNull, !rect.isEmpty, let cropped = image.cropping(to: rect) else {
                throw GlovError.screenshotFailed("region is outside the \(frame.width)x\(frame.height) screen")
            }
            image = cropped
        }
        let out = NSMutableData()
        guard let dest = CGImageDestinationCreateWithData(out, UTType.png.identifier as CFString, 1, nil) else {
            throw GlovError.screenshotFailed("cannot encode PNG")
        }
        CGImageDestinationAddImage(dest, image, nil)
        guard CGImageDestinationFinalize(dest) else { throw GlovError.screenshotFailed("cannot encode PNG") }
        return Screenshot(png: out as Data, size: PixelSize(width: image.width, height: image.height), frame: frame)
    }
}

/// The engine side of a `Console`: whole-screen frames and raw input.
protocol ScreenDevice: Sendable {
    /// The guest screen at its own resolution.
    func capture() async throws -> CGImage
    /// Moves the pointer to pixel (x, y) of a screenshot `frame` big.
    func move(x: Int, y: Int, frame: PixelSize) async throws
    /// `name`: left, right, middle, wheel-up, wheel-down, wheel-left or wheel-right.
    func button(_ name: String, down: Bool) async throws
    /// Presses the qcodes (see `Input`) together, then releases them.
    func keys(_ qcodes: [String]) async throws
}

/// QEMU's screen over QMP: `screendump` for frames, `input-send-event` and `send-key` for input.
struct QMPScreen: ScreenDevice {
    let qmp: QMPClient
    let scratchPath: String

    func capture() async throws -> CGImage {
        _ = try await qmp.execute("screendump", ["filename": .string(scratchPath), "format": "ppm"])
        defer { try? FileManager.default.removeItem(atPath: scratchPath) }
        return try ImageProcessing.decode(try Data(contentsOf: URL(fileURLWithPath: scratchPath)))
    }

    func move(x: Int, y: Int, frame: PixelSize) async throws {
        _ = try await qmp.execute("input-send-event", Input.move(x: x, y: y, frame: frame))
    }

    func button(_ name: String, down: Bool) async throws {
        _ = try await qmp.execute("input-send-event", Input.button(name, down: down))
    }

    func keys(_ qcodes: [String]) async throws {
        _ = try await qmp.execute("send-key", Input.sendKey(qcodes))
    }
}

/// Screenshots and input for one running VM. Coordinates are pixels of the screenshot a Guest Service sees
/// (at most `maxWidth` wide); the console remembers the latest frame size to convert them.
public actor Console {
    private let device: any ScreenDevice
    private let maxWidth: Int
    private var frame: PixelSize?

    /// `scratchPath` is a file QEMU may write, ideally inside the VM directory.
    public init(qmp: QMPClient, scratchPath: String, maxWidth: Int = 1280) {
        self.init(device: QMPScreen(qmp: qmp, scratchPath: scratchPath), maxWidth: maxWidth)
    }

    init(device: any ScreenDevice, maxWidth: Int = 1280) {
        self.device = device
        self.maxWidth = maxWidth
    }

    public func screenshot(maxWidth: Int? = nil, region: PixelRect? = nil) async throws -> Screenshot {
        let shot = try ImageProcessing.process(image: try await device.capture(), maxWidth: maxWidth ?? self.maxWidth, region: region)
        frame = shot.frame
        return shot
    }

    public func move(x: Int, y: Int) async throws {
        try await device.move(x: x, y: y, frame: try await currentFrame())
    }

    /// `button`: left, right or middle.
    public func click(x: Int, y: Int, button: String = "left", count: Int = 1) async throws {
        try await move(x: x, y: y)
        for _ in 0..<count {
            try await press(button, down: true)
            try await press(button, down: false)
        }
    }

    public func drag(fromX: Int, fromY: Int, toX: Int, toY: Int, button: String = "left") async throws {
        try await move(x: fromX, y: fromY)
        try await press(button, down: true)
        let steps = 8
        for i in 1...steps {
            try await move(x: fromX + (toX - fromX) * i / steps, y: fromY + (toY - fromY) * i / steps)
            try await Task.sleep(for: .milliseconds(10))
        }
        try await press(button, down: false)
    }

    /// Wheel ticks at (x, y): positive `dy` scrolls down, positive `dx` right.
    public func scroll(x: Int, y: Int, dx: Int = 0, dy: Int = 0) async throws {
        try await move(x: x, y: y)
        for (n, plus, minus) in [(dy, "wheel-down", "wheel-up"), (dx, "wheel-right", "wheel-left")] {
            for _ in 0..<abs(n) {
                let b = n > 0 ? plus : minus
                try await press(b, down: true)
                try await press(b, down: false)
            }
        }
    }

    /// Types text on a US layout; characters outside printable ASCII, newline and tab throw before any key is sent.
    public func type(_ text: String) async throws {
        let codes = try text.map { try Input.qcodes(for: $0) }
        for q in codes {
            try await device.keys(q)
            try await Task.sleep(for: .milliseconds(40))
        }
    }

    /// A key or chord such as "enter", "ctrl+alt+delete", "win+r", "f5".
    public func key(_ combo: String) async throws {
        try await device.keys(try Input.qcodes(forCombo: combo))
    }

    private func press(_ button: String, down: Bool) async throws {
        try await device.button(button, down: down)
    }

    private func currentFrame() async throws -> PixelSize {
        if let frame { return frame }
        return try await screenshot().frame
    }
}
