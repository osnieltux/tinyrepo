# tinyrepo — public website

Static landing page for the project, in English (`/`) and Spanish (`/es/`).

```
docs/
  index.html        English
  es/index.html     Spanish
  assets/tui.css    the whole design, ~4 KB
```

Named `docs/` because that is the one subdirectory GitHub Pages will serve from
a branch; there is nothing generated here and nothing to build.

Serve it locally with anything:

```sh
python3 -m http.server -d docs 8000
# or, with tinyrepo itself in front of a copy of these files
```

Total weight: two HTML files and one stylesheet. No JavaScript, no webfont, no
build step, no request beyond the page itself. It renders in a text browser.

## Design

A TUI look built from what a terminal already gives you: the system monospace
stack, box-drawing rules on the headings, an amber-phosphor accent, a blinking
block cursor (disabled under `prefers-reduced-motion`), and a light palette for
`prefers-color-scheme: light`. One column capped at 74ch, so it reflows on a
phone without a single media query for layout; anything that cannot shrink —
tables, command blocks — scrolls inside its own box.

## Framework

There is deliberately none. Two pages of static text do not need a runtime, and
"lightweight" and "framework" pull against each other here: the smallest CSS
framework is larger than this entire site.

If the site grows past ~5 pages, or the two languages start drifting apart,
move to **[Astro](https://astro.build/)**: it ships zero JavaScript by default,
has file-based i18n routing (`src/pages/en/`, `src/pages/es/`) with a
`i18n` config for the default locale, and outputs plain static files — so the
result stays exactly as light as what is here, while the content stops being
duplicated by hand. Port `assets/tui.css` across unchanged.

Things worth *not* reaching for:
- **Tailwind** — the utility classes would replace a 4 KB stylesheet with a
  build step and a purge config, for a design that is one column of monospace.
- **`terminal.css` / `tuicss`** — real TUI frameworks, but they impose their own
  palette and widget set, and cost more than the CSS they would replace.
- Any SPA framework — this page has no state.

## Deploying

The directory is already the document root, so there is no build command to set
anywhere.

**GitHub Pages**: Settings → Pages → Source *Deploy from a branch*, branch
`main`, folder `/docs`. The site appears at `https://<user>.github.io/tinyrepo/`
within a minute or two.

For a custom domain, add a `CNAME` file here holding the bare domain, then point
DNS at GitHub: four `A` records on the apex (185.199.108.153, .109.153,
.110.153, .111.153) or a `CNAME` to `<user>.github.io` on a `www` subdomain.
Pages issues the certificate itself once the DNS resolves.

**Anything else**: Netlify, Cloudflare Pages or `rsync` to a static host all
work the same way; point them at `docs/` and set no build command.
