# Lucide icon dependency

The embedded UI draws its icons from `lucide-static` 1.60.0. Lucide is licensed
under ISC; icons derived from Feather are additionally MIT-licensed. The full
notice is in `internal/ui/static/icons.svg.LEGAL.txt`.

`internal/ui/static/icons.svg` is a sprite of the icons the UI uses. Each
`<symbol id="i-NAME">` holds the unmodified inner markup of
`package/icons/NAME.svg`; comments and whitespace runs are collapsed. Pages
reference symbols as `/static/icons.svg#i-NAME` and style the stroke with the
`.ic` class from `tokens.css`. To add an icon, copy the inner markup of the
matching file from the same release into a new symbol.

## Source and checksums

Retrieved on 2026-10-20 from the
[versioned npm archive](https://registry.npmjs.org/lucide-static/-/lucide-static-1.60.0.tgz).

Registry archive integrity:

```text
sha512-Y8/IYjBYST40TuqzknGt1XJOcZp83UH5fLndgXlQCQ4lM97eZO3EQo6cIyDcBrprredIGDVxge8yslRyM4wD0Q==
```

| File | Bytes | SHA-256 |
| --- | ---: | --- |
| npm archive | 7005471 | `feb647afe973601d3eefa97539f23acfaefecb5f81e51fef9a03b4d6c4787d75` |
| `icons.svg.LEGAL.txt` (`package/LICENSE`) | | `b495047bd93a9b06913511076f504daba17d5bbeb3e0650f3bb53a4220329c57` |
