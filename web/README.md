# tinyrepo — public website

Static landing page for the project, in English (`/`) and Spanish (`/es/`).

```
web/
  index.html        English
  es/index.html     Spanish
  assets/tui.css    the whole design, ~4 KB
```

Serve it with anything:

```sh
python3 -m http.server -d web 8000
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

The directory is already the document root. GitHub Pages, Netlify, Cloudflare
Pages or `rsync` to a static host all work with no configuration; point them at
`web/` and set no build command.
