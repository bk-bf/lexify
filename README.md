<img src="assets/image.png" alt="lexify"/>

Terminal tool that does what a dictionary, thesaurus, and translator do — all at once, in parallel, with output in the target language. No API keys. No external dependencies. Pure Go stdlib.


## Usage

```
lexify <word>
lexify <word> <lang>
lexify <word> <lang> <lang> ...
```

## Examples

```
lexify serendipity
```
English definition, synonyms, and etymology.

```
lexify serendipity fr
```
Everything above, plus translation to French — definition, etymology, and synonyms rendered in French.

```
lexify serendipity fr ru
```
Parallel translations to both French and Russian.

```
lexify Schadenfreude en
```
Non-English source word, translated into English.

Pass any BCP-47 code: `fr` `ru` `de` `es` `it` `pt` `ja` `zh` `ko` `ar` `nl` `pl` `sv` `tr` `uk` `hi` …

---

## Output

Each lookup produces a formatted block with:

| Section             | Source                                              |
| ------------------- | --------------------------------------------------- |
| **DEFINITION**      | dictionaryapi.dev (English source words)            |
| **SYNONYMS (EN)**   | datamuse.com                                        |
| **SYNONYMS (lang)** | Target-language Wiktionary (one per requested lang) |
| **ETYMOLOGY**       | en.wiktionary.org — `action=parse` wikitext API     |
| **TRANSLATIONS**    | Google Translate `gtx` · MyMemory fallback          |

When a target language is given, definition and etymology are also translated into that language via Google's unofficial `gtx` endpoint (MyMemory as fallback), so the entire output is readable in the target language.

---

## Implementation

- **Language**: Go 1.21+, zero external dependencies (`stdlib` only)
- **Concurrency**: Phase 1 launches goroutines for definition, synonyms, etymology, and all translations simultaneously. Phase 2 fans out per-result goroutines for content translation and target-language synonyms the moment Phase 1 results arrive.
- **Wiktionary**: Uses the stable `action=parse` two-step API (sections index → wikitext by section number) instead of scraping HTML. Wikitext templates (`{{m}}`, `{{der}}`, `{{bor}}`, `{{suffix}}`, `{{w}}`) are resolved by `resolveTemplate()`.
- **Synonym headings**: Matched across languages with a multilingual regex covering `Synonyms`, `Synonymes`, `Synonyme`, `Синонимы`, `Sinónimos`, and more.
- **Unicode**: All string length and case operations use rune slices, not byte lengths, so Cyrillic, CJK, Arabic etc. wrap and filter correctly.

## Build

```sh
cd ~/Documents/Projects/special_projects/lexify
go build -o lexify .
```


