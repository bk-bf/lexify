// lexify — word lookup: definition · synonyms · etymology · translation
// Usage: lexify <word> [lang ...]
// APIs: dictionaryapi.dev · datamuse.com · en.wiktionary.org · google gtx
// Deps: stdlib only
package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

// ── ANSI constants ────────────────────────────────────────────────────────────

const (
	R      = "\033[0m"
	Bold   = "\033[1m"
	Dim    = "\033[2;37m"
	CWord  = "\033[1;36m" // bold cyan   — word title
	CHead  = "\033[1;35m" // bold mauve  — section headers
	CPos   = "\033[33m"   // peach       — part of speech
	CSyn   = "\033[36m"   // cyan        — synonyms
	CTrans = "\033[1;34m" // bold blue   — translated word
	CEx    = "\033[37m"   // light grey  — examples / meta
	CErr = "\033[31m"       // red — errors
	CBar = "\033[38;5;208m" // orange — install progress bar
)

// winsize mirrors the kernel struct winsize used by TIOCGWINSZ.
type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }

// dividerWidth is computed once at startup from the real terminal column count
// via TIOCGWINSZ ioctl. Falls back to 76 when stdout is not a TTY.
var dividerWidth = func() int {
	var ws winsize
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(1), // stdout fd
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno == 0 && ws.Col > 0 {
		w := int(ws.Col) - 4 // 2-space left margin + 2-char right breathing room
		if w < 40 {
			w = 40
		}
		return w
	}
	return 76 // fallback: 80-col terminal
}()

// Nerd Font icons (nf-fa, reliable in SauceCodePro NF)
const (
	IDef   = "\uf02d "
	ISyn   = "\uf0ca "
	IEty   = "\uf017 "

	// minEtymRunes is the minimum rune length a wiki-sourced etymology string must
	// have to be shown. Shorter results are fragments/stubs; GTX~ will be tried instead.
	minEtymRunes = 40

	ITrans = "\uf0ac "
)

// ── HTTP helper ───────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 7 * time.Second}

func fetchJSON(rawURL string, target interface{}) error {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(target)
}

// ── Text helpers ──────────────────────────────────────────────────────────────

// runeLen returns the number of Unicode code points in s.
// Used for text width calculations so Cyrillic/CJK/etc. wrap correctly.
func runeLen(s string) int { return len([]rune(s)) }

// ansiRe matches ANSI SGR escape sequences (colours, bold, dim, reset, etc.).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripANSI removes all ANSI escape sequences, returning the bare visible text.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func wordWrap(text string, width int, indent string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	indentLen := runeLen(indent)
	var sb strings.Builder
	line := indent
	lineLen := indentLen
	for _, w := range words {
		wLen := runeLen(w)
		if lineLen == indentLen {
			line += w
			lineLen += wLen
		} else if lineLen+1+wLen <= width {
			line += " " + w
			lineLen += 1 + wLen
		} else {
			sb.WriteString(line + "\n")
			line = indent + w
			lineLen = indentLen + wLen
		}
	}
	if lineLen > indentLen {
		sb.WriteString(line)
	}
	return sb.String()
}

func divider() string {
	return "  " + CEx + strings.Repeat("─", dividerWidth) + R
}

func sectionHeader(icon, title string) string {
	return fmt.Sprintf("\n  %s%s%s%s\n", CHead, Bold, icon+title, R)
}

// mentionTemplates: structure is {{name|lang|word|gloss?}} — word is parts[2]
var mentionTemplates = map[string]bool{
	"m": true, "l": true, "m+": true, "mention": true, "link": true,
	"cog": true, "noncog": true, "ncog": true, // cognate — same shape as mention
}

// etymTemplates: structure is {{name|dest|src|word|gloss?}} — word is parts[3]
var etymTemplates = map[string]bool{
	"inh": true, "inh+": true,
	"bor": true, "bor+": true,
	"der": true, "der+": true,
	"inherited": true, "borrowed": true, "derived": true,
}

// combinerTemplates produce "A + -B" or "A- + B" display text.
var combinerTemplates = map[string]bool{
	"suffix": true, "prefix": true, "confix": true, "compound": true, "affix": true, "af": true,
}

// resolveTemplate attempts to extract a readable display string from a
// Wiktionary template. Falls back to empty string (template is silently dropped)
// for purely metadata templates.
func resolveTemplate(raw string) string {
	// raw is the content between {{ }}, e.g. "m|en|Serendip|variant of..."
	parts := strings.Split(raw, "|")
	if len(parts) == 0 {
		return ""
	}
	name := strings.ToLower(strings.TrimSpace(parts[0]))

	if mentionTemplates[name] || etymTemplates[name] {
		// mention/cog: word at parts[2]; etym (der/inh/bor…): word at parts[3]
		idx := 2
		if etymTemplates[name] {
			idx = 3
		}
		// Collect positional args and tr= transliteration
		word := ""
		tr := ""
		for i, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "tr=") {
				tr = strings.TrimPrefix(p, "tr=")
			} else if strings.HasPrefix(p, "t=") || strings.HasPrefix(p, "gloss=") || strings.Contains(p, "=") {
				continue
			} else if i+1 == idx { // i+1 because parts[1:] drops name
				word = p
			}
		}
		if word != "" {
			return word
		}
		if tr != "" {
			return tr
		}
		return ""
	}

	// {{doublet|lang|word1|word2|...}} — list all words
	if name == "doublet" {
		var words []string
		for _, p := range parts[2:] {
			p = strings.TrimSpace(p)
			if p != "" && !strings.Contains(p, "=") {
				words = append(words, p)
			}
		}
		return strings.Join(words, ", ")
	}

	if combinerTemplates[name] {
		// suffix|lang|base|suf  → base + -suf  (base may be empty → just -suf)
		// prefix|lang|pre|base  → pre- + base
		// compound|lang|a|b     → a + b
		var words []string
		for _, p := range parts[2:] {
			p = strings.TrimSpace(p)
			if p != "" && !strings.Contains(p, "=") {
				words = append(words, p)
			}
		}
		if name == "suffix" {
			switch len(words) {
			case 0:
				return ""
			case 1:
				return "-" + words[0] // empty base: just show -suffix
			default:
				return words[0] + " + -" + words[1]
			}
		}
		if name == "prefix" && len(words) >= 2 {
			return words[0] + "- + " + words[1]
		}
		return strings.Join(words, " + ")
	}

	// {{w|Page name}} or {{w|Page|display}} — Wikipedia links
	if name == "w" {
		if len(parts) >= 3 && strings.TrimSpace(parts[2]) != "" {
			return strings.TrimSpace(parts[2])
		}
		if len(parts) >= 2 {
			return strings.TrimSpace(parts[1])
		}
		return ""
	}

	// {{ux|lang|usage example}}, {{usex|lang|text}}, {{uxi|lang|text}} — usage example templates
	if name == "ux" || name == "usex" || name == "uxi" {
		if len(parts) >= 3 {
			return strings.TrimSpace(parts[2])
		}
		return ""
	}

	// Namespace transclusion templates (name contains ":") are always resolved
	// from a separate wiki page — their arguments are metadata/flags (e.g. "да"),
	// never inline content. Drop entirely.
	if strings.Contains(name, ":") {
		return ""
	}

	// Single-argument templates not otherwise handled: return the argument as-is.
	// Covers things like {{lang|word}}, {{smallcaps|word}}, etc.
	// Exception: drop values that are BCP-47 language codes (2–3 lowercase ASCII
	// letters) — templates like {{спец.|ru}}, {{п.|ru}} use a language code as
	// their sole argument and must not emit it as visible text.
	if len(parts) == 2 {
		val := strings.TrimSpace(parts[1])
		if !strings.Contains(val, "=") && !langCodeRe.MatchString(val) {
			return val
		}
	}

	return "" // drop metadata / formatting templates
}

// stripWikitext removes template markup and resolves link syntax to display text.
// {{m|en|word}}         → word   (term reference templates)
// {{suffix|en|a|b}}     → a + -b (combiner templates)
// {{other templates}}   → removed
// [[link|display]]      → display
// [[link]]              → link
// ”italic”, ”'bold”' markers removed
// danglingConnectorRe matches etymology text that ends with an incomplete fragment
// after template removal — a connector/preposition with nothing meaningful after it.
// These are left when a transclusion template resolved to empty.
var danglingConnectorRe = regexp.MustCompile(
	`(?i)(?:^|\.)\s*[\p{L} ]*(от|из|далее из|далее|von|aus|de|da|from|of|del|della|di|du|der|dem|den|des|af|van|fr\S*|fra|further from)\s*$`)

// wikitext cleanup regexes — compiled once at package level so they are not
// re-compiled on every stripWikitext call (cmdInstall calls it ~500k times).
var (
	wikiHTMLCommentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
	wikiRefBlockRe     = regexp.MustCompile(`(?s)<ref[^>]*>.*?</ref>`)
	wikiRefSelfRe      = regexp.MustCompile(`<ref[^>]*/>`)
	wikiHTMLTagRe      = regexp.MustCompile(`<[^>]+>`)
	wikiTmplRe         = regexp.MustCompile(`\{\{([^{}]*)\}\}`)
	wikiLinkDispRe     = regexp.MustCompile(`\[\[(?:[^\]|]+\|)([^\]]+)\]\]`)
	wikiLinkRe         = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	wikiListMarkerRe   = regexp.MustCompile(`(?m)^[#*:;]+\s*`)
	wikiDupDotRe       = regexp.MustCompile(`(\. ){2,}\.?`)
	wikiDupCommaRe     = regexp.MustCompile(`(, ){2,},?`)
	wikiEmptyParenRe   = regexp.MustCompile(`\(\s*\)`)
	wikiMultiSpaceRe   = regexp.MustCompile(` {2,}`)
	wikiMultiNewlineRe = regexp.MustCompile(`\n{3,}`)

	// synSplitRe splits synonym lines on comma/semicolon separators.
	synSplitRe = regexp.MustCompile(`[,;]+`)
	// synTemplateRe extracts synonym words from {{syn|lang|word1|…}} templates.
	synTemplateRe = regexp.MustCompile(`\{\{syn\|[^|{}]+\|([^{}]+)\}\}`)
)

func stripWikitext(text string) string {
	// Decode HTML entities (&nbsp; → " ", &amp; → "&", &#160; → " ", etc.)
	text = html.UnescapeString(text)
	text = wikiHTMLCommentRe.ReplaceAllString(text, "")
	text = wikiRefBlockRe.ReplaceAllString(text, "")
	text = wikiRefSelfRe.ReplaceAllString(text, "")
	text = wikiHTMLTagRe.ReplaceAllString(text, "")

	// Resolve innermost {{ }} templates first (no nesting), repeat until stable
	for {
		n := wikiTmplRe.ReplaceAllStringFunc(text, func(m string) string {
			inner := m[2 : len(m)-2]
			return resolveTemplate(inner)
		})
		if n == text {
			break
		}
		text = n
	}
	// [[link|display]] → display
	text = wikiLinkDispRe.ReplaceAllString(text, "$1")
	// [[link]] → link
	text = wikiLinkRe.ReplaceAllString(text, "$1")
	// remove '''bold''' and ''italic'' markers
	text = strings.ReplaceAll(text, "'''", "")
	text = strings.ReplaceAll(text, "''", "")
	// remove wikitext list/indent markers at start of lines
	text = wikiListMarkerRe.ReplaceAllString(text, "")
	// tidy up punctuation artefacts left by removed templates: ". ." "( )" ", ,"
	text = wikiDupDotRe.ReplaceAllString(text, "")
	text = wikiDupCommaRe.ReplaceAllString(text, "")
	text = wikiEmptyParenRe.ReplaceAllString(text, "")
	// tidy up spacing artefacts from removed templates
	text = wikiMultiSpaceRe.ReplaceAllString(text, " ")
	text = wikiMultiNewlineRe.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)
	// Remove dangling connector/preposition fragments left when a transclusion
	// template resolved to empty (e.g. RU "Происходит от" with no continuation).
	if danglingConnectorRe.MatchString(text) {
		return ""
	}
	return text
}

// ── Data types ────────────────────────────────────────────────────────────────

type Def struct {
	Text    string
	Example string
}

type Meaning struct {
	POS  string
	Defs []Def
	Syns []string
}

type Definition struct {
	Phonetic string
	Meanings []Meaning
}

type Translation struct {
	Word     string
	Detected string
	Source   string // "gtx" or "mymemory"
}

// LexEntry is the value stored in each embedded data pack entry.
// Field names must be stable — gob round-trips between cmdInstall (writer) and installedPack.Lookup (reader).
type LexEntry struct {
	IPA      string
	Meanings []Meaning
	Syns     []string // union across senses; caller trims to display limit
	Etym     string
}

// xdgDataDir returns ~/.local/share/lexify (or $XDG_DATA_HOME/lexify).
func xdgDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "lexify")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "lexify")
}

// installedPack is the offline lookup provider backed by a user-installed pack on disk.
// Pack format (written by cmdInstall):
//
//	lang.idx — sorted fixed-size records: [32-byte key, zero-padded][8-byte offset, big-endian]
//	lang.dat — sequential entries:        [4-byte length, big-endian][gob-encoded LexEntry bytes]
//
// Lookup is O(log n) binary search over the in-memory index (~32 MB for EN)
// + one file seek + decode of a single gob entry. Total: ~30 ms cold, ~1 ms warm.
type installedPack struct {
	idxPath string
	datPath string
	once    sync.Once
	idxData []byte // full .idx loaded once; 40 bytes × N entries
}

