// swift-tools-version: 6.3
// The swift-tools-version declares the minimum version of Swift required to build this package.

import PackageDescription

let package = Package(
    name: "ocr",
    platforms: [
        .macOS(.v13)
    ],
    products: [
        .library(
            name: "ocr",
            targets: ["ocr"]
        ),
        .executable(
            name: "vision-ocr",
            targets: ["vision-ocr"]
        ),
    ],
    targets: [
        .target(
            name: "ocr"
        ),
        .executableTarget(
            name: "vision-ocr",
            dependencies: ["ocr"]
        ),
        .testTarget(
            name: "ocrTests",
            dependencies: ["ocr"]
        ),
    ],
    swiftLanguageModes: [.v6]
)
