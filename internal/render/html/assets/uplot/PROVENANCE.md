# uPlot — vendored

The report's chart library, embedded with `go:embed` so the report makes no network
request (§5.8). Unmodified.

| | |
| --- | --- |
| Project | uPlot, by Leon Sorokin |
| Version | 1.6.32 |
| Licence | MIT — see [`LICENSE`](LICENSE), which ships beside the files and is reproduced in every report that embeds them |
| Source | npm registry tarball `https://registry.npmjs.org/uplot/-/uplot-1.6.32.tgz` |
| Tarball integrity | `sha512-KIMVnG68zvu5XXUbC4LQEPnhwOxBuLyW1AHtpm6IKTXImkbLgkMy+jabjLgSLMasNuGGzQm/ep3tOkyTxpiQIw==` (matches the registry's published value) |
| Files taken | `dist/uPlot.iife.min.js`, `dist/uPlot.min.css`, `LICENSE` |
| SHA-256 `uPlot.iife.min.js` | `19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f` |
| SHA-256 `uPlot.min.css` | `df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04` |

Checked before vendoring: no `eval` or `new Function`, no network calls, no inline
`style` attributes (it styles through the CSSOM, which a strict `style-src` permits),
and no `</` sequence that could end an inline `<script>` early. A test pins the
SHA-256 values above, so replacing the files is a reviewed change.

To upgrade: fetch the new tarball, check its integrity against the registry, repeat
the checks above, replace the three files and update this table and the test.