const idxRecordSize = 40 // 32-byte key + 8-byte offset
const idxKeySize = 32

func (p *installedPack) init() {
	p.once.Do(func() {
		data, err := os.ReadFile(p.idxPath)
		if err != nil || len(data)%idxRecordSize != 0 {
			return
		}
		p.idxData = data
	})
}

func (p *installedPack) Lookup(word string) *LexEntry {
	p.init()
	if len(p.idxData) == 0 {
		return nil
	}
	key := []byte(strings.ToLower(word))
	if len(key) > idxKeySize {
		return nil // key too long to be in index
	}
	var keyBuf [idxKeySize]byte
	copy(keyBuf[:], key)

	// Binary search over fixed-size records.
	n := len(p.idxData) / idxRecordSize
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		rec := p.idxData[mid*idxRecordSize : mid*idxRecordSize+idxRecordSize]
		cmp := bytes.Compare(keyBuf[:], rec[:idxKeySize])
		switch {
		case cmp == 0:
			offset := binary.BigEndian.Uint64(rec[idxKeySize : idxKeySize+8])
			f, err := os.Open(p.datPath)
			if err != nil {
				return nil
			}
			defer f.Close()
			if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
				return nil
			}
			var lenBuf [4]byte
			if _, err := io.ReadFull(f, lenBuf[:]); err != nil {
				return nil
			}
			entryLen := binary.BigEndian.Uint32(lenBuf[:])
			if entryLen == 0 || entryLen > 1<<20 {
				return nil
			}
			entryBytes := make([]byte, entryLen)
			if _, err := io.ReadFull(f, entryBytes); err != nil {
				return nil
			}
			var e LexEntry
			if err := gob.NewDecoder(bytes.NewReader(entryBytes)).Decode(&e); err != nil {
				return nil
			}
			return &e
		case cmp < 0:
			hi = mid
		default:
			lo = mid + 1
		}
	}
	return nil
}

// enProvider is the active EN pack, backed by a user-installed XDG pack.
// nil when no pack is installed; lookupEN returns nil in that case and run() falls
// back to the dictionaryapi.dev network API.
var enProvider = func() *installedPack {
	dir := xdgDataDir()
	idxPath := filepath.Join(dir, "en.idx")
	datPath := filepath.Join(dir, "en.dat")
	if _, err := os.Stat(idxPath); err == nil {
		p := &installedPack{idxPath: idxPath, datPath: datPath}
		go p.init()
		return p
	}
	return nil
}()

func lookupEN(word string) *LexEntry {
	if enProvider == nil {
		return nil
	}
	return enProvider.Lookup(word)
}

// packRegistry holds installed packs for non-EN languages.
// Built once at startup by scanning the XDG data dir.
var packRegistry = func() map[string]*installedPack {
	packs := map[string]*installedPack{}
	dir := xdgDataDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return packs
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".idx") {
			continue
		}
		lang := strings.TrimSuffix(name, ".idx")
		if lang == "en" {
			continue // handled by enProvider
		}
		idxPath := filepath.Join(dir, name)
		datPath := filepath.Join(dir, lang+".dat")
		if _, err := os.Stat(datPath); err != nil {
			continue
		}
		p := &installedPack{idxPath: idxPath, datPath: datPath}
		go p.init()
		packs[lang] = p
	}
	return packs
}()

func lookupLang(word, lang string) *LexEntry {
	if p := packRegistry[lang]; p != nil {
		return p.Lookup(word)
	}
	return nil
}

// ── Wiktionary XML types (used by 'lexify -i') ───────────────────────────────

// langNames maps BCP-47 codes to their English-language names as used in
// Wiktionary level-2 section headings (== English ==, == Russian ==, etc.).
var langNames = map[string]string{
	"en": "English",
	"de": "German", "fr": "French", "es": "Spanish", "it": "Italian",
	"pt": "Portuguese", "ru": "Russian", "ja": "Japanese", "zh": "Chinese",
	"ko": "Korean", "nl": "Dutch", "pl": "Polish", "sv": "Swedish",
	"ar": "Arabic", "tr": "Turkish", "uk": "Ukrainian", "hi": "Hindi",
}

// wiktionaryDumpURL returns the en.wiktionary.org XML dump URL.
// The dump is bzip2-compressed MediaWiki XML and contains entries for all
// languages in the same wikitext format our live-API parser already handles.
func wiktionaryDumpURL() string {
	return "https://dumps.wikimedia.org/enwiktionary/latest/enwiktionary-latest-pages-articles.xml.bz2"
}

// xmlPage is used to stream-decode pages from a MediaWiki XML dump.
type xmlPage struct {
	Title    string `xml:"title"`
	NS       int    `xml:"ns"`
	Revision struct {
		Text string `xml:"text"`
	} `xml:"revision"`
}

// posSections is the set of Wiktionary section headings (lowercased) that
// represent a part-of-speech and contain numbered definitions.
var posSections = map[string]struct{}{
	"noun": {}, "verb": {}, "adjective": {}, "adverb": {},
	"pronoun": {}, "preposition": {}, "conjunction": {},
	"interjection": {}, "determiner": {}, "article": {},
	"numeral": {}, "particle": {}, "phrase": {},
	"suffix": {}, "prefix": {}, "affix": {},
	"proper noun": {}, "proverb": {}, "idiom": {},
}

// ipaExtractRe matches the first IPA argument from {{IPA|lang|/…/|…}}.
var ipaExtractRe = regexp.MustCompile(`\{\{IPA\|[^|{}]+\|([^|{}]+)`)

