<img src="assets/image.png" alt="lexify"/>

CLI tool for English word lookup: definition, synonyms, etymology, and optional translation into other languages. No API keys required.


## Installation

Requires Go 1.25+.

```sh
go install github.com/bk-bf/lexify@latest
```

The binary lands in `$GOPATH/bin` (typically `~/go/bin`) — ensure that is on your `$PATH`.

<details>
<summary>Build from source</summary>

```sh
git clone https://github.com/bk-bf/lexify.git
cd lexify
go build -o lexify .
```

</details>



## Usage

```
lexify <word>
lexify <word> <lang> [lang ...]
```

| Flag             | Description                                                              |
| ---------------- | ------------------------------------------------------------------------ |
| `-i <lang> [lang …]` | Install offline pack(s) for the given language(s)                   |
| `--kaikki`       | Pack source: kaikki.org JSONL (~200–500 MB, ~2 min) — default            |
| `--wiki`         | Pack source: en.wiktionary.org XML dump (~1.2 GB, ~10 min)               |
| `--force`        | Reinstall even if pack is already up to date                             |
| `-o`             | Force live API (skip installed pack)                                     |
| `-d`             | Show per-fetch debug timing                                              |
| `--no-gtx-ety`   | Skip GTX etymology fallback (leave blank when pack/wiki miss)            |

Pass any BCP-47 language code as `<lang>`:
`fr` `ru` `de` `es` `it` `pt` `ja` `zh` `ko` `ar` `nl` `pl` `sv` `tr` `uk` `hi` …

Unrecognised language codes are dropped with an inline warning; output falls back to English.



## Examples

```sh
# English only
lexify serendipity

# With translation (definition, synonyms, etymology rendered in French)
lexify serendipity fr

# Multiple target languages
lexify serendipity fr ru

# Non-English source word — detected automatically, translated to EN, then looked up
lexify Schadenfreude en

# Install multiple packs at once
lexify -i en de fr

# Force live API even when a pack is installed
lexify serendipity -o
```



## Offline packs

Packs are installed per-language into `~/.local/share/lexify/` as a binary-search index (`<lang>.idx` + `<lang>.dat`).

```sh
lexify -i en           # install English pack (~2 min from kaikki.org)
lexify -i en --wiki    # same, from Wiktionary XML dump (~10 min)
lexify -i de           # install German pack
lexify -i de fr es     # install multiple packs at once
```

By default the tool uses the installed pack automatically when one is present. Pass `-o` to force live API calls instead.

If the word is not found in the pack the tool falls back to the APIs and marks this in output with `⚠ pack miss → api`.

**Native-edition packs** — for `de es fr it ja ko nl pl pt ru tr zh`, kaikki.org provides a native-language wiktionary edition. These packs contain definitions and etymologies already in the target language, so no translation round-trip is needed for those sections.

When a target-language pack is installed and is *not* a native edition, synonym lookups are served from the pack; definitions and etymology are fetched from the target-language Wiktionary API.



## Data sources

| Section                                  | Source                                               |
| ---------------------------------------- | ---------------------------------------------------- |
| Definition (EN)                          | Offline pack — or — dictionaryapi.dev                |
| Synonyms (EN)                            | Offline pack — or — dictionaryapi.dev (inline)       |
| Etymology (EN)                           | Offline pack — or — en.wiktionary.org `action=query` |
| Definitions + etymology (target lang)    | Native-edition pack — or — target-lang Wiktionary API |
| Synonyms (target lang)                   | Installed pack — or — target-lang Wiktionary API     |
| Translation (word)                       | Google Translate `gtx` endpoint; MyMemory fallback   |
| Translation (definition, etymology text) | Google Translate `gtx` endpoint                      |

All packs are built from [kaikki.org](https://kaikki.org) or the [Wiktionary XML dump](https://dumps.wikimedia.org/enwiktionary/).



## Implementation notes

**Concurrency** — Phase 1 fires goroutines for EN resolution (pack or API) and all word translations simultaneously. Phase 2 fans out immediately on Phase 1 results: per-language pack lookups, Wiktionary fetches, and text translations run without a second barrier. Use `-d` to see per-task timing split by phase.

**Wiktionary parsing** — Uses a single `action=query` API call to fetch full page wikitext, then extracts the relevant section client-side. Wikitext templates (`{{m}}`, `{{der}}`, `{{bor}}`, `{{suffix}}`, `{{w}}`, …) are resolved before display.

**Multilingual synonyms** — Section headings matched with a single regex covering `Synonyms`, `Synonymes`, `Synonyme`, `Синонимы`, `Sinónimos`, `同義語`, `동의어`, `مرادف`, and others.

**Unicode** — String widths and substring operations use rune slices throughout, so Cyrillic, CJK, Arabic, and other scripts wrap correctly.

**Non-EN source words** — If a word returns no definition, the tool translates it to English via GTX and retries the lookup in English (pack or API). The header shows `<original> → <translated>` when this path is taken.


