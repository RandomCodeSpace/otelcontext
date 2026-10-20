# Geist font dependency

The embedded UI uses the Geist and Geist Mono variable fonts from the `geist`
npm package 1.8.1 (Vercel). Both are licensed under the SIL Open Font License
1.1; the full license ships beside the fonts in
`internal/ui/static/geist.woff2.LEGAL.txt`. The fonts are embedded in the Go
binary and served from `/static/`; no font CDN is contacted at runtime.

## Source and checksums

Retrieved on 2026-10-20 from the
[versioned npm archive](https://registry.npmjs.org/geist/-/geist-1.8.1.tgz).

Registry archive integrity:

```text
sha512-oSBhuzTvwZqGAZVC4swTRo61G/JHojtEVNR3DCToU+4nUBkG48ndKNvj4Ma7zZ+OlJkYDLT66HS+hrFszkovTg==
```

| File | Source path | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| npm archive | | 4995543 | `856021a8b20f88fc57b4a3e84a29b41ae4d57a1709122598dcde923178b05a2d` |
| `geist.woff2` | `package/dist/fonts/geist-sans/Geist-Variable.woff2` | 69492 | `9ce2d97863cab59a0c7522534112d6c42a4e25c0e6b2ab37766761b0f82cfbd6` |
| `geist-mono.woff2` | `package/dist/fonts/geist-mono/GeistMono-Variable.woff2` | 71236 | `119f3e159fc15dd77311e929d3a657a193b57a3e625234666bc43c6e2b1127f7` |
| `geist.woff2.LEGAL.txt` | `package/LICENSE.txt` | | `930853ee1daa68554d9e35c8a9175affb74f699fad9a5da6ee5ebe76379d9137` |

The font files are copied unmodified.