// extractL2Section returns the wikitext between the == sectionName == heading
// (case-insensitive) and the next level-2 heading.
func extractL2Section(wikitext, sectionName string) string {
	lines := strings.Split(wikitext, "\n")
	var result []string
	inSection := false
	for _, line := range lines {
		level, heading := parseWikiHeading(line)
		if level == 2 {
			if inSection {
				break
			}
			if strings.EqualFold(heading, sectionName) {
				inSection = true
			}
			continue
		}
		if inSection {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// appendUniq appends elements from src to dst, skipping duplicates.
func appendUniq(dst []string, src ...string) []string {
	seen := make(map[string]bool, len(dst))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range src {
		if v != "" && !seen[v] {
			seen[v] = true
			dst = append(dst, v)
		}
	}
	return dst
}

// truncateRunes returns s truncated to maxLen runes at a word boundary, appending "…".
// Returns the original string unchanged when len([]rune(s)) <= maxLen.
func truncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	i := maxLen - 3
	for i > 0 && r[i] != ' ' {
		i--
	}
	if i == 0 {
		i = maxLen - 3
	}
	return string(r[:i]) + "…"
}

// trimEtym caps etymology text at 500 runes at a sentence boundary.
func trimEtym(etym string) string {
	r := []rune(etym)
	if len(r) <= 500 {
		return etym
	}
	for i := 499; i >= 0; i-- {
		if r[i] == '.' || r[i] == '!' || r[i] == '?' {
			if i+1 == len(r) || r[i+1] == ' ' {
				return strings.TrimSpace(string(r[:i+1])) + "…"
			}
		}
	}
	return truncateRunes(etym, 500)
}

// parsePOSSection extracts up to 3 definitions (with first usage example each)
// from a Wiktionary POS section's wikitext. Returns nil if no definitions found.
func parsePOSSection(pos, text string) *Meaning {
	var defs []Def
	var pendingText string
	var pendingEx string
	flush := func() {
		if pendingText != "" {
			defs = append(defs, Def{Text: pendingText, Example: pendingEx})
			pendingText = ""
			pendingEx = ""
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if len(defs) >= 3 {
			break
		}
		switch {
		// Top-level definition: starts with '# ' (not '## ' subsense)
		case strings.HasPrefix(line, "# "):
			flush()
			pendingText = strings.TrimSpace(stripWikitext(strings.TrimPrefix(line, "# ")))
		// Usage example: starts with '#:' — take first one per definition
		case strings.HasPrefix(line, "#:") && pendingText != "" && pendingEx == "":
			raw := strings.TrimSpace(strings.TrimPrefix(line, "#:"))
			ex := strings.TrimSpace(stripWikitext(raw))
			if len([]rune(ex)) >= 2 {
				pendingEx = truncateRunes(ex, 150)
			}
		}
	}
	flush()
	if len(defs) == 0 {
		return nil
	}
	// Collect sense-level synonyms from {{syn|lang|word1|word2|…}} templates.
	seen := map[string]bool{}
	var syns []string
	for _, sm := range synTemplateRe.FindAllStringSubmatch(text, -1) {
		for _, w := range strings.Split(sm[1], "|") {
			w = strings.TrimSpace(w)
			if w != "" && !strings.Contains(w, "=") && !seen[w] {
				seen[w] = true
				syns = append(syns, w)
			}
		}
	}
	return &Meaning{POS: normalizePOS(pos), Defs: defs, Syns: syns}
}

// parseSynSection extracts synonym words from a Wiktionary Synonyms section.
func parseSynSection(text string) []string {
	stripped := stripWikitext(text)
	seen := map[string]bool{}
	var result []string
	for _, line := range strings.Split(stripped, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, w := range synSplitRe.Split(line, -1) {
			w = strings.TrimSpace(w)
			r := []rune(w)
			if len(r) < 2 || len(r) > 40 {
				continue
			}
			if unicode.IsUpper(r[0]) {
				continue
			}
			if !seen[w] {
				seen[w] = true
				result = append(result, w)
			}
			if len(result) >= 10 {
				return result
			}
		}
	}
	return result
}

// parseWiktionaryPage extracts a LexEntry from a Wiktionary page's wikitext.
// langSection is the English-language name of the section to extract
// (e.g. "English", "Russian"). Returns false if the section is absent.
func parseWiktionaryPage(wikitext, langSection string) (LexEntry, bool) {
	langText := extractL2Section(wikitext, langSection)
	if langText == "" {
		return LexEntry{}, false
	}
	var (
		ipa      string
		meanings []Meaning
		etym     string
		allSyns  []string
	)
	var curHeading string
	var curLines []string
	flush := func() {
		if curHeading == "" {
			return
		}
		text := strings.Join(curLines, "\n")
		lower := strings.ToLower(strings.TrimSpace(curHeading))
		_, isPOS := posSections[lower]
		switch {
		case lower == "pronunciation":
			if m := ipaExtractRe.FindStringSubmatch(text); m != nil {
				ipa = strings.TrimSpace(m[1])
			}
		case lower == "etymology" || strings.HasPrefix(lower, "etymology "):
			etym = trimEtym(stripWikitext(text))
		case isPOS:
			if m := parsePOSSection(curHeading, text); m != nil {
				meanings = append(meanings, *m)
				allSyns = appendUniq(allSyns, m.Syns...)
			}
		case lower == "synonyms":
			syns := parseSynSection(text)
			if len(meanings) > 0 {
				meanings[len(meanings)-1].Syns = appendUniq(meanings[len(meanings)-1].Syns, syns...)
			}
			allSyns = appendUniq(allSyns, syns...)
		}
		curLines = nil
	}
	for _, line := range strings.Split(langText, "\n") {
		level, heading := parseWikiHeading(line)
		if level >= 3 {
			flush()
			curHeading = heading
		} else {
			curLines = append(curLines, line)
		}
	}
	flush()
	if len(meanings) == 0 && ipa == "" && etym == "" {
		return LexEntry{}, false
	}
	return LexEntry{IPA: ipa, Meanings: meanings, Syns: allSyns, Etym: etym}, true
}

// ── Kaikki JSONL types (used by 'lexify -i --kaikki') ──────────────────────

// kEntry mirrors the Kaikki JSONL schema for stream-parsing.
type kEntry struct {
	Word   string `json:"word"`
	POS    string `json:"pos"`
	Sounds []struct {
		IPA  string   `json:"ipa"`
		Tags []string `json:"tags"`
	} `json:"sounds"`
	Senses []struct {
		Glosses  []string `json:"glosses"`
		Examples []struct {
			Text string `json:"text"`
		} `json:"examples"`
		Synonyms []struct {
			Word string `json:"word"`
		} `json:"synonyms"`
	} `json:"senses"`
	Synonyms []struct {
		Word string `json:"word"`
	} `json:"synonyms"`
	EtymologyText string `json:"etymology_text"`
	LangCode      string `json:"lang_code"`
}

// kaikkiURL returns the Kaikki.org JSONL download URL for the given language.
// Used for EN and languages without a native kaikki wiktionary edition.
func kaikkiURL(langName string) string {
	return fmt.Sprintf("https://kaikki.org/dictionary/%s/kaikki.org-dictionary-%s.jsonl", langName, langName)
}

// kaikkiNativeEditions maps BCP-47 codes to the kaikki.org native-wiktionary
// edition prefix (e.g. "ru" → "ruwiktionary"). Packs from these editions have
// etymology_text written in the target language rather than English.
var kaikkiNativeEditions = map[string]string{
	"de": "de", "es": "es", "fr": "fr", "it": "it",
	"ja": "ja", "ko": "ko", "nl": "nl", "pl": "pl",
	"pt": "pt", "ru": "ru", "tr": "tr", "zh": "zh",
}

// kaikkiNativeURL returns the raw wiktextract JSONL.GZ download URL for langs
// that have a native kaikki wiktionary edition, or ("", false) otherwise.
func kaikkiNativeURL(lang string) (string, bool) {
	if _, ok := kaikkiNativeEditions[lang]; ok {
		return fmt.Sprintf("https://kaikki.org/%swiktionary/raw-wiktextract-data.jsonl.gz", lang), true
	}
	return "", false
}

// nativePath returns the path to the lang.native marker file which records
// that the installed pack was built from a native-language wiktionary edition.
func nativePath(lang string) string {
	return filepath.Join(xdgDataDir(), lang+".native")
}

// isNativeLangPack reports whether the installed pack for lang was sourced from
// a native kaikki wiktionary edition (so etymology_text is in the target lang).
func isNativeLangPack(lang string) bool {
	_, err := os.Stat(nativePath(lang))
	return err == nil
}

// convertKEntry converts a raw Kaikki entry to a LexEntry.
// Glosses and etymology are run through stripWikitext to clean light markup.
func convertKEntry(k kEntry) (string, LexEntry) {
	key := strings.ToLower(strings.TrimSpace(k.Word))
	ipa := ""
	for _, s := range k.Sounds {
		if strings.HasPrefix(s.IPA, "/") || strings.HasPrefix(s.IPA, "[") {
			ipa = s.IPA
			break
		}
	}
	if ipa == "" && len(k.Sounds) > 0 {
		ipa = k.Sounds[0].IPA
	}
	pos := strings.TrimSpace(k.POS)
	seenSyn := map[string]bool{}
	var senseSyns []string
	var defs []Def
	for _, s := range k.Senses {
		if len(s.Glosses) == 0 {
			continue
		}
		gloss := strings.TrimSpace(stripWikitext(s.Glosses[0]))
		if gloss == "" {
			continue
		}
		example := ""
		if len(s.Examples) > 0 {
			ex := strings.TrimSpace(s.Examples[0].Text)
			if len([]rune(ex)) >= 2 {
				example = truncateRunes(ex, 150)
			}
		}
		defs = append(defs, Def{Text: gloss, Example: example})
		for _, syn := range s.Synonyms {
			w := strings.TrimSpace(syn.Word)
			if w != "" && !seenSyn[w] {
				seenSyn[w] = true
				senseSyns = append(senseSyns, w)
			}
		}
	}
	for _, syn := range k.Synonyms {
		w := strings.TrimSpace(syn.Word)
		if w != "" && !seenSyn[w] {
			seenSyn[w] = true
			senseSyns = append(senseSyns, w)
		}
	}
	var meanings []Meaning
	if pos != "" && len(defs) > 0 {
		meanings = append(meanings, Meaning{POS: normalizePOS(pos), Defs: defs, Syns: senseSyns})
	}
	etym := trimEtym(stripWikitext(strings.TrimSpace(k.EtymologyText)))
	return key, LexEntry{IPA: ipa, Meanings: meanings, Syns: senseSyns, Etym: etym}
}

// mergeKEntry merges an incoming Kaikki entry (separate POS line) into an existing one.
func mergeKEntry(existing, incoming LexEntry) LexEntry {
	existing.Meanings = append(existing.Meanings, incoming.Meanings...)
	existing.Syns = appendUniq(existing.Syns, incoming.Syns...)
	if existing.IPA == "" {
		existing.IPA = incoming.IPA
	}
	if existing.Etym == "" {
		existing.Etym = incoming.Etym
	}
	return existing
}

// ── Fetchers ──────────────────────────────────────────────────────────────────

func fetchDefinition(word string) *Definition {
	var raw []struct {
		Phonetic string `json:"phonetic"`
		Meanings []struct {
			PartOfSpeech string `json:"partOfSpeech"`
			Definitions  []struct {
				Definition string `json:"definition"`
				Example    string `json:"example"`
			} `json:"definitions"`
			Synonyms []string `json:"synonyms"`
		} `json:"meanings"`
	}
	if err := fetchJSON("https://api.dictionaryapi.dev/api/v2/entries/en/"+url.PathEscape(word), &raw); err != nil || len(raw) == 0 {
		return nil
	}
	d := &Definition{Phonetic: raw[0].Phonetic}
	for i, m := range raw[0].Meanings {
		if i >= 3 {
			break
		}
		meaning := Meaning{POS: normalizePOS(m.PartOfSpeech)}
		for j, def := range m.Definitions {
			if j >= 3 {
				break
			}
			meaning.Defs = append(meaning.Defs, Def{Text: def.Definition, Example: def.Example})
		}
		syns := m.Synonyms
		if len(syns) > 10 {
			syns = syns[:10]
		}
		meaning.Syns = syns
		d.Meanings = append(d.Meanings, meaning)
	}
	return d
}

// parseWikiHeading returns the heading level and trimmed title for a wikitext
// heading line (e.g. "===Etymology 1===" → 3, "Etymology 1").
// Returns 0, "" if the line is not a heading.
func parseWikiHeading(line string) (int, string) {
	line = strings.TrimRight(line, " \t")
	if len(line) < 4 || line[0] != '=' {
		return 0, ""
	}
	open := 0
	for open < len(line) && line[open] == '=' {
		open++
	}
	if open < 2 || open > 4 {
		return 0, ""
	}
	close := 0
	for close < len(line) && line[len(line)-1-close] == '=' {
		close++
	}
	if close != open {
		return 0, ""
	}
	inner := strings.TrimSpace(line[open : len(line)-close])
	if inner == "" {
		return 0, ""
	}
	return open, inner
}

// extractWikiSection walks pre-fetched wikitext and returns the body of the
// first section whose heading satisfies match (stops at the next heading).
func extractWikiSection(fullText string, match func(string) bool) string {
	lines := strings.Split(fullText, "\n")
	var result []string
	inSection := false
	for _, line := range lines {
		level, heading := parseWikiHeading(line)
		if level > 0 {
			if inSection {
				break
			}
			if match(heading) {
				inSection = true
			}
			continue
		}
		if inSection {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// wikiRedirectLinkRe extracts the redirect target from a wikitext redirect line.
var wikiRedirectLinkRe = regexp.MustCompile(`\[\[([^\]#|]+)`)

// isWikiRedirect returns the redirect target if the wikitext is a redirect page,
// or "" if it is a normal content page. Redirect pages have their very first
// non-empty line starting with "#" followed by [[target]] (all language wikis).
func isWikiRedirect(text string) string {
	for _, line := range strings.SplitN(text, "\n", 5) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return "" // first content line is not a redirect
		}
		if m := wikiRedirectLinkRe.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
		return ""
	}
	return ""
}

// fetchFullWikitext retrieves the raw wikitext for a page via the MediaWiki
// action=query&prop=revisions API. Follows one level of wikitext redirect
// (#REDIRECT / #WEITERLEITUNG / etc.) automatically. Returns "" on error.
func fetchFullWikitext(base, word string) string {
	text := fetchWikitextRaw(base, word)
	if text == "" {
		return ""
	}
	// Follow one redirect (inflected forms, spelling variants, etc.)
	if target := isWikiRedirect(text); target != "" && target != word {
		if t2 := fetchWikitextRaw(base, target); t2 != "" {
			return t2
		}
	}
	return text
}

// fetchWikitextRaw is the raw (no redirect-following) wikitext fetcher.
func fetchWikitextRaw(base, word string) string {
	var resp struct {
		Query struct {
			Pages map[string]struct {
				Revisions []struct {
					Text string `json:"*"`
				} `json:"revisions"`
			} `json:"pages"`
		} `json:"query"`
	}
	// &redirects tells MediaWiki to follow canonical DB-level redirects automatically.
	if err := fetchJSON(base+"?action=query&titles="+url.QueryEscape(word)+"&prop=revisions&rvprop=content&redirects&format=json", &resp); err != nil {
		return ""
	}
	for _, p := range resp.Query.Pages {
		if len(p.Revisions) > 0 {
			return p.Revisions[0].Text
		}
	}
	return ""
}

// wiktionarySection fetches the full page wikitext in a single API call and
// extracts the first section whose heading satisfies match.
// This avoids the two-round-trip sections-index → wikitext pattern.
func wiktionarySection(base, word string, match func(string) bool) string {
	return extractWikiSection(fetchFullWikitext(base, word), match)
}

// parseGTXSegments concatenates the translated text chunks from raw[0] of a GTX response.
func parseGTXSegments(raw []json.RawMessage) string {
	var segs [][]json.RawMessage
	var sb strings.Builder
	if json.Unmarshal(raw[0], &segs) == nil {
		for _, seg := range segs {
			if len(seg) > 0 {
				var s string
				if json.Unmarshal(seg[0], &s) == nil {
					sb.WriteString(s)
				}
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// fetchGTX hits the undocumented Google Translate gtx endpoint.
// Returns the translation and a boolean indicating HTTP 429 rate-limiting.
func fetchGTX(word, lang string) (*Translation, bool) {
	req, err := http.NewRequest("GET", "https://translate.google.com/translate_a/single?client=gtx&sl=auto&tl="+url.QueryEscape(lang)+"&dt=t&dt=bd&q="+url.QueryEscape(word), nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, true
	}
	var raw []json.RawMessage
	if json.NewDecoder(resp.Body).Decode(&raw) != nil || len(raw) == 0 {
		return nil, false
	}
	translated := parseGTXSegments(raw)
	detected := "?"
	if len(raw) > 2 {
		json.Unmarshal(raw[2], &detected) //nolint
	}
	if translated == "" || strings.EqualFold(translated, word) {
		return nil, false
	}
	return &Translation{Word: translated, Detected: detected, Source: "gtx"}, false
}

// fetchMyMemory is a documented, stable, no-key fallback (5k chars/day free).
func fetchMyMemory(word, lang string) *Translation {
	endpoint := "https://api.mymemory.translated.net/get?q=" +
		url.QueryEscape(word) + "&langpair=en|" + url.QueryEscape(lang)
	var raw struct {
		ResponseData struct {
			TranslatedText string `json:"translatedText"`
		} `json:"responseData"`
		ResponseStatus int `json:"responseStatus"`
	}
	if err := fetchJSON(endpoint, &raw); err != nil {
		return nil
	}
	if raw.ResponseStatus != 200 {
		return nil
	}
	t := strings.TrimSpace(raw.ResponseData.TranslatedText)
	if t == "" || strings.EqualFold(t, word) || strings.Contains(strings.ToUpper(t), "INVALID") {
		return nil
	}
	// TODO: Detected is hardcoded "en" because MyMemory's responseData does not
	// expose the detected source language. The match.segment field in the full
	// response can carry it, but only when langpair is "autodetect|<lang>" \u2014
	// swap the endpoint's "auto" to "autodetect" and parse match[0].source to
	// populate Detected correctly instead of assuming English.
	return &Translation{Word: t, Detected: "en", Source: "mymemory"}
}

// fetchDefnAndEtymPar fetches an English definition and etymology concurrently,
// returning both along with their individual round-trip durations.
// Replaces the repeated inline wgFB goroutine pair that appeared in run().
func fetchDefnAndEtymPar(word string) (defn *Definition, etym string, tDefn, tEtym time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		t := time.Now()
		defn = fetchDefinition(word)
		tDefn = time.Since(t)
	}()
	go func() {
		defer wg.Done()
		t := time.Now()
		etym = fetchEtymologyWiki(word)
		tEtym = time.Since(t)
	}()
	wg.Wait()
	return
}

func fetchTranslation(word, lang string) *Translation {
	if lang == "" || strings.ToLower(lang) == "en" {
		return nil
	}
	// Race GTX and MyMemory in parallel. Both goroutines always complete because
	// the channel is buffered(2), avoiding leaks regardless of which path returns.
	type gtxRes struct {
		t  *Translation
		rl bool // true = HTTP 429 rate-limited
	}
	gtxCh := make(chan gtxRes, 1)
	mmCh  := make(chan *Translation, 1)
	go func() { t, rl := fetchGTX(word, lang); gtxCh <- gtxRes{t, rl} }()
	go func() { mmCh <- fetchMyMemory(word, lang) }()

	// Wait for the first result that is either a success or a definitive GTX failure.
	var gtxR gtxRes
	var mmT  *Translation
	gtxDone, mmDone := false, false
	for !gtxDone || !mmDone {
		select {
		case gtxR = <-gtxCh:
			gtxDone = true
			if gtxR.t != nil {
				return gtxR.t // GTX succeeded — no need to wait for MyMemory
			}
		case mmT = <-mmCh:
			mmDone = true
		}
		// If MyMemory has a result and GTX has already definitively failed (nil,
		// whether 429 or otherwise), we can return now.
		if mmDone && mmT != nil && gtxDone {
			if gtxR.rl {
				mmT.Source = "mymemory(gtx↯)"
			}
			return mmT
		}
		// If MyMemory finished but GTX is still in-flight, keep waiting so we
		// can annotate the rate-limit flag if needed.
	}
	// Both done; GTX returned nil.
	if mmT != nil && gtxR.rl {
		mmT.Source = "mymemory(gtx↯)"
	}
	return mmT
}

// etymHeadingPat matches the "Etymology" section heading across major Wiktionary languages.
var etymHeadingPat = regexp.MustCompile(
	`(?i)(` +
		`etymology|` + // en
		`herkunft|wortherkunft|` + // de
		`[eé]tymologie|` + // fr
		`[eé]timolog[íi]a|` + // es, pt, it
		`этимология|этимологія|` + // ru, uk
		`語源|어원` + // ja, ko
		`)`,
)

// synHeadingPat matches the "Synonyms" section heading across major Wiktionary languages.
// Uses a word-boundary style match (no ^ anchor) since some wikis wrap headings in <span> tags.
var synHeadingPat = regexp.MustCompile(
	`(?i)(` +
		`synonyms?|` + // en
		`synonyme|` + // de, fr (Synonymes)
		`sin[oó]nimo|` + // es, pt
		`sinonimo|` + // it
		`синоним|` + // ru, uk, bg (Синонимы / Синоніми)
		`同義語|類義語|近義詞|近义词|` + // ja, zh
		`동의어|유의어|` + // ko
		`مرادف` + // ar
		`)`,
)

// fetchEtymologyWiki fetches and strips the Etymology section from en.wiktionary.org.
// Single API call (action=query&prop=revisions); runs in parallel with fetchDefinition.
func fetchEtymologyWiki(word string) string {
	wt := wiktionarySection(
		"https://en.wiktionary.org/w/api.php", word,
		func(heading string) bool {
			h := strings.ToLower(heading)
			return h == "etymology" || strings.HasPrefix(h, "etymology ")
		},
	)
	if wt == "" {
		return ""
	}
	return stripWikitext(wt)
}

// parseSynsFromWikitext extracts synonym words from pre-fetched wikitext.
func parseSynsFromWikitext(fullText, lang string) []string {
	lines := strings.Split(fullText, "\n")
	wt := extractWikiSection(fullText, synHeadingPat.MatchString)
	if wt == "" {
		inlineRe := regexp.MustCompile(`(?i)\{\{Synonym`)
		var buf []string
		collecting := false
		for _, line := range lines {
			if !collecting {
				if inlineRe.MatchString(line) {
					collecting = true
					buf = append(buf, line)
				}
			} else {
				if strings.HasPrefix(line, "{{") || strings.HasPrefix(strings.TrimSpace(line), "==") {
					break
				}
				buf = append(buf, line)
			}
		}
		wt = strings.Join(buf, "\n")
	}
	if wt == "" {
		return nil
	}
	linkRe := regexp.MustCompile(`\[\[(?:[^\]|]+\|)?([^\]|]+)\]\]`)
	matches := linkRe.FindAllStringSubmatch(wt, -1)
	digitSlashRe := regexp.MustCompile(`[\d/]`)
	seen := map[string]bool{}
	var result []string
	for _, m := range matches {
		w := strings.TrimSpace(m[1])
		runes := []rune(w)
		if len(runes) < 2 || len(runes) > 30 {
			continue
		}
		if lang == "en" && unicode.IsUpper(runes[0]) {
			continue
		}
		if digitSlashRe.MatchString(w) {
			continue
		}
		if !seen[w] {
			seen[w] = true
			result = append(result, w)
		}
		if len(result) >= 10 {
			break
		}
	}
	return result
}

// fetchTextTranslation machine-translates a multi-line text block via Google GTX.
// Used only as a last-resort fallback when native target-language content is unavailable.
func fetchTextTranslation(text, lang string) string {
	if text == "" || lang == "" || strings.ToLower(lang) == "en" {
		return ""
	}
	var raw []json.RawMessage
	if err := fetchJSON("https://translate.google.com/translate_a/single?client=gtx&sl=auto&tl="+url.QueryEscape(lang)+"&dt=t&q="+url.QueryEscape(text), &raw); err != nil || len(raw) == 0 {
		return ""
	}
	return parseGTXSegments(raw)
}

// headingTemplateArgRe extracts the first non-empty argument of a Wiktionary heading
// template, e.g. {{Wortart|Adjektiv|Deutsch}} → "Adjektiv", {{S|adjectif|fr}} → "adjectif".
var headingTemplateArgRe = regexp.MustCompile(`\{\{[^|{}]+\|([^|{}]+)`)

// langCodeRe matches BCP-47 language codes (2–3 lowercase ASCII letters, whole string).
var langCodeRe = regexp.MustCompile(`^[a-z]{2,3}$`)

// extractHeadingPOS returns the part-of-speech name from a wikitext heading string.
// Plain-text headings (EN wiki) are returned as-is; template-based headings used by
// DE, FR, RU, etc. are resolved via their first template argument.
func extractHeadingPOS(heading string) string {
	bare := strings.TrimSpace(stripWikitext(heading))
	if bare != "" {
		return bare
	}
	if m := headingTemplateArgRe.FindStringSubmatch(heading); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// defnItemRe matches definition list items in Wiktionary wikitext:
//   - `# text`      — most language wikis (fr, ru, es, it, pt, …)
//   - `:[1] text`   — German Wiktionary
//   - `:[[1]] text` — German Wiktionary alternate form
var defnItemRe = regexp.MustCompile(`^(?:#|:\[+\d+\]+)\s*(.+)`)

// parseDefsFromWikitext extracts native-language Meaning entries from wikitext
// fetched from a target-language Wiktionary. Groups definition items by the
// nearest preceding level-3+ heading (used as POS label).
func parseDefsFromWikitext(fullText string) []Meaning {
	lines := strings.Split(fullText, "\n")
	type posGroup struct {
		pos  string
		defs []string
	}
	var groups []posGroup
	curPOS := ""
	for _, line := range lines {
		level, heading := parseWikiHeading(line)
		if level > 0 {
			if level >= 3 {
				if pos := extractHeadingPOS(heading); pos != "" {
					curPOS = pos
				}
			}
			continue
		}
		// Skip sub-list lines (examples, sub-defs, usage notes).
		if strings.HasPrefix(line, "##") || strings.HasPrefix(line, "#:") || strings.HasPrefix(line, "#*") {
			continue
		}
		m := defnItemRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		text := strings.TrimSpace(stripWikitext(m[1]))
		if len([]rune(text)) < 3 {
			continue
		}
		if len(groups) == 0 || groups[len(groups)-1].pos != curPOS {
			if len(groups) >= 3 {
				break // cap: at most 3 POS groups
			}
			groups = append(groups, posGroup{pos: curPOS})
		}
		last := &groups[len(groups)-1]
		if len(last.defs) < 4 {
			last.defs = append(last.defs, text)
		}
	}
	var result []Meaning
	for _, g := range groups {
		if len(g.defs) == 0 {
			continue
		}
		pos := normalizePOS(g.pos)
		if pos == "" {
			pos = "word"
		}
		var defs []Def
		for _, d := range g.defs {
			defs = append(defs, Def{Text: d})
		}
		result = append(result, Meaning{POS: pos, Defs: defs})
	}
	return result
}

// templateArgRe captures up to the first 3 positional arguments of any wikitext
// template. Named/keyword arguments (containing "=") are excluded by the character
// class so they don't pollute the positional slots.
var templateArgRe = regexp.MustCompile(`\{\{[^|{}\n]+\|([^|={}\n\[\]]{2,80})(?:\|([^|={}\n\[\]]{2,80}))?(?:\|([^|={}\n\[\]]{2,80}))?`)

// wikiFlexionLemma extracts the lemma (base form) from an inflection/Flexion page's
// wikitext without needing to know any template names. It scans the first positional
// arguments of every template on the page and returns the first candidate C where:
//
//   - word starts with C  (inflected form is always longer than its lemma)
//   - len(C) >= 3         (avoid matching lang codes like "de", "fr")
//
// This is language- and template-name-agnostic: works for DE {{Deklinationsseite}},
// {{inflection of}}, and any other convention where the lemma appears as a positional
// template argument — which is universal across all major Wiktionary editions.
// Requires no additional API call since we already hold the wikitext.
func wikiFlexionLemma(wikitext, word string) string {
	wLower := strings.ToLower(word)
	for _, m := range templateArgRe.FindAllStringSubmatch(wikitext, 40) {
		for _, arg := range m[1:] {
			arg = strings.TrimSpace(arg)
			if arg == "" {
				continue
			}
			aLower := strings.ToLower(arg)
			if aLower == wLower {
				continue
			}
			if strings.HasPrefix(wLower, aLower) && len([]rune(arg)) >= 3 {
				return arg
			}
		}
	}
	return ""
}

// wiktionaryLemma uses the opensearch API to find the canonical/lemma form of
// a word when the direct page lookup returns nothing (e.g. inflected form with
// no own article). Returns the first result that is a proper prefix of word
// (so "einheitlich" is preferred over "einheitliche"), or the first result that
// differs from word by simple suffix, or "" if nothing useful is found.
func wiktionaryLemma(base, word string) string {
	var resp [4]json.RawMessage
	if err := fetchJSON(base+"?action=opensearch&search="+url.QueryEscape(word)+"&limit=5&namespace=0&format=json", &resp); err != nil {
		return ""
	}
	var titles []string
	if json.Unmarshal(resp[1], &titles) != nil {
		return ""
	}
	wLower := strings.ToLower(word)
	for _, t := range titles {
		tLower := strings.ToLower(t)
		if tLower == wLower {
			continue
		}
		// Prefer a result that is a base form: word starts with the result
		// (e.g. word="einheitliche", t="einheitlich").
		if strings.HasPrefix(wLower, tLower) && len([]rune(t)) >= 3 {
			return t
		}
	}
	return ""
}

// wikiResult holds the data fetched from lang.wiktionary.org for a target-language word.
type wikiResult struct {
	Meanings     []Meaning
	Syns         []string
	Etym         string
	EtymFromWiki bool   // set when etym came from wiki (pack had none); used for debug source label
	ResolvedWord string // non-empty when a lemma was resolved from the inflected input form
}

// fetchTargetWikiData fetches native definitions, synonyms, and etymology for a word
// from lang.wiktionary.org in a single request.
// Falls back to an opensearch-based lemma lookup when the direct page is absent OR
// when the page exists but has no definitions (e.g. de.wiktionary.org Flexion pages
// for inflected forms like "einheitliche" which contain only conjugation tables).
func fetchTargetWikiData(word, lang string) wikiResult {
	apiBase := "https://" + lang + ".wiktionary.org/w/api.php"
	fullText := fetchFullWikitext(apiBase, word)

	// Pre-check definitions so we know whether to attempt lemma resolution.
	var wr wikiResult
	if fullText != "" {
		wr.Meanings = parseDefsFromWikitext(fullText)
	}

	// Trigger lemma fallback if the page was absent OR if it yielded no usable
	// definitions (inflection/Flexion pages exist but contain no def sections).
	if len(wr.Meanings) == 0 {
		// 1. Try to read the lemma from the Flexion page wikitext itself — zero
		//    extra API calls. Covers de.wiktionary.org "Deklinationsseite"/
		//    "Konjugationsseite" templates and their multilingual equivalents.
		lemma := wikiFlexionLemma(fullText, word)
		// 2. Opensearch fallback for cases where the page is absent or uses an
		//    unknown template style.
		if lemma == "" {
			lemma = wiktionaryLemma(apiBase, word)
		}
		if lemma != "" {
			if lemmaText := fetchFullWikitext(apiBase, lemma); lemmaText != "" {
				fullText = lemmaText
				wr.Meanings = parseDefsFromWikitext(fullText)
				wr.ResolvedWord = lemma
			}
		}
	}

	if fullText == "" {
		return wikiResult{}
	}
	wr.Syns = parseSynsFromWikitext(fullText, lang)
	wt := extractWikiSection(fullText, etymHeadingPat.MatchString)
	wr.Etym = strings.TrimSpace(stripWikitext(wt))
	return wr
}

// ── Renderer ──────────────────────────────────────────────────────────────────

type RenderInput struct {
	Word          string
	ResolvedEN    string // non-empty when input was non-EN and translated to this EN word
	APIFallback   bool   // true when -o was set but pack had no entry and API was used instead
	TargetLangs   []string
	Defn          *Definition
	SynSource     []string
	Etym          string
	Translations  []*Translation
	SynTargets         [][]string
	TargetEntries      []*LexEntry // native target-language entries (pack or wiktionary)
	TargetDefnFallback []string    // GTX-translated EN def per lang, used when native source missed
	TargetEtymFallback []string    // GTX-translated EN etym per lang, used when native source missed
	Warnings           []string
	Elapsed       time.Duration
	FetchLog      []string // per-fetch timing lines
}

func render(in RenderInput) {
	phonetic := ""
	if in.Defn != nil {
		phonetic = in.Defn.Phonetic
	}
	detected := "en"
	for _, t := range in.Translations {
		if t != nil && t.Detected != "" {
			detected = t.Detected
			break
		}
	}
	srcLang := strings.ToUpper(detected)
	langTag := detected
	if len(in.TargetLangs) > 0 {
		uppers := make([]string, len(in.TargetLangs))
		for i, l := range in.TargetLangs {
			uppers[i] = strings.ToUpper(l)
		}
		langTag += " → " + strings.Join(uppers, ", ")
	}

	// ── header ────────────────────────────────────────────────────────────────
	fmt.Println()
	wordDisplay := Bold + CWord + in.Word + R
	if in.ResolvedEN != "" {
		wordDisplay += Dim + " → " + R + Bold + CWord + in.ResolvedEN + R
	}
	// Append target-language translations inline (mirrors the ResolvedEN → style).
	if len(in.Translations) > 0 {
		var parts []string
		for _, t := range in.Translations {
			if t != nil && t.Word != "" {
				parts = append(parts, Bold+CTrans+t.Word+R)
			}
		}
		if len(parts) > 0 {
			wordDisplay += Dim + " → " + R + strings.Join(parts, Dim+" · "+R)
		}
	}
	// Print header; if the full line would exceed dividerWidth, drop [langTag] to the next line
	// so it left-aligns at the same indent rather than wrapping mid-line.
	headerW := 2 + runeLen(stripANSI(wordDisplay)) + 2 + runeLen(phonetic) + 2 + 1 + runeLen(langTag) + 1
	if headerW <= dividerWidth {
		fmt.Printf("  %s  %s%s%s  %s[%s]%s\n", wordDisplay, CEx, phonetic, R, Dim, langTag, R)
	} else {
		fmt.Printf("  %s  %s%s%s\n", wordDisplay, CEx, phonetic, R)
		fmt.Printf("  %s[%s]%s\n", Dim, langTag, R)
	}
	fmt.Println(divider())

	// ── warnings ─────────────────────────────────────────────────────────────
	for _, w := range in.Warnings {
		fmt.Printf("  %s! %s%s\n", CErr, w, R)
	}
	if len(in.Warnings) > 0 {
		fmt.Println()
	}

	// ── definition ────────────────────────────────────────────────────────────
	defnLang := srcLang
	var targetMeanings []Meaning
	hasFallbackDef := len(in.TargetLangs) > 0 && len(in.TargetDefnFallback) > 0 && in.TargetDefnFallback[0] != ""
	if len(in.TargetEntries) > 0 && in.TargetEntries[0] != nil && len(in.TargetEntries[0].Meanings) > 0 {
		targetMeanings = in.TargetEntries[0].Meanings
		defnLang = strings.ToUpper(in.TargetLangs[0])
	} else if hasFallbackDef {
		defnLang = strings.ToUpper(in.TargetLangs[0]) + " ~"
	}
	fmt.Print(sectionHeader(IDef, "DEFINITION ("+defnLang+")"))
	if len(targetMeanings) > 0 {
		for _, m := range targetMeanings {
			fmt.Printf("  %s%s%s\n", CPos+Bold, m.POS, R)
			for i, d := range m.Defs {
				fmt.Println(wordWrap(fmt.Sprintf("%d. %s", i+1, d.Text), dividerWidth-4, "  "))
				if d.Example != "" {
					ex := strings.Join(strings.Fields(d.Example), " ")
					fmt.Printf("%s%s%s\n", CEx, wordWrap("\""+ex+"\"", dividerWidth-4, "  "), R)
				}
				if i < len(m.Defs)-1 {
					fmt.Println()
				}
			}
			fmt.Println()
		}
	} else if hasFallbackDef {
		// No native content — show machine-translated EN text as last resort.
		for _, para := range strings.Split(in.TargetDefnFallback[0], "\n\n") {
			para = strings.TrimSpace(para)
			if para == "" {
				continue
			}
			lines := strings.SplitN(para, "\n", 2)
			if len(lines) == 2 {
				fmt.Printf("  %s%s%s\n", CPos+Bold, strings.TrimSuffix(lines[0], ":"), R)
				fmt.Println(wordWrap(strings.TrimSpace(lines[1]), dividerWidth-4, "  "))
			} else {
				fmt.Println(wordWrap(para, dividerWidth-4, "  "))
			}
			fmt.Println()
		}
	} else if in.Defn != nil && len(in.Defn.Meanings) > 0 {
		for _, m := range in.Defn.Meanings {
			fmt.Printf("  %s%s%s\n", CPos+Bold, m.POS, R)
			for i, d := range m.Defs {
				fmt.Println(wordWrap(fmt.Sprintf("%d. %s", i+1, d.Text), dividerWidth-4, "  "))
				if d.Example != "" {
					// Normalize embedded newlines/whitespace before wrapping.
					ex := strings.Join(strings.Fields(d.Example), " ")
					fmt.Printf("%s%s%s\n", CEx, wordWrap("\""+ex+"\"", dividerWidth-4, "  "), R)
				}
				if i < len(m.Defs)-1 {
					fmt.Println()
				}
			}
			fmt.Println()
		}
	} else {
		fmt.Printf("  %s(no definition found — try a different form)%s\n\n", CEx, R)
	}

	// ── target-language synonyms (one section per lang that has results) ──────
	for i, lang := range in.TargetLangs {
		if i >= len(in.SynTargets) {
			break
		}
		syns := in.SynTargets[i]
		if len(syns) == 0 {
			continue
		}
		if len(syns) > 12 {
			syns = syns[:12]
		}
		fmt.Print(sectionHeader(ISyn, "SYNONYMS ("+strings.ToUpper(lang)+")"))
		fmt.Printf("%s%s%s\n\n", CSyn, wordWrap(strings.Join(syns, " · "), dividerWidth-2, "  "), R)
	}

	// ── synonyms (source language) ────────────────────────────────────────────
	allSyn := appendUniq(nil, in.SynSource...)
	if in.Defn != nil {
		for _, m := range in.Defn.Meanings {
			allSyn = appendUniq(allSyn, m.Syns...)
		}
	}
	if len(allSyn) > 12 {
		allSyn = allSyn[:12]
	}
	if len(allSyn) > 0 {
		fmt.Print(sectionHeader(ISyn, "SYNONYMS ("+srcLang+")"))
		fmt.Printf("%s%s%s\n\n", CSyn, wordWrap(strings.Join(allSyn, " · "), dividerWidth-2, "  "), R)
	}

	// ── etymology ─────────────────────────────────────────────────────────────
	etymText := in.Etym
	etymLang := srcLang
	if len(in.TargetEntries) > 0 && in.TargetEntries[0] != nil && in.TargetEntries[0].Etym != "" {
		etymText = in.TargetEntries[0].Etym
		etymLang = strings.ToUpper(in.TargetLangs[0])
	} else if len(in.TargetEtymFallback) > 0 && in.TargetEtymFallback[0] != "" {
		etymText = in.TargetEtymFallback[0]
		etymLang = strings.ToUpper(in.TargetLangs[0]) + " ~"
	}
	if etymText != "" {
		fmt.Print(sectionHeader(IEty, "ETYMOLOGY ("+etymLang+")"))
		fmt.Println(wordWrap(etymText, dividerWidth-4, "  "))
		fmt.Println()
	}

	fallbackNote := ""
	if in.APIFallback {
		fallbackNote = fmt.Sprintf("  %s⚠ pack miss → api%s", CErr, R)
	}

	// ── fine print: absent sections ───────────────────────────────────────────
	var absent []string
	// note missing target definition only if both native and GTX fallback failed.
	for i, lang := range in.TargetLangs {
		hasTargetDef := i < len(in.TargetEntries) && in.TargetEntries[i] != nil && len(in.TargetEntries[i].Meanings) > 0
		hasFallback := i < len(in.TargetDefnFallback) && in.TargetDefnFallback[i] != ""
		if !hasTargetDef && !hasFallback {
			absent = append(absent, "no definition ("+strings.ToUpper(lang)+")")
		}
	}
	if len(allSyn) == 0 {
		absent = append(absent, "no synonyms ("+srcLang+")")
	}
	for i, lang := range in.TargetLangs {
		if i >= len(in.SynTargets) || len(in.SynTargets[i]) == 0 {
			absent = append(absent, "no synonyms ("+strings.ToUpper(lang)+")")
		}
	}
	if etymText == "" {
		absent = append(absent, "no etymology")
	}
	if len(absent) > 0 {
		fmt.Printf("  %s%s%s\n", Dim, strings.Join(absent, " · "), R)
	}

	fmt.Printf("  %sfetched in %dms%s%s\n", Dim, in.Elapsed.Milliseconds(), R, fallbackNote)
	if len(in.FetchLog) > 0 {
		for _, line := range in.FetchLog {
			fmt.Printf("  %s%s%s\n", Dim, line, R)
		}
	}
	fmt.Println(divider())
	fmt.Println()
}

// ── Help ──────────────────────────────────────────────────────────────────────

func printHelp() {
	art := []string{
		`  ██╗     ███████╗██╗  ██╗██╗███████╗██╗   ██╗`,
		`  ██║     ██╔════╝╚██╗██╔╝██║██╔════╝╚██╗ ██╔╝`,
		`  ██║     █████╗   ╚███╔╝ ██║█████╗   ╚████╔╝ `,
		`  ██║     ██╔══╝   ██╔██╗ ██║██╔══╝    ╚██╔╝  `,
		`  ███████╗███████╗██╔╝ ██╗██║██║        ██║   `,
		`  ╚══════╝╚══════╝╚═╝  ╚═╝╚═╝╚═╝        ╚═╝   `,
	}
	fmt.Println()
	for _, line := range art {
		fmt.Printf("%s%s%s%s\n", CWord, Bold, line, R)
	}
	fmt.Printf("  %sword lookup: definition · synonyms · etymology · translation%s\n", CEx, R)
	fmt.Println(divider())
	fmt.Println()

	fmt.Printf("  %s%sUSAGE%s\n", CHead, Bold, R)
	fmt.Printf("  %slexify%s %s<word>%s\n", CPos, R, CSyn, R)
	fmt.Printf("  %slexify%s %s<word>%s %s<lang>%s\n", CPos, R, CSyn, R, CTrans, R)
	fmt.Printf("  %slexify%s %s<word>%s %s<lang> <lang>%s %s...%s\n\n", CPos, R, CSyn, R, CTrans, R, CEx, R)

	fmt.Printf("  %s%sFLAGS%s\n", CHead, Bold, R)
	fmt.Printf("  %s-i <lang> [lang ...]%s  install offline pack  (e.g. lexify -i en de ru)\n", CPos, R)
	fmt.Printf("  %s  --kaikki%s  source: kaikki.org JSONL  ~200–500 MB  %s(default; native editions for de fr es it pt ru ja zh ko nl pl tr)%s\n", CPos, R, Dim, R)
	fmt.Printf("  %s  --wiki%s   source: en.wiktionary.org XML dump  ~1.2 GB, ~10 min\n", CPos, R)
	fmt.Printf("  %s  --force%s  reinstall even if pack is already up to date\n", CPos, R)
	fmt.Printf("  %s-o%s         force live API (skip installed pack)\n", CPos, R)
	fmt.Printf("  %s-d%s         show per-fetch debug timing\n\n", CPos, R)

	fmt.Printf("  %s%sEXAMPLES%s\n", CHead, Bold, R)
	examples := [][2]string{
		{"lexify serendipity", "English definition, synonyms, etymology"},
		{"lexify serendipity fr", "+ translation to French, content in French"},
		{"lexify serendipity fr ru", "+ translations to French and Russian"},
		{"lexify Schadenfreude en", "non-English source word, translate to English"},
	}
	for _, ex := range examples {
		fmt.Printf("  %s%s%s\n", CPos, ex[0], R)
		fmt.Printf("    %s→ %s%s\n\n", CEx, ex[1], R)
	}

	fmt.Printf("  %s%sLANGUAGES%s\n", CHead, Bold, R)
	fmt.Println(wordWrap("Pass any BCP-47 code: fr  ru  de  es  it  pt  ja  zh  ko  ar  nl  pl  sv  tr  uk  hi  …", dividerWidth-4, "  "))
	fmt.Println()

	fmt.Printf("  %s%sDATA SOURCES%s\n", CHead, Bold, R)
	sources := [][2]string{
		{IDef + "Definition", "en.wiktionary.org (offline pack)  ·  dictionaryapi.dev (live API)"},
		{ISyn + "Synonyms", "en.wiktionary.org (offline pack)  ·  Wiktionary API (target langs, live)"},
		{IEty + "Etymology", "en.wiktionary.org (offline pack)  ·  Wiktionary API (live)"},
		{ITrans + "Translation", "Google Translate gtx  ·  MyMemory fallback  (no key)"},
	}
	for _, s := range sources {
		fmt.Printf("  %s%s%s%s\n", CHead, Bold, s[0], R)
		fmt.Println(wordWrap(s[1], dividerWidth-4, "    "))
		fmt.Println()
	}
	fmt.Println(divider())
	fmt.Println(wordWrap("uses installed pack automatically · -o forces live API · translations fire in parallel · no API keys", dividerWidth-4, "  "))
	fmt.Println()
}

// ── Utilities ─────────────────────────────────────────────────────────────────

// normalizePOS maps abbreviated and variant POS labels (from kaikki JSONL) to their
// full English equivalents used by the API, so both sources render identically.
var posNorm = map[string]string{
	"adj":         "adjective",
	"adv":         "adverb",
	"n":           "noun",
	"v":           "verb",
	"prep":        "preposition",
	"pron":        "pronoun",
	"conj":        "conjunction",
	"interj":      "interjection",
	"num":         "numeral",
	"det":         "determiner",
	"abbrev":      "abbreviation",
	"name":        "proper noun",
	"particle":    "particle",
	"phrase":      "phrase",
	"prefix":      "prefix",
	"suffix":      "suffix",
	"affix":       "affix",
	"punct":       "punctuation",
	"character":   "character",
	"symbol":      "symbol",
	"contraction": "contraction",
	"proverb":     "proverb",
}

func normalizePOS(pos string) string {
	l := strings.ToLower(strings.TrimSpace(pos))
	if full, ok := posNorm[l]; ok {
		return full
	}
	return l
}

// ── Orchestration helpers ─────────────────────────────────────────────────────

// fetchTimingLog accumulates per-fetch timing rows for the -d debug output.
// section() sets a pending header that is flushed lazily before the next row,
// so empty sections produce no output at all.
type fetchTimingLog struct {
	lines      []string
	pendingHdr string
}

func (f *fetchTimingLog) section(phase string, total time.Duration) {
	f.pendingHdr = fmt.Sprintf("├─ %s (%dms) %s", phase, total.Milliseconds(),
		strings.Repeat("─", max(0, 27-len(phase)-len(fmt.Sprintf("(%dms)", total.Milliseconds())))))
}

func (f *fetchTimingLog) add(label string, ms int64, src string) {
	if f.pendingHdr != "" {
		f.lines = append(f.lines, f.pendingHdr)
		f.pendingHdr = ""
	}
	srcStr := ""
	if src != "" {
		srcStr = "  " + src
	}
	f.lines = append(f.lines, fmt.Sprintf("│  %-12s %5dms%s", label, ms, srcStr))
}

// build wraps accumulated rows in box-drawing borders, or returns nil if empty.
func (f *fetchTimingLog) build() []string {
	if len(f.lines) == 0 {
		return nil
	}
	out := make([]string, 0, len(f.lines)+2)
	out = append(out, "┌"+strings.Repeat("─", 33))
	out = append(out, f.lines...)
	out = append(out, "└"+strings.Repeat("─", 33))
	return out
}

// resolveState holds all results and timing produced by the Phase-1 EN-word
// resolution path, so run() can read them cleanly after wg1.Wait().
type resolveState struct {
	entry        *LexEntry
	apiDefn      *Definition
	apiEtym      string
	resolvedEN   string
	apiFallback  bool
	p1XDGHit     bool
	resolveInP1  bool
	tEmbed       time.Duration
	tAPI         time.Duration
	tAPIEtym     time.Duration
	tResolve     time.Duration
	tResolveXDG  time.Duration
	tResolveDefn time.Duration
	tP1Total     time.Duration
}

// resolveWordXDGOrAPI tries the XDG pack first (when useXDG) and falls back to
// the network API on a miss. Covers both the pack-enabled and API-only paths.
func resolveWordXDGOrAPI(word string, useXDG bool) (rs resolveState) {
	if useXDG {
		t := time.Now()
		rs.entry = lookupEN(word)
		rs.tEmbed = time.Since(t)
		if rs.entry != nil {
			rs.p1XDGHit = true
		} else {
			rs.apiDefn, rs.apiEtym, rs.tAPI, rs.tAPIEtym = fetchDefnAndEtymPar(word)
			if rs.apiDefn != nil {
				rs.apiFallback = true
			}
		}
	} else {
		rs.apiDefn, rs.apiEtym, rs.tAPI, rs.tAPIEtym = fetchDefnAndEtymPar(word)
	}
	return
}

// resolveNonENWord handles Phase-1 when the user flagged the input as non-English.
// It translates the word → English via GTX, then looks up the English form in XDG/API.
func resolveNonENWord(word string, useXDG bool) (rs resolveState) {
	tStart := time.Now()
	t := time.Now()
	enTrans, _ := fetchGTX(word, "en")
	rs.tResolve = time.Since(t)
	if enTrans != nil {
		rs.resolveInP1 = true
		rs.resolvedEN = enTrans.Word
		if useXDG {
			t2 := time.Now()
			if e := lookupEN(enTrans.Word); e != nil {
				rs.tResolveXDG = time.Since(t2)
				rs.entry = e
				rs.p1XDGHit = true
			} else {
				rs.tResolveXDG = time.Since(t2)
				rs.apiDefn, rs.apiEtym, rs.tResolveDefn, _ = fetchDefnAndEtymPar(enTrans.Word)
				if rs.apiDefn != nil {
					rs.apiFallback = true
				}
			}
		} else {
			// apiOnly or no pack: query API for the resolved EN word directly.
			rs.apiDefn, rs.apiEtym, rs.tResolveDefn, _ = fetchDefnAndEtymPar(enTrans.Word)
		}
	} else {
		// GTX returned nil — the word is already English; fall back to direct lookup.
		rs = resolveWordXDGOrAPI(word, useXDG)
	}
	rs.tP1Total = time.Since(tStart)
	return
}

// debugTimingParams bundles all data needed by buildFetchLog.
type debugTimingParams struct {
	// Phase-1 decision flags.
	hintNonEN   bool
	useXDG      bool
	p1XDGHit    bool
	resolveInP1 bool
	// Phase-1 timings.
	tEmbed            time.Duration
	tAPI              time.Duration
	tAPIEtym          time.Duration
	tResolve          time.Duration
	tResolveXDG       time.Duration
	tResolveDefn      time.Duration
	tP1GoroutineTotal time.Duration
	// Per-lang timings.
	tWordTrans          []time.Duration
	tSynTargets         []time.Duration
	tPackLookup         []time.Duration
	tWikiFetch          []time.Duration
	tTargetDefnFallback []time.Duration
	tTargetEtymFallback []time.Duration
	// Per-lang results (for source-label logic).
	translateLangs     []string
	wordTrans          []*Translation
	etym               string
	targetEntries      []*LexEntry
	wikiResults        []wikiResult
	targetDefnFallback []string
	targetEtymFallback []string
	synPackHits        []bool
}

// buildFetchLog produces the -d debug timing table from collected timing data.
func buildFetchLog(p debugTimingParams) []string {
	maxDur := func(ds ...time.Duration) time.Duration {
		var m time.Duration
		for _, d := range ds {
			if d > m {
				m = d
			}
		}
		return m
	}
	tl := &fetchTimingLog{}

	// Phase 1 wall time = max of all parallel tasks in wg1.
	var p1Dur []time.Duration
	if p.hintNonEN {
		p1Dur = append(p1Dur, p.tP1GoroutineTotal)
	} else if p.useXDG {
		p1Dur = append(p1Dur, p.tEmbed)
		if !p.p1XDGHit {
			p1Dur = append(p1Dur, p.tAPI, p.tAPIEtym)
		}
	} else {
		p1Dur = append(p1Dur, p.tAPI, p.tAPIEtym)
	}
	for _, d := range p.tWordTrans {
		p1Dur = append(p1Dur, d)
	}
	p1Total := maxDur(p1Dur...)

	// Phase 2 wall time = max of wg2 goroutines + sequential GTX~ fallback after wg2.
	var p2Dur []time.Duration
	for _, d := range p.tSynTargets {
		p2Dur = append(p2Dur, d)
	}
	p2GoroutineTotal := maxDur(p2Dur...)
	// GTX~ fallbacks run in parallel with each other but sequentially after wg2.
	// Per-lang cost = defn fallback + etym fallback (they run concurrently per lang
	// inside wgFallback, so take the max across langs).
	var p2FallbackDur []time.Duration
	for i := range p.tTargetDefnFallback {
		d := maxDur(p.tTargetDefnFallback[i], p.tTargetEtymFallback[i])
		p2FallbackDur = append(p2FallbackDur, d)
	}
	p2Total := p2GoroutineTotal + maxDur(p2FallbackDur...)

	// ── Phase 1 rows ─────────────────────────────────────────────────────────
	tl.section("phase 1", p1Total)
	for i, lang := range p.translateLangs {
		src := ""
		if p.wordTrans[i] != nil {
			src = p.wordTrans[i].Source
		}
		tl.add("trans("+lang+")", p.tWordTrans[i].Milliseconds(), src)
	}
	if !p.resolveInP1 {
		if p.p1XDGHit {
			tl.add("syns(en)", p.tEmbed.Milliseconds(), "xdg")
		} else if p.useXDG {
			tl.add("syns(en)", p.tEmbed.Milliseconds(), "xdg miss")
			if p.tAPI > 0 {
				tl.add("defn", p.tAPI.Milliseconds(), "api")
			}
			if p.etym != "" && p.tAPIEtym > 0 {
				tl.add("etym", p.tAPIEtym.Milliseconds(), "api")
			}
		} else {
			tl.add("defn", p.tAPI.Milliseconds(), "api")
			if p.etym != "" {
				tl.add("etym", p.tAPIEtym.Milliseconds(), "api")
			}
		}
	}

	// ── Resolve rows (non-EN source word routed through GTX → EN) ────────────
	if p.tResolve > 0 {
		resolveTotal := p.tResolve + p.tResolveXDG + p.tResolveDefn
		tl.section("resolve", resolveTotal)
		tl.add("gtx→en", p.tResolve.Milliseconds(), "gtx")
		if p.tResolveXDG > 0 {
			tl.add("syns(en)", p.tResolveXDG.Milliseconds(), "xdg")
		}
		if p.tResolveDefn > 0 {
			tl.add("defn", p.tResolveDefn.Milliseconds(), "api")
		}
	}

	// ── Phase 2 rows ─────────────────────────────────────────────────────────
	tl.section("phase 2", p2Total)
	for i, lang := range p.translateLangs {
		hasEntry := i < len(p.targetEntries) && p.targetEntries[i] != nil
		if hasEntry {
			e := p.targetEntries[i]
			// Definition source label and timing.
			var defnSrc string
			defnMs := p.tSynTargets[i].Milliseconds()
			if i < len(p.tPackLookup) && p.tPackLookup[i] > 0 {
				defnMs = p.tPackLookup[i].Milliseconds()
			}
			if lang == "en" {
				defnSrc = "xdg"
				if len(e.Meanings) == 0 {
					defnSrc = "xdg miss"
				}
			} else {
				hasWikiMeanings := i < len(p.wikiResults) && len(p.wikiResults[i].Meanings) > 0
				if isNativeLangPack(lang) && len(e.Meanings) > 0 {
					// native pack meanings always take priority over wiki
					defnSrc = "xdg"
				} else if hasWikiMeanings {
					defnSrc = "wiki"
				} else if len(e.Meanings) > 0 {
					// wiki was skipped or missed — meanings came from pack
					defnSrc = "xdg"
				} else if i < len(p.targetDefnFallback) && p.targetDefnFallback[i] != "" {
					defnSrc = "wiki miss→gtx~"
					defnMs = p.tTargetDefnFallback[i].Milliseconds()
				} else {
					defnSrc = "wiki miss"
				}
			}
			// Etymology source label and timing.
			// tWikiFetch covers the wiki round-trip (triggered when pack had no
			// etym). tTargetEtymFallback covers the GTX~ call that runs after
			// wg2 when wiki also missed. Both costs are attributed to etym since
			// the missing etym triggered the wiki call in the first place.
			var etymSrc string
			wikiMs := int64(0)
			if i < len(p.tWikiFetch) && p.tWikiFetch[i] > 0 {
				wikiMs = p.tWikiFetch[i].Milliseconds()
			}
			etymMs := int64(0)
			etymFromWiki := i < len(p.wikiResults) && p.wikiResults[i].EtymFromWiki
			if e.Etym != "" && !etymFromWiki {
				etymSrc = "xdg"
			} else if etymFromWiki {
				etymSrc = "wiki"
				etymMs = wikiMs
			} else if i < len(p.targetEtymFallback) && p.targetEtymFallback[i] != "" {
				if lang == "en" {
					etymSrc = "xdg miss→gtx~"
				} else {
					etymSrc = "wiki miss→gtx~"
				}
				etymMs = wikiMs + p.tTargetEtymFallback[i].Milliseconds()
			} else {
				if lang == "en" {
					etymSrc = "xdg miss"
				} else {
					etymSrc = "wiki miss"
				}
				etymMs = wikiMs
			}
			tl.add("defn("+lang+")", defnMs, defnSrc)
			tl.add("etym("+lang+")", etymMs, etymSrc)
		} else {
			if i < len(p.targetDefnFallback) && p.targetDefnFallback[i] != "" {
				tl.add("defn("+lang+")", p.tTargetDefnFallback[i].Milliseconds(), "gtx~")
			}
			if i < len(p.targetEtymFallback) && p.targetEtymFallback[i] != "" {
				tl.add("etym("+lang+")", p.tTargetEtymFallback[i].Milliseconds(), "gtx~")
			}
		}
		if p.tSynTargets[i] > 0 || p.synPackHits[i] {
			synSrc := "api"
			if p.synPackHits[i] {
				synSrc = "xdg"
			} else if lang != "en" {
				synSrc = "wiki"
				if !hasEntry {
					synSrc = "api"
				}
			}
			tl.add("syns("+lang+")", 0, synSrc)
		}
	}

	return tl.build()
}

// ── Orchestration ─────────────────────────────────────────────────────────────

func run(word string, translateLangs []string, debug, apiOnly, hintNonEN bool) {
	start := time.Now()
	fmt.Printf("\n  %slooking up %s%s%s%s…%s\r", CEx, Bold, word, R, CEx, R)

	// ── Phase 1: parallel — EN-word resolution + target-language translations ─
	var (
		defn      *Definition
		synSource []string
		etym      string
		wordTrans  = make([]*Translation, len(translateLangs))
		tWordTrans = make([]time.Duration, len(translateLangs))
	)
	// rs collects all Phase-1 resolution results (entry, timings, flags).
	var rs resolveState

	// synTargets and wg2 are declared here so trans goroutines can chain
	// syns lookups immediately after the translation result arrives,
	// overlapping with defn/etym fetches in Phase 1.
	var (
		synTargets    = make([][]string, len(translateLangs))
		tSynTargets   = make([]time.Duration, len(translateLangs))
		tPackLookup   = make([]time.Duration, len(translateLangs))
		tWikiFetch    = make([]time.Duration, len(translateLangs))
		synPackHits   = make([]bool, len(translateLangs))
		targetEntries = make([]*LexEntry, len(translateLangs))
		wikiResults   = make([]wikiResult, len(translateLangs))
	)
	var wg2 sync.WaitGroup

	useXDG := !apiOnly && enProvider != nil

	var wg1 sync.WaitGroup
	wg1.Add(len(translateLangs) + 1) // +1 for the Phase-1 EN-word resolution goroutine
	if hintNonEN {
		// User declared the word is non-EN (e.g. `lexify einheitlich en`).
		// Resolve via GTX in Phase 1, parallel with any translation goroutines.
		go func() { defer wg1.Done(); rs = resolveNonENWord(word, useXDG) }()
	} else {
		go func() { defer wg1.Done(); rs = resolveWordXDGOrAPI(word, useXDG) }()
	}
	for i, lang := range translateLangs {
		i, lang := i, lang
		go func() {
			defer wg1.Done()
			t := time.Now()
			wordTrans[i] = fetchTranslation(word, lang)
			tWordTrans[i] = time.Since(t)
			// Fire target-language syns immediately — overlaps with defn/etym.
			// If an installed pack exists for this lang, query it directly;
			// otherwise fall back to the Wiktionary API scrape.
			if wordTrans[i] != nil && wordTrans[i].Word != "" {
				wg2.Add(1)
				go func() {
					defer wg2.Done()
					t2 := time.Now()
					translated := wordTrans[i].Word

					// Pack lookup: provides IPA, syns, etym, and (for native-edition
					// packs) target-language definitions and etymology.
					tPack := time.Now()
					packEntry := lookupLang(translated, lang)
					tPackLookup[i] = time.Since(tPack)

					// For non-EN target langs, fetch native definitions, synonyms, and
					// etymology from lang.wiktionary.org — but only when the pack does
					// not already provide target-language content. Native-edition packs
					// (installed from lang.wiktionary.org kaikki dumps) have meanings
					// and etymology in the target language; skip the API call when they
					// hit.
					var wr wikiResult
					if lang != "en" {
						needsWiki := !isNativeLangPack(lang) ||
							packEntry == nil ||
							len(packEntry.Meanings) == 0 ||
							packEntry.Etym == ""
						if needsWiki {
							tWiki := time.Now()
							wr = fetchTargetWikiData(translated, lang)
							tWikiFetch[i] = time.Since(tWiki)
							// If wiki resolved an inflected form to its lemma, retry the
							// pack lookup with the lemma so XDG syns/etym/IPA aren't lost.
							if packEntry == nil && wr.ResolvedWord != "" {
								packEntry = lookupLang(wr.ResolvedWord, lang)
							}
						}
					}

					// Build synthetic entry.
					// IPA and syns from pack are language-neutral so always preferred.
					// For etym: native-edition packs (isNativeLangPack) have etymology_text
					// in the target language; en.wiktionary.org-sourced packs have English.
					// Use pack etym only when it is in the right language.
					synthetic := &LexEntry{}
					if packEntry != nil {
						synthetic.IPA = packEntry.IPA
						if lang == "en" || isNativeLangPack(lang) {
							synthetic.Etym = packEntry.Etym
						}
						if len(packEntry.Syns) > 0 {
							synthetic.Syns = packEntry.Syns
							synTargets[i] = packEntry.Syns
							synPackHits[i] = true
						}
					}
					if lang != "en" {
						// For native-edition packs, pack meanings take priority: they are
						// sourced from the native Wiktionary and include examples, multiple
						// senses, etc. Wiki meanings are only used when the pack has none.
						// For non-native packs, wiki meanings are preferred (pack glosses
						// are in English, which is useless for target-lang display).
						if isNativeLangPack(lang) && packEntry != nil && len(packEntry.Meanings) > 0 {
							synthetic.Meanings = packEntry.Meanings
						} else if len(wr.Meanings) > 0 {
							synthetic.Meanings = wr.Meanings
						} else if packEntry != nil {
							synthetic.Meanings = packEntry.Meanings
						}
						// For etymology: prefer pack etym (already set above for native
						// editions). Fall back to wiki etym when pack has none; gated
						// on minEtymRunes to discard unresolvable stubs (e.g. RU
						// {{этимология:бить|да}} templates that survive stripping as
						// bare fragments like "Происходит от"). When both miss, the
						// GTX~ fallback fires in the next pass.
						if synthetic.Etym == "" {
							if runeLen(wr.Etym) >= minEtymRunes {
								synthetic.Etym = wr.Etym
								wr.EtymFromWiki = true
							}
						}
						if !synPackHits[i] && len(wr.Syns) > 0 {
							synTargets[i] = wr.Syns
						}
					} else if packEntry != nil {
						synthetic.Meanings = packEntry.Meanings
					}
					wikiResults[i] = wr

					if packEntry != nil || len(wr.Meanings) > 0 {
						targetEntries[i] = synthetic
					}
					tSynTargets[i] = time.Since(t2)
				}()
			}
		}()
	}
	wg1.Wait()

	// Populate defn/synSource/etym from Phase-1 results.
	if rs.entry != nil {
		defn = &Definition{Phonetic: rs.entry.IPA, Meanings: rs.entry.Meanings}
		synSource = rs.entry.Syns
		etym = rs.entry.Etym
	} else {
		if rs.apiDefn != nil {
			defn = rs.apiDefn
		}
		etym = rs.apiEtym
	}

	// Non-EN source word routing: if lookup missed, try treating the input as a
	// non-English word. fetchGTX returns nil when the translation equals the
	// input (i.e. the word is already English), so this is safe to call
	// unconditionally — no detectedSrc guard needed. This also covers the case
	// where the only target lang is "en" (fetchTranslation short-circuits to nil
	// for that lang, so wordTrans[0] would be nil and Detected would be unavailable).
	if rs.entry == nil && defn == nil && rs.resolvedEN == "" {
		t := time.Now()
		enTrans, _ := fetchGTX(word, "en")
		rs.tResolve = time.Since(t)
		if enTrans != nil {
			rs.resolvedEN = enTrans.Word
			if !apiOnly && enProvider != nil {
				t2 := time.Now()
				if e := lookupEN(enTrans.Word); e != nil {
					rs.tResolveXDG = time.Since(t2)
					rs.entry = e
					defn = &Definition{Phonetic: e.IPA, Meanings: e.Meanings}
					synSource = e.Syns
					etym = e.Etym
				} else {
					rs.tResolveXDG = time.Since(t2)
				}
			}
			if rs.entry == nil {
				t3 := time.Now()
				if d := fetchDefinition(enTrans.Word); d != nil {
					rs.tResolveDefn = time.Since(t3)
					defn = d
					if !apiOnly && enProvider != nil {
						rs.apiFallback = true
					}
				} else {
					rs.tResolveDefn = time.Since(t3)
				}
			}
		}
	}

	var warnings []string
	var validIdx []int
	{
		var validLangs []string
		var validTrans []*Translation
		for i, lang := range translateLangs {
			if wordTrans[i] == nil {
				warnings = append(warnings, fmt.Sprintf("language %q not recognised — falling back to English", lang))
			} else {
				validIdx = append(validIdx, i)
				validLangs = append(validLangs, lang)
				validTrans = append(validTrans, wordTrans[i])
			}
		}
		translateLangs = validLangs
		wordTrans = validTrans
	}

	// ── Phase 2: wait for target-language goroutines ─────────────────────────
	// synTargets/targetEntries goroutines were already launched inside Phase-1
	// translation goroutines; wg2 collects them here.
	wg2.Wait()

	// Remap synTargets, targetEntries, and wikiResults to match the (possibly filtered) translateLangs slice.
	{
		mapped := make([][]string, len(validIdx))
		mappedT := make([]time.Duration, len(validIdx))
		mappedPack := make([]time.Duration, len(validIdx))
		mappedWiki := make([]time.Duration, len(validIdx))
		mappedHits := make([]bool, len(validIdx))
		mappedEntries := make([]*LexEntry, len(validIdx))
		mappedWikiRes := make([]wikiResult, len(validIdx))
		for j, idx := range validIdx {
			mapped[j] = synTargets[idx]
			mappedT[j] = tSynTargets[idx]
			mappedPack[j] = tPackLookup[idx]
			mappedWiki[j] = tWikiFetch[idx]
			mappedHits[j] = synPackHits[idx]
			mappedEntries[j] = targetEntries[idx]
			mappedWikiRes[j] = wikiResults[idx]
		}
		synTargets = mapped
		tSynTargets = mappedT
		tPackLookup = mappedPack
		tWikiFetch = mappedWiki
		synPackHits = mappedHits
		targetEntries = mappedEntries
		wikiResults = mappedWikiRes
	}

	// GTX fallback: for any target lang where neither wiki nor pack provided native
	// definitions/etym, machine-translate the EN content as a last resort.
	n := len(translateLangs)
	targetDefnFallback := make([]string, n)
	targetEtymFallback := make([]string, n)
	tTargetDefnFallback := make([]time.Duration, n)
	tTargetEtymFallback := make([]time.Duration, n)
	{
		defBlock := ""
		if defn != nil && len(defn.Meanings) > 0 {
			var parts []string
			for _, m := range defn.Meanings {
				var defs []string
				for _, d := range m.Defs {
					if len(defs) < 2 {
						defs = append(defs, "  "+d.Text)
					}
				}
				parts = append(parts, m.POS+":\n"+strings.Join(defs, "\n"))
			}
			defBlock = strings.Join(parts, "\n\n")
		}
		var wgFallback sync.WaitGroup
		for i, lang := range translateLangs {
			i, lang := i, lang
			needsDef := !(i < len(targetEntries) && targetEntries[i] != nil && len(targetEntries[i].Meanings) > 0)
			needsEtym := etym != "" && !(i < len(targetEntries) && targetEntries[i] != nil && targetEntries[i].Etym != "")
			if needsDef && defBlock != "" {
				wgFallback.Add(1)
				go func() {
					defer wgFallback.Done()
					t := time.Now()
					targetDefnFallback[i] = fetchTextTranslation(defBlock, lang)
					tTargetDefnFallback[i] = time.Since(t)
				}()
			}
			if needsEtym {
				wgFallback.Add(1)
				go func() {
					defer wgFallback.Done()
					t := time.Now()
					targetEtymFallback[i] = fetchTextTranslation(etym, lang)
					tTargetEtymFallback[i] = time.Since(t)
				}()
			}
		}
		wgFallback.Wait()
	}

	// Clear the "looking up…" line.
	// The spinner was printed as "\n  looking up...\r" which leaves cursor at
	// col 0 of the spinner line. \033[2K erases that line, then \033[1A moves up
	// to the blank line the leading \n created. Without the \033[2K the header
	// would overwrite the spinner text, leaving trailing chars visible (e.g. “ncy…”).
	fmt.Printf("\r\033[2K\033[1A")

	var fetchLog []string
	if debug {
		fetchLog = buildFetchLog(debugTimingParams{
			hintNonEN:           hintNonEN,
			useXDG:              useXDG,
			p1XDGHit:            rs.p1XDGHit,
			resolveInP1:         rs.resolveInP1,
			tEmbed:              rs.tEmbed,
			tAPI:                rs.tAPI,
			tAPIEtym:            rs.tAPIEtym,
			tResolve:            rs.tResolve,
			tResolveXDG:         rs.tResolveXDG,
			tResolveDefn:        rs.tResolveDefn,
			tP1GoroutineTotal:   rs.tP1Total,
			tWordTrans:          tWordTrans,
			tSynTargets:         tSynTargets,
			tPackLookup:         tPackLookup,
			tWikiFetch:          tWikiFetch,
			tTargetDefnFallback: tTargetDefnFallback,
			tTargetEtymFallback: tTargetEtymFallback,
			translateLangs:      translateLangs,
			wordTrans:           wordTrans,
			etym:                etym,
			targetEntries:       targetEntries,
			wikiResults:         wikiResults,
			targetDefnFallback:  targetDefnFallback,
			targetEtymFallback:  targetEtymFallback,
			synPackHits:         synPackHits,
		})
	}

	render(RenderInput{
		Word:               word,
		ResolvedEN:         rs.resolvedEN,
		APIFallback:        rs.apiFallback,
		TargetLangs:        translateLangs,
		Defn:               defn,
		SynSource:          synSource,
		Etym:               etym,
		Translations:       wordTrans,
		SynTargets:         synTargets,
		TargetEntries:      targetEntries,
		TargetDefnFallback: targetDefnFallback,
		TargetEtymFallback: targetEtymFallback,
		Warnings:           warnings,
		Elapsed:            time.Since(start),
		FetchLog:           fetchLog,
	})
}

// ── Progress bar (stdlib-only, terminal-width-safe) ─────────────────────────────────────────────

var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// installProgress is a terminal-width-aware progress bar for the install subcommand.
// It writes a single overwriting line via "\r\033[2K" and caps the bar to
// dividerWidth columns so the line never wraps — eliminating the newline-spam
// that occurs when \r only returns to the start of the current visual row.
type installProgress struct {
	desc    string
	total   int64 // -1 = indeterminate
	written int64
	start   time.Time
	last    time.Time
	frame   int
	mu      sync.Mutex
}

func newProgress(total int64, desc string) *installProgress {
	p := &installProgress{
		desc:  desc,
		total: total,
		start: time.Now(),
		last:  time.Now().Add(-100 * time.Millisecond), // force first render
	}
	p.render()
	return p
}

func (p *installProgress) render() {
	elapsed := time.Since(p.start).Seconds()
	speed := float64(0)
	if elapsed > 0.001 {
		speed = float64(p.written) / elapsed
	}
	desc := fmt.Sprintf("%-13s", p.desc)
	suffix := fmt.Sprintf("  %s  %s/s", fmtBytes(p.written), fmtBytes(int64(speed)))

	var bar string
	if p.total > 0 {
		pct := float64(p.written) / float64(p.total)
		if pct > 1 {
			pct = 1
		}
		pctStr := fmt.Sprintf("  %3.0f%%", pct*100)
		suffix = pctStr + suffix
		// "  " prefix(2) + desc(13) + " "(1) + bar(barW) + suffix — total ≤ dividerWidth+2
		barW := dividerWidth - 16 - runeLen(suffix)
		if barW < 6 {
			barW = 6
		}
		filled := int(pct * float64(barW))
		bar = " " + CBar + strings.Repeat("█", filled) + R + Dim + strings.Repeat("░", barW-filled) + R
	} else {
		// indeterminate: spinner only
		bar = " " + CBar + spinnerFrames[p.frame%len(spinnerFrames)] + R
	}

	line := fmt.Sprintf("  %s%s%s", desc, bar, suffix)
	// Hard-clamp to terminal width as a last resort against wrapping.
	// Strip ANSI codes before measuring so invisible escape bytes don't count.
	visible := []rune(ansiRe.ReplaceAllString(line, ""))
	if len(visible) > dividerWidth+2 {
		// Rebuild line truncated to fit: strip then re-add colours is complex,
		// so just reuse the plain clamped visible text when overflow occurs.
		line = string(visible[:dividerWidth+2])
	}
	fmt.Fprintf(os.Stderr, "\r\033[2K%s", line)
}

func (p *installProgress) Write(b []byte) (int, error) {
	n := len(b)
	p.mu.Lock()
	p.written += int64(n)
	now := time.Now()
	if now.Sub(p.last) >= 80*time.Millisecond {
		p.last = now
		p.frame++
		p.render()
	}
	p.mu.Unlock()
	return n, nil
}

func (p *installProgress) Finish() {
	p.mu.Lock()
	p.render()
	p.mu.Unlock()
	fmt.Fprintln(os.Stderr)
}

// progressReader wraps an io.Reader, reporting bytes read to an installProgress.
type progressReader struct {
	r    io.Reader
	prog *installProgress
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.prog.Write(b[:n]) //nolint:errcheck
	}
	return n, err
}

// ── Install subcommand  (-i / --install) ────────────────────────────────────────────────────────

// cmdInstall downloads and indexes a language pack for offline use.
// source is "kaikki" (default, fast ~500 MB JSONL) or "wiki" (~1.2 GB XML dump).
func cmdInstall(lang, source string, force bool) {
	langName, ok := langNames[lang]
	if !ok {
		fmt.Fprintf(os.Stderr, "lexify -i: unsupported language %q\n  supported: en de fr es it pt ru ja zh ko nl pl sv ar tr uk hi\n", lang)
		os.Exit(1)
	}

	dataDir := xdgDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: mkdir %s: %v\n", dataDir, err)
		os.Exit(1)
	}

	// Determine download URL and temp file extension from source.
	var dlURL, tmpExt string
	var kaikkiGZ bool // true when kaikki download is gzip-compressed (native edition)
	switch source {
	case "wiki":
		dlURL = wiktionaryDumpURL()
		tmpExt = ".xml.bz2.tmp"
	default: // "kaikki"
		source = "kaikki"
		if nativeURL, ok := kaikkiNativeURL(lang); ok {
			// Native wiktionary edition: raw wiktextract JSONL.GZ, filtered by
			// lang_code during parse. Etymologies are in the target language.
			dlURL = nativeURL
			tmpExt = ".jsonl.gz.tmp"
			kaikkiGZ = true
		} else {
			// No native edition available: fall back to kaikki.org/dictionary/
			// (sourced from en.wiktionary.org — etymologies will be in English).
			dlURL = kaikkiURL(langName)
			tmpExt = ".jsonl.tmp"
		}
	}

	// ── Up-to-date check: HEAD request → Last-Modified ─────────────────
	// Source URL is embedded in the version file, so switching --kaikki↔--wiki
	// always triggers a fresh install even if Last-Modified is unchanged.
	idxPath := filepath.Join(dataDir, lang+".idx")
	verPath := filepath.Join(dataDir, lang+".version")
	var remoteLastMod string
	headResp, headErr := httpClient.Head(dlURL) //nolint:gosec
	if headErr == nil {
		headResp.Body.Close()
		remoteLastMod = headResp.Header.Get("Last-Modified")
		if remoteLastMod != "" {
			if existing, err := os.ReadFile(verPath); err == nil {
				exStr := string(existing)
				if !force &&
					strings.Contains(exStr, "source: "+dlURL) &&
					strings.Contains(exStr, "last-modified: "+remoteLastMod) {
					if _, err := os.Stat(idxPath); err == nil {
						fmt.Printf("\n  %s%s%s pack is already up to date (%s).\n  Run with --force to reinstall.\n\n",
							Bold+CSyn, strings.ToUpper(lang), R, remoteLastMod)
						return
					}
				}
			}
		}
	}

	fmt.Printf("\n  installing %s%s%s language pack (source: %s)\n\n",
		Bold, strings.ToUpper(lang), R, source)

	// ── Step 1: download ─────────────────────────────────────────────────
	tmpPath := filepath.Join(dataDir, lang+tmpExt)
	defer os.Remove(tmpPath)

	resp, err := http.Get(dlURL) //nolint:gosec
	if err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: download: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "lexify -i: HTTP %d\n", resp.StatusCode)
		os.Exit(1)
	}
	if remoteLastMod == "" {
		remoteLastMod = resp.Header.Get("Last-Modified")
	}
	f, err := os.Create(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: create temp: %v\n", err)
		os.Exit(1)
	}
	dlBar := newProgress(resp.ContentLength, "downloading")
	n, err := io.Copy(io.MultiWriter(f, dlBar), resp.Body)
	f.Close()
	dlBar.Finish()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: download: %v\n", err)
		os.Exit(1)
	}
	fileSize := n

	// ── Step 2: parse (branched by source) ──────────────────────────────
	t0 := time.Now()
	rdr, err := os.Open(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: open: %v\n", err)
		os.Exit(1)
	}
	parseBar := newProgress(fileSize, "parsing")
	parseRdr := &progressReader{r: rdr, prog: parseBar}
	index := make(map[string]LexEntry, 500_000)
	var total int

	switch source {
	case "kaikki":
		var scanSrc io.Reader = parseRdr
		if kaikkiGZ {
			gz, err := gzip.NewReader(parseRdr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "lexify -i: gzip: %v\n", err)
				os.Exit(1)
			}
			defer gz.Close()
			scanSrc = gz
		}
		scanner := bufio.NewScanner(scanSrc)
		scanner.Buffer(make([]byte, 4<<20), 4<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var k kEntry
			if json.Unmarshal(line, &k) != nil || k.LangCode != lang || strings.TrimSpace(k.Word) == "" {
				continue
			}
			key, entry := convertKEntry(k)
			if existing, ok := index[key]; ok {
				index[key] = mergeKEntry(existing, entry)
			} else {
				index[key] = entry
			}
			total++
		}
	default: // wiki
		bzRdr := bzip2.NewReader(parseRdr)
		decoder := xml.NewDecoder(bzRdr)
		for {
			tok, err := decoder.Token()
			if err != nil {
				break
			}
			se, ok := tok.(xml.StartElement)
			if !ok || se.Name.Local != "page" {
				continue
			}
			var p xmlPage
			if err := decoder.DecodeElement(&p, &se); err != nil {
				continue
			}
			if p.NS != 0 {
				continue
			}
			title := strings.TrimSpace(p.Title)
			if title == "" || strings.ContainsAny(title, ":/") {
				continue
			}
			entry, ok := parseWiktionaryPage(p.Revision.Text, langName)
			if !ok {
				continue
			}
			index[strings.ToLower(title)] = entry
			total++
		}
	}
	rdr.Close()
	parseBar.Finish()
	fmt.Fprintf(os.Stderr, "  %s%dk words%s indexed in %.1fs\n",
		Dim, total/1000, R, time.Since(t0).Seconds())

	// ── Step 3: write idx + dat pack ─────────────────────────────────────
	type kv struct {
		key   string
		entry LexEntry
	}
	entries := make([]kv, 0, len(index))
	for k, v := range index {
		if len(k) <= idxKeySize {
			entries = append(entries, kv{k, v})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	packBar := newProgress(-1, "packing")
	datPath := filepath.Join(dataDir, lang+".dat")
	datF, err := os.Create(datPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: create dat: %v\n", err)
		os.Exit(1)
	}
	idxF, err := os.Create(idxPath)
	if err != nil {
		datF.Close()
		fmt.Fprintf(os.Stderr, "lexify -i: create idx: %v\n", err)
		os.Exit(1)
	}
	var datOffset uint64
	idxRecord := make([]byte, idxRecordSize)
	for _, kv := range entries {
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(kv.entry); err != nil {
			continue
		}
		entryBytes := buf.Bytes()
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(entryBytes)))
		if _, err := datF.Write(lenBuf[:]); err != nil {
			break
		}
		if _, err := datF.Write(entryBytes); err != nil {
			break
		}
		packBar.Write(entryBytes) //nolint:errcheck
		clear(idxRecord)
		copy(idxRecord[:idxKeySize], []byte(kv.key))
		binary.BigEndian.PutUint64(idxRecord[idxKeySize:], datOffset)
		if _, err := idxF.Write(idxRecord); err != nil {
			break
		}
		datOffset += uint64(4 + len(entryBytes))
	}
	datF.Close()
	idxF.Close()
	packBar.Finish()
	if st, err := os.Stat(datPath); err == nil {
		fmt.Fprintf(os.Stderr, "  %s%.1f MB%s written  →  %s\n",
			Dim, float64(st.Size())/(1024*1024), R, datPath)
	}

	// ── Version file ────────────────────────────────────────────────────
	ver := fmt.Sprintf("source: %s\nbuilt:  %s\nlast-modified: %s\n", dlURL, time.Now().Format(time.RFC3339), remoteLastMod)
	os.WriteFile(verPath, []byte(ver), 0o644) //nolint

	// Mark whether this pack provides native-language etymology (for runtime use).
	if kaikkiGZ {
		os.WriteFile(nativePath(lang), []byte("native"), 0o644) //nolint
	} else {
		os.Remove(nativePath(lang)) //nolint
	}

	fmt.Printf("\n  %s%s pack installed.%s\n\n",
		Bold+CSyn, strings.ToUpper(lang), R)
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		printHelp()
		return
	}
	// Check for -i/--install flag anywhere in args (e.g. lexify -i en de ru --force)
	args := os.Args[1:]
	for j, a := range args {
		if a == "-i" || a == "--install" {
			if j+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: lexify -i <lang> [lang ...] [--kaikki|--wiki] [--force]")
				os.Exit(1)
			}
			source := "kaikki"
			force := false
			var langs []string
			for _, arg := range args[j+1:] {
				switch arg {
				case "--kaikki":
					source = "kaikki"
				case "--wiki":
					source = "wiki"
				case "--force":
					force = true
				default:
					if !strings.HasPrefix(arg, "-") {
						langs = append(langs, strings.ToLower(strings.TrimSpace(arg)))
					}
				}
			}
			if len(langs) == 0 {
				fmt.Fprintln(os.Stderr, "usage: lexify -i <lang> [lang ...] [--kaikki|--wiki] [--force]")
				os.Exit(1)
			}
			for _, lang := range langs {
				cmdInstall(lang, source, force)
			}
			return
		}
	}
	word := strings.TrimSpace(os.Args[1])
	debug := false
	apiOnly := false
	hintNonEN := false
	var translateLangs []string
	for _, a := range os.Args[2:] {
		if a == "-d" {
			debug = true
			continue
		}
		if a == "-o" {
			apiOnly = true
			continue
		}
		l := strings.ToLower(strings.TrimSpace(a))
		if l == "en" {
			hintNonEN = true
		} else {
			translateLangs = append(translateLangs, l)
		}
	}
	run(word, translateLangs, debug, apiOnly, hintNonEN)
}
