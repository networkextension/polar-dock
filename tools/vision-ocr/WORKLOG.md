# Work Log

## 2026-04-17

### Package layout
- Added `vision-ocr` executable product and `.executableTarget` in `Package.swift`.
- Set `platforms: [.macOS(.v13)]` to satisfy Vision / `automaticallyDetectsLanguage` / `withoutEscapingSlashes` availability.
- Moved `Sources/ocr/main.swift` to `Sources/vision-ocr/main.swift`; `ocr` stays as the library target.

### Image loading
- Added UTI probe via `CGImageSourceGetType`, logged to stderr as `Detected image type: <uti>`.
- Added `systemSupportsWebP()` helper that checks `CGImageSourceCopyTypeIdentifiers()` against `UTType.webP`.
- Auto-transcode: if the source UTI is not `public.png` / `public.jpeg`, decode then re-encode to PNG at `$TMPDIR/<name>.converted.png` with `CGImageDestination`, reload from disk, and use that for OCR. `OCRResult.imagePath` reflects the effective path. WebP verified end-to-end.

### Tall-image handling
- `planTiles(imageHeight:tileHeight:overlap:)` produces overlapping vertical slices (default tile = 2000px, overlap = 200px).
- OCR per tile via `performVisionRequest`, then remap each observation's normalized bbox from tile space to full-image space.
- Dedup across overlaps using an "ownership band" per tile: keep an observation only if its pixel center-y falls inside the band (edge tiles keep the outer half of their overlap).
- Records sorted top-to-bottom for readable output.

### Output shaping
- `OCRRecord.boundingBox` is now optional with a custom `Encodable` impl that omits the field when nil (no `null` in JSON).
- `--no-bbox` flag hides position info: JSON drops the `boundingBox` key; CSV header collapses to `text,confidence`.
- Switched `OCRRecord` / `OCRResult` from `Codable` to `Encodable` (no decoding path used).
- Cleaned the `request.results as? [...]` warning by using the SDK's already-typed `results` directly.
