import Foundation
import Vision
import CoreGraphics
import ImageIO
import UniformTypeIdentifiers

struct OCRRecord: Encodable {
    let text: String
    let confidence: Float
    let boundingBox: BoundingBox?
    let sortKey: Double

    enum CodingKeys: String, CodingKey {
        case text, confidence, boundingBox
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(text, forKey: .text)
        try container.encode(confidence, forKey: .confidence)
        if let boundingBox {
            try container.encode(boundingBox, forKey: .boundingBox)
        }
    }
}

struct BoundingBox: Encodable {
    let x: Double
    let y: Double
    let width: Double
    let height: Double
}

struct OCRResult: Encodable {
    let imagePath: String
    let recordCount: Int
    let records: [OCRRecord]
    let fullText: String
}

enum OutputFormat: String {
    case json
    case csv
}

struct CLI {
    let imagePath: String
    let format: OutputFormat
    let languages: [String]
    let recognitionLevel: VNRequestTextRecognitionLevel
    let detectLanguage: Bool
    let useLanguageCorrection: Bool
    let hideBoundingBox: Bool
}

enum CLIError: Error, LocalizedError {
    case usage(String)
    case invalidArgument(String)
    case cannotLoadImage(String)

    var errorDescription: String? {
        switch self {
        case .usage(let msg), .invalidArgument(let msg), .cannotLoadImage(let msg):
            return msg
        }
    }
}

func parseArgs() throws -> CLI {
    var args = Array(CommandLine.arguments.dropFirst())

    guard !args.isEmpty else {
        throw CLIError.usage("""
Usage:
  vision-ocr <image-path> [json|csv] [--lang en-US,zh-Hans] [--fast] [--no-detect-language] [--no-language-correction] [--no-bbox]

Examples:
  vision-ocr ./sample.png json
  vision-ocr ./scan.jpg csv --lang en-US,zh-Hans
  vision-ocr ./note.png json --fast --no-detect-language
""")
    }

    let imagePath = args.removeFirst()
    var format: OutputFormat = .json
    var languages: [String] = ["en-US"]
    var recognitionLevel: VNRequestTextRecognitionLevel = .accurate
    var detectLanguage = true
    var useLanguageCorrection = true
    var hideBoundingBox = false

    if let first = args.first, let parsed = OutputFormat(rawValue: first.lowercased()) {
        format = parsed
        args.removeFirst()
    }

    var index = 0
    while index < args.count {
        let arg = args[index]
        switch arg {
        case "--lang":
            guard index + 1 < args.count else {
                throw CLIError.invalidArgument("Missing value after --lang")
            }
            languages = args[index + 1]
                .split(separator: ",")
                .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
                .filter { !$0.isEmpty }
            if languages.isEmpty {
                throw CLIError.invalidArgument("--lang requires at least one language, e.g. en-US,zh-Hans")
            }
            index += 2

        case "--fast":
            recognitionLevel = .fast
            index += 1

        case "--no-detect-language":
            detectLanguage = false
            index += 1

        case "--no-language-correction":
            useLanguageCorrection = false
            index += 1

        case "--no-bbox":
            hideBoundingBox = true
            index += 1

        default:
            throw CLIError.invalidArgument("Unknown argument: \(arg)")
        }
    }

    return CLI(
        imagePath: imagePath,
        format: format,
        languages: languages,
        recognitionLevel: recognitionLevel,
        detectLanguage: detectLanguage,
        useLanguageCorrection: useLanguageCorrection,
        hideBoundingBox: hideBoundingBox
    )
}

func systemSupportsWebP() -> Bool {
    guard let webpUTI = UTType.webP.identifier as CFString? else {
        return false
    }
    guard let supported = CGImageSourceCopyTypeIdentifiers() as? [String] else {
        return false
    }
    return supported.contains(webpUTI as String)
}

func writePNG(_ image: CGImage, to url: URL) throws {
    guard let dest = CGImageDestinationCreateWithURL(
        url as CFURL,
        UTType.png.identifier as CFString,
        1,
        nil
    ) else {
        throw CLIError.cannotLoadImage("Failed to create PNG destination: \(url.path)")
    }
    CGImageDestinationAddImage(dest, image, nil)
    if !CGImageDestinationFinalize(dest) {
        throw CLIError.cannotLoadImage("Failed to write PNG: \(url.path)")
    }
}

func loadCGImage(from path: String) throws -> (image: CGImage, path: String) {
    let url = URL(fileURLWithPath: path)

    guard let source = CGImageSourceCreateWithURL(url as CFURL, nil) else {
        throw CLIError.cannotLoadImage("Failed to create image source: \(path)")
    }

    let sourceUTI = CGImageSourceGetType(source) as String?
    if let uti = sourceUTI {
        fputs("Detected image type: \(uti)\n", stderr)
    }

    guard let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else {
        throw CLIError.cannotLoadImage("Failed to decode image: \(path)")
    }

    let friendly: Set<String> = [UTType.png.identifier, UTType.jpeg.identifier]
    if let uti = sourceUTI, friendly.contains(uti) {
        return (image, path)
    }

    let base = (path as NSString).lastPathComponent
    let outURL = URL(fileURLWithPath: NSTemporaryDirectory())
        .appendingPathComponent("\(base).converted.png")
    try writePNG(image, to: outURL)
    fputs("Converted to PNG: \(outURL.path)\n", stderr)

    guard let reSource = CGImageSourceCreateWithURL(outURL as CFURL, nil),
          let reImage = CGImageSourceCreateImageAtIndex(reSource, 0, nil) else {
        throw CLIError.cannotLoadImage("Failed to reload converted PNG: \(outURL.path)")
    }
    return (reImage, outURL.path)
}

