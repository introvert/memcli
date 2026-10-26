// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Mem",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "Mem",
            path: "Sources/Mem",
            linkerSettings: [.linkedFramework("Carbon")]
        ),
    ]
)
