<img src="assets/image.png" alt="lexify"/>

Terminal word lookup: definition · synonyms · etymology · translation — all in parallel, output in the target language. No API keys. No external dependencies. Pure Go stdlib.



## Installation

Requires Go 1.21+.

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

Pass any BCP-47 language code as `<lang>`. Almost all languages are supported:
`fr` `ru` `de` `es` `it` `pt` `ja` `zh` `ko` `ar` `nl` `pl` `sv` `tr` `uk` `hi` …

An unrecognised language code is silently dropped and a warning is shown inline — output always falls back to English.



## Examples

**English only** — definition, synonyms, etymology

```
> lexify serendipity

  serendipity  /ˌsɛ.ɹən.ˈdɪ.pɪ.ti/  [en]
  ──────────────────────────────────────────────────────────────────

   DEFINITION (EN)
  noun
  1. A combination of events which have come together by
  chance to make a surprisingly good or wonderful outcome.
  2. An unsought, unintended, and/or unexpected, but
  fortunate, discovery and/or learning experience that happens
  by accident.


   SYNONYMS (EN)
  chance · luck


   ETYMOLOGY (EN)
  From Serendip + -ity. based on the Persian story of The
  Three Princes of Serendip, who (Walpole wrote to a friend)
  were "always making discoveries, by accidents and sagacity,
  of things which they were not in quest of".

  ──────────────────────────────────────────────────────────────────
```

**With translation** — everything above, re-rendered in the target language

```
> lexify serendipity fr
```

**Multiple target languages** — parallel output for each

```
> lexify serendipity fr ru
```

**Non-English source** — language detected automatically

```
> lexify Schadenfreude en
```



## Output

| Section             | Source                                          |
| ------------------- | ----------------------------------------------- |
| **DEFINITION**      | dictionaryapi.dev                               |
| **SYNONYMS (EN)**   | datamuse.com                                    |
| **SYNONYMS (lang)** | Target-language Wiktionary                      |
| **ETYMOLOGY**       | en.wiktionary.org — `action=parse` wikitext API |
| **TRANSLATIONS**    | Google Translate `gtx` · MyMemory fallback      |

When a target language is provided, the definition and etymology are also translated so the full output is readable in that language.



## Implementation

**Concurrency** — Phase 1 fires goroutines for definition, synonyms, etymology, and all word translations simultaneously. Phase 2 fans out immediately on Phase 1 results: per-language content translation and target-language synonym fetches run without a second barrier.

**Wiktionary** — Uses the stable `action=parse` two-step API (section index → wikitext by section number) rather than HTML scraping. Wikitext templates (`{{m}}`, `{{der}}`, `{{bor}}`, `{{suffix}}`, `{{w}}`, …) are resolved via `resolveTemplate`.

**Multilingual synonyms** — Section headings matched with a single regex covering `Synonyms`, `Synonymes`, `Synonyme`, `Синонимы`, `Sinónimos`, `同義語`, `동의어`, `مرادف`, and more.

**Unicode** — All string widths and substring operations use rune slices, not byte lengths, so Cyrillic, CJK, Arabic, and other scripts wrap and filter correctly.