struct Tile {
    let dy: Int
    let height: Int
}

func planTiles(imageHeight: Int, tileHeight: Int, overlap: Int) -> [Tile] {
    if imageHeight <= tileHeight {
        return [Tile(dy: 0, height: imageHeight)]
    }
    var tiles: [Tile] = []
    let stride = max(1, tileHeight - overlap)
    var dy = 0
    while dy < imageHeight {
        let h = min(tileHeight, imageHeight - dy)
        tiles.append(Tile(dy: dy, height: h))
        if dy + h >= imageHeight { break }
        dy += stride
    }
    return tiles
}

func performVisionRequest(on image: CGImage, cli: CLI) throws -> [VNRecognizedTextObservation] {
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = cli.recognitionLevel
    request.recognitionLanguages = cli.languages
    request.automaticallyDetectsLanguage = cli.detectLanguage
    request.usesLanguageCorrection = cli.useLanguageCorrection

    let handler = VNImageRequestHandler(cgImage: image, options: [:])
    try handler.perform([request])

    return request.results ?? []
}

func recognizeText(in image: CGImage, imagePath: String, cli: CLI) throws -> OCRResult {
    let tileHeight = 2000
    let overlap = 200
    let fullH = image.height
    let tiles = planTiles(imageHeight: fullH, tileHeight: tileHeight, overlap: overlap)
    if tiles.count > 1 {
        fputs("Tiling into \(tiles.count) slices (tile=\(tileHeight)px, overlap=\(overlap)px)\n", stderr)
    }

    var records: [OCRRecord] = []

    for (idx, tile) in tiles.enumerated() {
        let subImage: CGImage
        if tiles.count == 1 {
            subImage = image
        } else {
            let rect = CGRect(x: 0, y: tile.dy, width: image.width, height: tile.height)
            guard let cropped = image.cropping(to: rect) else {
                throw CLIError.cannotLoadImage("Failed to crop tile at dy=\(tile.dy)")
            }
            subImage = cropped
        }

        let observations = try performVisionRequest(on: subImage, cli: cli)

        let keepTop = idx == 0 ? tile.dy : tile.dy + overlap / 2
        let keepBottom = idx == tiles.count - 1 ? tile.dy + tile.height : tile.dy + tile.height - overlap / 2

        for observation in observations {
            guard let candidate = observation.topCandidates(1).first else { continue }
            let b = observation.boundingBox
            let th = Double(tile.height)

            let pixelBottomInFull = Double(tile.dy) + (1.0 - Double(b.origin.y)) * th
            let pixelTopInFull = Double(tile.dy) + (1.0 - Double(b.origin.y) - Double(b.size.height)) * th
            let pixelCenterInFull = Int((pixelBottomInFull + pixelTopInFull) / 2.0)

            if tiles.count > 1 && (pixelCenterInFull < keepTop || pixelCenterInFull >= keepBottom) {
                continue
            }

            let fullY = 1.0 - pixelBottomInFull / Double(fullH)
            let fullHeight = Double(b.size.height) * th / Double(fullH)
            let box = BoundingBox(
                x: Double(b.origin.x),
                y: fullY,
                width: Double(b.size.width),
                height: fullHeight
            )

            records.append(OCRRecord(
                text: candidate.string,
                confidence: candidate.confidence,
                boundingBox: cli.hideBoundingBox ? nil : box,
                sortKey: fullY + fullHeight
            ))
        }
    }

    records.sort { $0.sortKey > $1.sortKey }

    let fullText = records.map(\.text).joined(separator: "\n")

    return OCRResult(
        imagePath: imagePath,
        recordCount: records.count,
        records: records,
        fullText: fullText
    )
}

func escapeCSV(_ value: String) -> String {
    let escaped = value.replacingOccurrences(of: "\"", with: "\"\"")
    return "\"\(escaped)\""
}

func printJSON(_ result: OCRResult) throws {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
    let data = try encoder.encode(result)
    guard let text = String(data: data, encoding: .utf8) else {
        throw CLIError.invalidArgument("Failed to encode JSON as UTF-8")
    }
    print(text)
}

func printCSV(_ result: OCRResult) {
    let includeBox = result.records.contains { $0.boundingBox != nil }
    if includeBox {
        print("text,confidence,x,y,width,height")
    } else {
        print("text,confidence")
    }
    for record in result.records {
        var cols = [escapeCSV(record.text), String(record.confidence)]
        if includeBox, let b = record.boundingBox {
            cols.append(contentsOf: [
                String(b.x), String(b.y), String(b.width), String(b.height)
            ])
        }
        print(cols.joined(separator: ","))
    }
}

func main() {
    do {
        let cli = try parseArgs()
        let loaded = try loadCGImage(from: cli.imagePath)
        let result = try recognizeText(in: loaded.image, imagePath: loaded.path, cli: cli)

        switch cli.format {
        case .json:
            try printJSON(result)
        case .csv:
            printCSV(result)
        }
    } catch {
        fputs("Error: \(error.localizedDescription)\n", stderr)
        exit(1)
    }
}

main()
