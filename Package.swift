// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "glov",
    platforms: [.macOS(.v26)],
    products: [
        .library(name: "GlovCore", targets: ["GlovCore"]),
        .executable(name: "glovd", targets: ["glovd"]),
        .executable(name: "glov", targets: ["glov"]),
    ],
    dependencies: [
        .package(url: "https://github.com/modelcontextprotocol/swift-sdk", from: "0.12.1"),
        .package(url: "https://github.com/apple/swift-argument-parser", from: "1.8.2"),
        .package(url: "https://github.com/apple/swift-log", from: "1.16.2"),
        .package(url: "https://github.com/apple/swift-nio", from: "2.104.0"),
        .package(url: "https://github.com/apple/swift-nio-ssl", from: "2.37.6"),
        .package(url: "https://github.com/apple/swift-certificates", from: "1.21.1"),
        .package(url: "https://github.com/apple/swift-crypto", from: "5.1.0"),
    ],
    targets: [
        // Harness profiles (glov vm create --harness) are data files compiled into the binary (.embedInCode).
        .target(name: "GlovCore", resources: [
            .embedInCode("Harnesses/antigravity.json"), .embedInCode("Harnesses/claude.json"), .embedInCode("Harnesses/codex.json"),
        ]),
        // JSON-RPC types, the VM directory layout and the socket client, shared by the daemon and the CLI.
        // The egress CA (EgressCA) lives here because `glov vm create` writes its certificate into the seed.
        .target(name: "GlovDaemonAPI", dependencies: [
            "GlovCore",
            .product(name: "X509", package: "swift-certificates"),
            .product(name: "Crypto", package: "swift-crypto"),
        ]),
        // The egress proxy terminates TLS for hosts a granted secret is bound to (NIOSSL), and nowhere else.
        .target(name: "GlovDaemon", dependencies: [
            "GlovCore", "GlovDaemonAPI",
            .product(name: "NIOCore", package: "swift-nio"),
            .product(name: "NIOPosix", package: "swift-nio"),
            .product(name: "NIOSSL", package: "swift-nio-ssl"),
        ]),
        // Info.plist is linked into __TEXT,__info_plist: macOS shows its Local Network alert only to a standalone
        // executable that carries one with NSLocalNetworkUsageDescription, and a LaunchAgent is not allowed by default.
        .executableTarget(name: "glovd", dependencies: [
            "GlovCore", "GlovDaemon", "GlovDaemonAPI",
            .product(name: "ArgumentParser", package: "swift-argument-parser"),
        ], exclude: ["Info.plist"], linkerSettings: [
            .unsafeFlags(["-Xlinker", "-sectcreate", "-Xlinker", "__TEXT", "-Xlinker", "__info_plist", "-Xlinker",
                          Context.packageDirectory + "/Sources/glovd/Info.plist"]),
        ]),
        // `glov guide` prints the agent skill. Guide/ holds symlinks to skills/glov/, compiled into the binary
        // (.embedInCode), so the installed `glov`, copied on its own, carries the current text and cannot drift from it.
        .executableTarget(name: "glov", dependencies: [
            "GlovCore", "GlovDaemonAPI", "GlovViewer",
            .product(name: "ArgumentParser", package: "swift-argument-parser"),
            .product(name: "MCP", package: "swift-sdk"),
            .product(name: "Logging", package: "swift-log"),
        ], resources: [
            .embedInCode("Guide/SKILL.md"), .embedInCode("Guide/troubleshooting.md"),
        ]),
        // AppKit window behind `glov view`; a plain client of glovd, so closing it never affects the VM.
        // The icon and its OS badges are compiled into the binary (.embedInCode), so a copied or symlinked `glov` still has them.
        .target(name: "GlovViewer", dependencies: ["GlovCore", "GlovDaemonAPI"], resources: [
            .embedInCode("Icons/icon.png"), .embedInCode("Icons/badge-windows.svg"), .embedInCode("Icons/badge-linux.svg"),
        ]),
        .testTarget(name: "GlovViewerTests", dependencies: ["GlovViewer"]),
        .testTarget(name: "GlovCoreTests", dependencies: ["GlovCore"]),
        .testTarget(name: "GlovDaemonTests", dependencies: [
            "GlovCore", "GlovDaemonAPI", "GlovDaemon", "glovd", "glov",
            .product(name: "NIOCore", package: "swift-nio"),
            .product(name: "NIOPosix", package: "swift-nio"),
            .product(name: "NIOSSL", package: "swift-nio-ssl"),
        ]),
    ]
)

for target in package.targets {
    target.swiftSettings = (target.swiftSettings ?? []) + [.treatAllWarnings(as: .error)]
}
