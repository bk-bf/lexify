# Noctalia Lexify Plugin — Implementation Guide

Full redesign of the translator plugin into a local-first word lookup plugin
(`>define`) backed by the lexify binary.

---

## Part 1 — Fork the Translator Plugin

### Step 1 · Copy the plugin directory

Duplicate `~/.config/noctalia/plugins/translator/` as a new directory named
`lexify` (or `define`, `wordlookup` — whatever the command prefix will be) in
the same `plugins/` tree.

Files to carry over as starting points (all will be modified):
- `manifest.json`
- `LauncherProvider.qml`
- `Main.qml`
- `Settings.qml`
- `TranslationPreview.qml`
- `translatorUtils.js`
- `i18n/en.json` (start with English only, add more later)

Delete `preview.png`; replace it once the plugin has a working UI.

---

### Step 2 · Update `manifest.json`

Change the following fields:
- `id` → new unique identifier (e.g. `lexify`)
- `name` → display name (e.g. `Word Lookup`)
- `version` → `0.1.0`
- `author` → your name
- `repository` → your repo URL or omit
- `description` → something like "Dictionary, synonyms and etymology via lexify"
- `metadata.commandPrefix` → `define` (so the launcher command becomes `>define`)
- `metadata.defaultSettings` → strip out `backend` / `realTime`; replace with
  `defaultLanguage: "en"` and `showPreview: true`
- Remove `entryPoints.settings` if you want to stub settings out initially

---

### Step 3 · Register the plugin in `plugins.json`

Add an entry for the new plugin `id` under `states` with `enabled: true`. If
you're hosting locally and not via a Noctalia plugin source, check whether the
Noctalia plugin loader supports local-only plugins without a `sourceUrl` — if
not, you may need to leave `sourceUrl` pointing at a placeholder or your own repo.

Optionally disable the original translator entry (`"enabled": false`) so the
two commands don't coexist confusingly.

---

### Step 4 · Understand the contract each file must satisfy

Before touching anything, map out what each file is responsible for:

