# Theming guide

The UI is themed entirely through CSS custom properties (variables). Every color,
shadow, and corner radius in the stylesheet reads a `var(--token)`; no rule hard-
codes a color. A theme is therefore just one block that assigns the full set of
tokens. This guide documents the contract and walks through adding a new theme,
using the classic Windows 3.1 **Hot Dog Stand** scheme as the worked example.

## How theming works

Two files, two small additions per theme:

1. `internal/web/assets/css/app.css`: a `:root[data-theme="<id>"]` block that
   defines every token (the **palette**).
2. `internal/web/assets/js/app.js`: one entry in the `THEMES` array (the
   **picker label + icon**).

That is the whole surface. The rest of the stylesheet, every page (grid, table,
reader, comic viewer, edit, imports), automatically uses the new palette.

### Selection and persistence

- The active theme is the `data-theme` attribute on `<html>`.
- With **no** `data-theme` set, the page follows the OS: light by default, or the
  `@media (prefers-color-scheme: dark)` block for a dark OS.
- The theme picker (the icon button at the top-right) sets `data-theme` and saves
  the theme **id** to `localStorage["theme"]`.
- A tiny inline script in each page's `<head>` re-applies the saved theme before
  first paint, so there is no flash of the wrong theme on load.

### Why the selectors are `:root`-scoped

The theme blocks are written `:root[data-theme="warm"]`, not `[data-theme="warm"]`.
An unscoped selector would re-theme **any** element carrying that attribute, not
just the document root. The theme picker's menu items must therefore NOT use a
`data-theme` attribute (they use `data-theme-id`), or each item would paint itself
in its own palette and become unreadable. Keep new selectors `:root`-scoped.

## The token contract

Every theme MUST define all of these. The defaults below are the built-in light
theme, shown for reference.

| Token | Role | Light value |
| --- | --- | --- |
| `--bg` | Page background | `#f7f7f8` |
| `--fg` | Primary text | `#1a1a1a` |
| `--muted` | Secondary text (authors, labels) | `#666` |
| `--faint` | Tertiary text (hints, empty states) | `#777` |
| `--panel` | Card / surface background | `#fff` |
| `--border` | Dividers, card outlines | `#e3e3e6` |
| `--cover-bg` | Image placeholder, hover fills | `#e9e9ee` |
| `--input-border` | Form-control outline | `#ccc` |
| `--btn-border` | Reader-bar button outline | `#ccc` |
| `--accent` | Primary action color (buttons, focus) | `#2a6` |
| `--accent-fg` | Text on `--accent` | `#fff` |
| `--banner-bg` | Info banner background | `#eef9f0` |
| `--banner-border` | Info banner border | `#cfe8d4` |
| `--banner-fg` | Info banner text | `#1c6b34` |
| `--link` | Reader link color | `#444` |
| `--bar` | Reader top bar background | `#fff` |
| `--shadow` | Card / popup elevation (a full `box-shadow` value) | `0 1px 3px rgba(0,0,0,.08)` |
| `--radius` | Default corner rounding (a length, e.g. `10px`) | `10px` |

Notes:

- `--accent` and `--accent-fg` must contrast with each other: `--accent` is the
  button fill, `--accent-fg` is the text on it.
- For a dark-ish theme, also set `color-scheme: dark;` in the block so native
  controls (scrollbars, the file picker) render dark too.
- `--shadow` and `--radius` are full values, not just numbers, so a theme can
  drop shadows entirely (`--shadow: none`) or go sharp-cornered (`--radius: 0`).

## The reader (epub)

The in-browser epub reader injects styles into the book's own iframe. It reads
the live `--bg`, `--fg`, and `--link` values off `<html>` and themes the book
page to match the active theme. So a new theme needs **no** reader-specific code:
register the palette and the book content follows. The comic viewer themes purely
through CSS and needs nothing either.

## Worked example: the Hot Dog Stand theme

The Windows 3.1 "Hot Dog Stand" scheme is bright red and yellow with black text,
maximally loud. It is a good stress test because it is high-contrast and uses a
warm, saturated background rather than a neutral one.

### Step 1: add the palette to `app.css`

Add this block alongside the other `:root[data-theme=...]` blocks:

```css
/* Hot Dog Stand: the infamous Windows 3.1 red+yellow scheme. */
:root[data-theme="hotdog"] {
  --bg:#ce0000; --fg:#000000; --muted:#3a0000; --faint:#5a1a00;
  --panel:#ffff00; --border:#000000; --cover-bg:#e6c200;
  --input-border:#000000; --accent:#000000; --accent-fg:#ffff00;
  --banner-bg:#ffff00; --banner-border:#000000; --banner-fg:#000000;
  --link:#000000; --btn-border:#000000; --bar:#ffff00;
  --shadow:0 2px 0 #000000; --radius:0;
}
```

Design reasoning for the tricky tokens:

- `--bg` is the loud red; `--panel` is the yellow that cards/bars sit on, with
  black text (`--fg`) on top. That is the canonical look.
- `--accent` is black with yellow text (`--accent-fg`), so primary buttons are
  black-on-yellow, readable against both the red page and yellow panels.
- `--muted` and `--faint` stay dark (not gray), because gray would vanish on red.
- `--radius: 0` plus a hard offset `--shadow` give it the flat, boxy retro feel;
  drop these if you prefer the modern rounded cards.

### Step 2: register it in the picker (`app.js`)

Add an entry to the `THEMES` array:

```js
var THEMES = [
  { id: "dark", label: "Dark", icon: "🌙" },
  { id: "light", label: "Light", icon: "☀️" },
  { id: "warm", label: "Warm", icon: "🕯️" },
  { id: "hotdog", label: "Hot Dog Stand", icon: "🌭" },
];
```

The `id` must exactly match the `:root[data-theme="hotdog"]` selector. `label` is
what the picker menu shows; `icon` is shown on the picker button when this theme
is active.

### Step 3: that's it

No template, route, or other CSS change is needed. Reload, open the picker, and
"Hot Dog Stand" appears with the others; selecting it themes every page and
persists across reloads.

## Checklist for a new theme

- [ ] Add a `:root[data-theme="<id>"]` block to `app.css` defining ALL tokens in
      the contract table.
- [ ] Keep the selector `:root`-scoped.
- [ ] Set `color-scheme: dark;` if the theme is dark-ish.
- [ ] Ensure `--accent` vs `--accent-fg` and `--fg` vs `--panel` / `--bg`
      contrast.
- [ ] Add a matching `{ id, label, icon }` entry to `THEMES` in `app.js` (same
      `id`).
- [ ] Reload and verify: grid, reader (epub + comic), edit form, and the imports
      page all read correctly, and the picker icon stays on the chosen theme.
