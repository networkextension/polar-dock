# vision-ocr — Apple Vision OCR CLI

Used by the expense module (`doc/laopo/design.md`, L2) to extract text from
receipt screenshots before LLM structuring. Apple Vision Framework runs
locally on the deploy box (macOS 13+, no API key, no rate limit).

The Go side `expense_ocr.go` shells out to this binary via `EXPENSE_OCR_BIN`
(default `/Users/local/.local/bin/vision-ocr`).

## Build

```bash
cd tools/vision-ocr
swift build -c release
# binary appears at .build/release/vision-ocr
```

## Install on the deploy box (.57)

```bash
# From a dev box on the same LAN:
scp .build/release/vision-ocr local@192.168.11.57:/Users/local/.local/bin/vision-ocr
ssh local@192.168.11.57 'chmod +x /Users/local/.local/bin/vision-ocr'
```

Then in `~/polar-dock.env` set `EXPENSE_OCR_BIN=/Users/local/.local/bin/vision-ocr`
(default already matches this path; only override if installing elsewhere).

## Usage

```
vision-ocr <image-path> [json|csv] [--lang en-US,zh-Hans] [--fast]
                       [--no-detect-language] [--no-language-correction]
                       [--no-bbox]
```

The Go caller invokes with `json --lang zh-Hans,en-US --no-bbox`, which
gives JSON like:

```json
{
  "imagePath": "/tmp/foo.png",
  "recordCount": 18,
  "records": [{"text": "...", "confidence": 0.99}, ...],
  "fullText": "...\n...\n..."
}
```

We use `fullText` directly as the LLM prompt input.

## Notes

- Auto-transcodes non-PNG/JPEG sources to PNG via CoreGraphics (HEIC,
  WebP, GIF all work).
- Tile-based for tall images (default tile=2000px, overlap=200px) —
  long Alipay / WeChat ledger screenshots remain accurate top-to-bottom.
- Outputs records sorted top-to-bottom for readable LLM input.

Original source: dropped by Kong on 2026-05-19. See `WORKLOG.md` for
the development history of the standalone package.