| File                     | Role                                                                                                                                                                                                          |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Main.qml`               | Exposes the IPC `toggle` handler; calls `pluginApi.withCurrentScreen` to open/close the launcher with a prefilled search string                                                                               |
| `LauncherProvider.qml`   | The brain — `getResults(searchText)` drives all launcher results; `handleCommand(searchText)` tells Noctalia this plugin owns the current query; `translateText` (rename to `lookupWord`) does the async work |
| `TranslationPreview.qml` | Renders whatever is in `currentItem` in the preview panel — currently a single scrollable text block; will need a multi-section redesign                                                                      |
| `Settings.qml`           | Saves user settings via `pluginApi.saveSettings()`; currently contains backend selector and API key input — both will be removed                                                                              |
| `translatorUtils.js`     | Language code lookup table; reuse as-is, it's already complete                                                                                                                                                |
| `i18n/*.json`            | Localisation strings keyed with `pluginApi.tr(...)`                                                                                                                                                           |

---

## Part 2 — Option C: Full Redesign with Lexify Backend

### Phase A · Add JSON output mode to lexify

This is the prerequisite. The QML side cannot parse ANSI escape codes or
formatted terminal output, so lexify must gain a structured output mode.

**Step A1** — Design the JSON schema lexify will emit under `--json`.

Decide upfront what fields the QML side needs:
- The queried word and the target language code
- Array of definitions, each with part-of-speech and one or more meaning strings
  plus optional example sentences
- Array of synonyms (flat list of strings, from the synonym section)
- Etymology string (if available)
- Translation string (the single translated-word result, not sentence translation)
- An `error` field for when the word isn't found

**Step A2** — Add a `--json` flag to `main.go`.

When the flag is present, suppress all ANSI output and instead marshal the
collected results into the schema from A1, then print it to stdout. The normal
display path stays unchanged — `--json` is a parallel codepath layered on top
of the same fetch logic.

**Step A3** — Handle partial results.

Lexify fetches from multiple APIs concurrently; some may fail. Under `--json`,
emit whatever was successfully retrieved rather than silently omitting it.
Include a `sources` field listing which APIs returned results so the UI can show
"etymology unavailable" rather than a blank section.

**Step A4** — Build and test the JSON output manually.

Run `lexify serendipity --json`, `lexify serendipity ru --json`,
`lexify sérénité fr --json`. Confirm the output is valid JSON with populated
fields before touching QML.

---

### Phase B · Rework the launcher command logic

**Step B1** — Change the command prefix.

Replace all `>translate` checks and string literals with `>define`. Update
`handleCommand`, `getResults`, `commands()`, `Main.qml`'s IPC toggle, and the
i18n strings.

**Step B2** — Redesign the query syntax.

Current flow: `>translate [lang] [sentence]`
New flow: `>define [word] [lang?]`

The language is now optional and secondary — the primary thing is the word.
A bare `>define serendipity` should default to English. `>define serendipity ru`
looks up the word and returns content in/for Russian.

Update the `getResults` argument parsing accordingly.

**Step B3** — Keep the language picker as the first-level default.

When the user types just `>define`, show the language list exactly as the
translator does, so they can set a language before typing the word. This is
optional — you may prefer jumping straight to a text prompt — but preserves
the existing UX pattern users already know.

**Step B4** — Replace the `translateText` function with a `lookupWord` function.

Instead of `XMLHttpRequest` to an HTTP API, this function will invoke the lexify
binary as a subprocess and capture its stdout. The callback signature stays the
same: `callback(result, error)`, but `result` is now a parsed JSON object rather
than a plain string.

Check the Quickshell documentation for the correct component to run a subprocess
asynchronously from QML — the translator already uses `Quickshell.execDetached`
for clipboard writes, but that discards output. You need the variant that
captures stdout so you can feed it to the callback.

---

### Phase C · Redesign the result list

**Step C1** — Map the JSON output to launcher result items.

The lexify JSON has multiple sections (definitions, synonyms, etymology,
translation). Each section that returned data becomes one result item in the
launcher list. The item's `name` field shows the section heading and a short
preview; `description` shows a truncated first entry. All items share the same
`onActivate` — copy the translation word to clipboard and close.

**Step C2** — Add a "no results" item.

If lexify exits with an error or the JSON `error` field is set, show a single
item: "Word not found" with the queried word as description.

**Step C3** — Handle the still-loading state.

While the subprocess is running, show a single "Looking up…" placeholder item,
same pattern as the current "Translating…" item.

---

### Phase D · Redesign the preview panel

The current `TranslationPreview.qml` is a single scrollable text block. For
lexify output it needs multiple named sections.

**Step D1** — Plan the layout.

Sections to render (skip any that are empty):
1. **Translation** — prominent, large text, right at the top
2. **Definitions** — grouped by part of speech; each POS is a sub-heading
   followed by a numbered/bulleted list; example sentences in a lighter style
3. **Synonyms** — comma-separated or pill-style list
4. **Etymology** — single paragraph, italicised or dim

**Step D2** — Decide on component strategy.

Option 1: one big `Text` element with rich text (HTML subset that Qt supports)
— fast to implement, limited styling control.
Option 2: a `Repeater` over the JSON sections with per-section delegates —
more work but fully styled with Noctalia's design tokens (`Color.mOnSurface`,
`Style.fontSizeM`, etc.).

Pick option 1 first to prove the data flow, then upgrade to option 2.

**Step D3** — Wire `currentItem` to the preview.

`LauncherProvider.qml` must expose the full parsed JSON object (not just the
display string) so `TranslationPreview.qml` can access all sections regardless
of which result item is selected in the launcher list.

---

### Phase E · Rework Settings

**Step E1** — Remove backend and API key settings.

The lexify binary is the only backend; no network credentials are needed.

**Step E2** — Add a default language setting.

A combo box or text input for the ISO 639-1 code that pre-fills when no language
is specified in the query (defaults to `en`).

**Step E3** — Keep the "show preview" toggle.

It's already there; leave it.

**Step E4** — Optional: add a lexify binary path override.

For cases where `lexify` is not in PATH, allow the user to specify an absolute
path. Not critical if the binary is always at `~/.local/bin/lexify`.

---

### Phase F · Localisation

**Step F1** — Audit all `pluginApi.tr(...)` call sites in the new plugin.

Replace translator-specific string keys (`messages.translation`,
`settings.backend-label`, etc.) with new keys meaningful to word lookup.

**Step F2** — Update `i18n/en.json` with the new key set.

**Step F3** — Delete the other i18n locale files for now.

Keeping stale translated strings that no longer match the key schema will cause
silent fallback to the raw key string. Start clean with English only and re-add
locales intentionally.

---

### Phase G · Testing checklist

Work through each scenario before calling it done:

- `>define` with no word — shows language picker or prompt
- `>define serendipity` — returns English results with all four sections
- `>define serendipity ru` — returns Russian-facing results
- `>define слово ru` — Cyrillic input word, Russian output (exercises the rune-
  safe logic in lexify)
- `>define` on a word that doesn't exist — "not found" item, no crash
- Network offline — subprocess exits with error, plugin shows error item
- Preview panel with all four sections populated
- Preview panel with etymology missing (not all words have it)
- Real-time mode — `onActivate` copies translation to clipboard
- Settings: changing default language persists and affects next lookup

---

### Implementation order summary

```
A1 → A2 → A3 → A4   (lexify --json, do this entirely in Go before touching QML)
B1 → B2 → B4         (rename command, parse new query format, subprocess call)
C1 → C2 → C3         (launcher result items)
D1 → D2 → D3         (preview panel)
B3                    (language picker — polish step, not blocking)
E1 → E2 → E3         (settings cleanup)
G                     (testing)
F1 → F2 → F3         (i18n — last, since strings change throughout)
```

---

## Notes

- The `translatorUtils.js` language table is complete and compatible — reuse it
  unchanged.
- The `--json` lexify flag must keep the normal terminal display path working;
  `lexify` called without `--json` should behave exactly as today.
- Do not touch `~/.config/noctalia/plugins/translator/` once the fork is live —
  keep the original plugin intact as a reference.
- The fork lives under `~/.config/noctalia/plugins/` which is in the public repo
  but excluded via `.git/info/exclude`. Keep it that way until the plugin is
  stable and you decide whether to publish it.
