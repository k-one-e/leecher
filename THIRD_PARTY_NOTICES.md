# Third-party components

The MIT license at the repository root covers Leecher's original work, not
third-party components or downloaded content. Preserve applicable upstream
copyright notices, licenses and attribution when redistributing.

## Go dependencies

Versions are recorded in `go.mod` and `go.sum`. License and notice files for
dependencies compiled into the supported platform builds are reproduced in
`third_party/licenses/`; `third_party/modules.json` maps modules and versions
to these files. Regenerate and review this inventory when dependencies change.
It is a record of the current dependency sources, not a legal certification.
Run `python3 scripts/update-notices.py` with the selected Go toolchain to collect
notices for all supported targets. The Go standard-library notices are in
`third_party/go/`. Source URLs in the inventory identify the exact upstream
module archives; those dependency files have not been modified by Leecher.

Notably, the BitTorrent implementation
[anacrolix/torrent](https://github.com/anacrolix/torrent) uses MPL-2.0.
Its covered source files remain under MPL-2.0. Distributing an executable that
includes those files entails MPL source-availability obligations for the
covered code, including any modifications; Leecher's MIT license does not
replace those obligations. The inventory identifies the upstream versions
available through Go modules. Distributors must ensure recipients can obtain
the corresponding covered source and applicable notices.

## Fonts and icons

- The bundled Inter fonts are copyright The Inter Project Authors and are
  licensed under SIL Open Font License 1.1. The full notice is in
  `static/fonts/OFL.txt`; the fonts are not relicensed under MIT.
- The interface uses inline SVG icon geometry, including Lucide-style icons.
  The Lucide ISC license and the upstream Feather MIT notice are included in
  `third_party/icons/`. These notices do not imply endorsement.

## Separately installed programs and container components

Leecher can install or invoke tools that are not relicensed by this project:

- [yt-dlp](https://github.com/yt-dlp/yt-dlp) has its own licensing and bundled
  dependencies; consult its license and the notices for the exact executable.
- [FFmpeg](https://ffmpeg.org/legal.html) licensing depends on its configuration
  and enabled components (including LGPL/GPL and potentially non-redistributable
  combinations). Review the exact binary and corresponding-source requirements.
  Codec patents may require a separate assessment.
- [Deno](https://github.com/denoland/deno) has its own license and third-party
  notices.
- [Transmission](https://transmissionbt.com/) is a separately operated torrent
  client with its own licenses.
- Docker base images and operating-system packages retain their upstream
  licenses, notices and corresponding-source requirements.

Do not assume the notices for Leecher's Go executable cover those external
programs or container layers. If you redistribute an image or bundle, obtain and
ship the required notices and fulfill applicable source-offer obligations for
that exact distribution.
