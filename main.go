// lexify — word lookup: definition · synonyms · etymology · translation
// Usage: lexify <word> [lang ...]
// APIs: dictionaryapi.dev · datamuse.com · en.wiktionary.org · google gtx
// Deps: github.com/schollz/progressbar/v3 (install subcommand only)
package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"encoding/xml"
	"fmt"
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

	"github.com/schollz/progressbar/v3"
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
	//lint:ignore U1000 kept for completeness
	CErr = "\033[31m" // red — errors
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}

// ── Text helpers ──────────────────────────────────────────────────────────────

// runeLen returns the number of Unicode code points in s.
// Used for text width calculations so Cyrillic/CJK/etc. wrap correctly.
func runeLen(s string) int { return len([]rune(s)) }

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

	// Single-argument templates not otherwise handled: return the argument as-is
	// Covers things like {{lang|word}}, {{smallcaps|word}}, etc.
	if len(parts) == 2 {
		val := strings.TrimSpace(parts[1])
		if !strings.Contains(val, "=") {
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
func stripWikitext(text string) string {
	// Strip HTML comments <!-- ... -->
	text = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(text, "")
	// Strip <ref>...</ref> and self-closing <ref ... />  (footnote citations)
	text = regexp.MustCompile(`(?s)<ref[^>]*>.*?</ref>`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`<ref[^>]*/>`).ReplaceAllString(text, "")
	// Strip remaining HTML tags
	text = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(text, "")

	// Resolve innermost {{ }} templates first (no nesting), repeat until stable
	tmplRe := regexp.MustCompile(`\{\{([^{}]*)\}\}`)
	for {
		n := tmplRe.ReplaceAllStringFunc(text, func(m string) string {
			inner := m[2 : len(m)-2]
			return resolveTemplate(inner)
		})
		if n == text {
			break
		}
		text = n
	}
	// [[link|display]] → display
	text = regexp.MustCompile(`\[\[(?:[^\]|]+\|)([^\]]+)\]\]`).ReplaceAllString(text, "$1")
	// [[link]] → link
	text = regexp.MustCompile(`\[\[([^\]]+)\]\]`).ReplaceAllString(text, "$1")
	// remove '''bold''' and ''italic'' markers
	text = strings.ReplaceAll(text, "'''", "")
	text = strings.ReplaceAll(text, "''", "")
	// remove wikitext list/indent markers at start of lines
	text = regexp.MustCompile(`(?m)^[#*:;]+\s*`).ReplaceAllString(text, "")
	// tidy up punctuation artefacts left by removed templates: ". ." "( )" ", ,"
	text = regexp.MustCompile(`(\. ){2,}\.?`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`(, ){2,},?`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`\(\s*\)`).ReplaceAllString(text, "")
	// tidy up spacing artefacts from removed templates
	text = regexp.MustCompile(` {2,}`).ReplaceAllString(text, " ")
	text = regexp.MustCompile(`\n{3,}`).ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
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
}

// LexEntry is the value stored in each embedded data pack entry.
// Field names must match tools/prepare_data.go exactly for gob round-trips.
type LexEntry struct {
	IPA      string
	Meanings []Meaning
	Syns     []string // union across senses; caller trims to display limit
	Etym     string
}

// LookupProvider abstracts over the embedded EN pack and user-installed packs.
type LookupProvider interface {
	Lookup(word string) *LexEntry // nil on miss
}

// xdgDataDir returns ~/.local/share/lexify (or $XDG_DATA_HOME/lexify).
func xdgDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "lexify")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "lexify")
}

// installedPack is the LookupProvider backed by a user-installed pack on disk.
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

// enProvider is the active EN LookupProvider, backed by a user-installed XDG pack.
// nil when no pack is installed; lookupEN returns nil in that case and run() falls
// back to the dictionaryapi.dev network API.
var enProvider, usingInstalledPack = func() (LookupProvider, bool) {
	idxPath := filepath.Join(xdgDataDir(), "en.idx")
	datPath := filepath.Join(xdgDataDir(), "en.dat")
	if _, err := os.Stat(idxPath); err == nil {
		p := &installedPack{idxPath: idxPath, datPath: datPath}
		go p.init()
		return p, true
	}
	return nil, false
}()

func lookupEN(word string) *LexEntry {
	if enProvider == nil {
		return nil
	}
	return enProvider.Lookup(word)
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
var posSections = map[string]bool{
	"noun": true, "verb": true, "adjective": true, "adverb": true,
	"pronoun": true, "preposition": true, "conjunction": true,
	"interjection": true, "determiner": true, "article": true,
	"numeral": true, "particle": true, "phrase": true,
	"suffix": true, "prefix": true, "affix": true,
	"proper noun": true, "proverb": true, "idiom": true,
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

// trimEtym caps etymology text at 500 runes at a sentence boundary.
func trimEtym(etym string) string {
	r := []rune(etym)
	if len(r) <= 500 {
		return etym
	}
	cut := -1
	for i := 499; i >= 0; i-- {
		if r[i] == '.' || r[i] == '!' || r[i] == '?' {
			if i+1 == len(r) || r[i+1] == ' ' {
				cut = i + 1
				break
			}
		}
	}
	if cut > 0 {
		return strings.TrimSpace(string(r[:cut])) + "…"
	}
	i := 497
	for i > 0 && r[i] != ' ' {
		i--
	}
	if i == 0 {
		i = 497
	}
	return string(r[:i]) + "…"
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
			r := []rune(ex)
			if len(r) >= 2 && len(r) <= 150 {
				pendingEx = ex
			} else if len(r) > 150 {
				i := 147
				for i > 0 && r[i] != ' ' {
					i--
				}
				if i == 0 {
					i = 147
				}
				pendingEx = string(append(r[:i], '…'))
			}
		}
	}
	flush()
	if len(defs) == 0 {
		return nil
	}
	// Collect sense-level synonyms from {{syn|lang|word1|word2|…}} templates.
	synRe := regexp.MustCompile(`\{\{syn\|[^|{}]+\|([^{}]+)\}\}`)
	seen := map[string]bool{}
	var syns []string
	for _, sm := range synRe.FindAllStringSubmatch(text, -1) {
		for _, w := range strings.Split(sm[1], "|") {
			w = strings.TrimSpace(w)
			if w != "" && !strings.Contains(w, "=") && !seen[w] {
				seen[w] = true
				syns = append(syns, w)
			}
		}
	}
	return &Meaning{POS: pos, Defs: defs, Syns: syns}
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
		for _, w := range regexp.MustCompile(`[,;]+`).Split(line, -1) {
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
		switch {
		case lower == "pronunciation":
			if m := ipaExtractRe.FindStringSubmatch(text); m != nil {
				ipa = strings.TrimSpace(m[1])
			}
		case lower == "etymology" || strings.HasPrefix(lower, "etymology "):
			etym = trimEtym(stripWikitext(text))
		case posSections[lower]:
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
func kaikkiURL(langName string) string {
	return fmt.Sprintf("https://kaikki.org/dictionary/%s/kaikki.org-dictionary-%s.jsonl", langName, langName)
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
			ex := []rune(strings.TrimSpace(s.Examples[0].Text))
			if len(ex) > 150 {
				i := 147
				for i > 0 && ex[i] != ' ' {
					i--
				}
				if i == 0 {
					i = 147
				}
				ex = append(ex[:i], '…')
			}
			if len(ex) >= 2 {
				example = string(ex)
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
		meanings = append(meanings, Meaning{POS: pos, Defs: defs, Syns: senseSyns})
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
		meaning := Meaning{POS: m.PartOfSpeech}
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

// wiktionarySection fetches the full page wikitext in a single API call and
// extracts the first section whose heading satisfies match.
// This avoids the two-round-trip sections-index → wikitext pattern.
func wiktionarySection(base, word string, match func(string) bool) string {
	var resp struct {
		Query struct {
			Pages map[string]struct {
				Revisions []struct {
					Text string `json:"*"`
				} `json:"revisions"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := fetchJSON(base+"?action=query&titles="+url.QueryEscape(word)+"&prop=revisions&rvprop=content&format=json", &resp); err != nil {
		return ""
	}
	var fullText string
	for _, p := range resp.Query.Pages {
		if len(p.Revisions) > 0 {
			fullText = p.Revisions[0].Text
		}
	}
	if fullText == "" {
		return ""
	}

	// Walk lines: find the heading that satisfies match, collect until next same/higher heading.
	// We parse headings manually (count leading/trailing '=') to avoid RE2 backreference limits.
	lines := strings.Split(fullText, "\n")
	var result []string
	inSection := false
	for _, line := range lines {
		level, heading := parseWikiHeading(line)
		if level > 0 {
			if inSection {
				break // stop at any heading once inside the target section
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
func fetchGTX(word, lang string) *Translation {
	var raw []json.RawMessage
	if err := fetchJSON("https://translate.google.com/translate_a/single?client=gtx&sl=auto&tl="+url.QueryEscape(lang)+"&dt=t&dt=bd&q="+url.QueryEscape(word), &raw); err != nil || len(raw) == 0 {
		return nil
	}
	translated := parseGTXSegments(raw)
	detected := "?"
	if len(raw) > 2 {
		json.Unmarshal(raw[2], &detected) //nolint
	}
	if translated == "" || strings.EqualFold(translated, word) {
		return nil
	}
	return &Translation{Word: translated, Detected: detected}
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
	return &Translation{Word: t, Detected: "en"}
}

func fetchTranslation(word, lang string) *Translation {
	if lang == "" || strings.ToLower(lang) == "en" {
		return nil
	}
	if t := fetchGTX(word, lang); t != nil {
		return t
	}
	return fetchMyMemory(word, lang)
}

// fetchTextTranslation translates a multi-line text block (definition, etymology).
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

// fetchTargetSynonyms fetches synonyms from the target-language Wiktionary.
func fetchTargetSynonyms(word, lang string) []string {
	wt := wiktionarySection(
		"https://"+lang+".wiktionary.org/w/api.php", word,
		func(line string) bool { return synHeadingPat.MatchString(line) },
	)
	if wt == "" {
		return nil
	}

	// Extract [[word]] or [[word|display]] from wikitext — these are the synonyms
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
		if unicode.IsUpper(runes[0]) {
			continue // skip proper nouns / section headings
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

// ── Renderer ──────────────────────────────────────────────────────────────────

type RenderInput struct {
	Word           string
	ResolvedEN     string // non-empty when input was non-EN and translated to this EN word
	APIFallback    bool   // true when -o was set but pack had no entry and API was used instead
	TargetLangs    []string
	Defn           *Definition
	SynSource      []string
	Etym           string
	Translations   []*Translation
	SynTargets     [][]string
	DefnTranslated string
	EtymTranslated string
	PrimaryLang    string
	Warnings       []string
	Elapsed        time.Duration
	FetchLog       []string // per-fetch timing lines
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
	showingTrans := in.PrimaryLang != "" && (in.DefnTranslated != "" || in.EtymTranslated != "")
	dispLang := srcLang
	if showingTrans {
		dispLang = strings.ToUpper(in.PrimaryLang)
	}
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
	fmt.Printf("  %s  %s%s%s  %s[%s]%s\n",
		wordDisplay, CEx, phonetic, R, Dim, langTag, R)
	fmt.Println(divider())

	// ── warnings ─────────────────────────────────────────────────────────────
	for _, w := range in.Warnings {
		fmt.Printf("  %s! %s%s\n", CErr, w, R)
	}
	if len(in.Warnings) > 0 {
		fmt.Println()
	}

	// ── translations ──────────────────────────────────────────────────────────
	if len(in.Translations) > 0 {
		fmt.Print(sectionHeader(ITrans, "TRANSLATIONS"))
		for i, t := range in.Translations {
			langCode := strings.ToUpper(in.TargetLangs[i])
			if t == nil {
				fmt.Printf("  %s%s%s  %s(translation failed)%s\n\n", CPos+Bold, langCode, R, CEx, R)
				continue
			}
			fmt.Printf("  %s%s%s  %s%s%s\n\n", CPos+Bold, langCode, R, CTrans+Bold, t.Word, R)
		}
	}

	// ── definition ────────────────────────────────────────────────────────────
	fmt.Print(sectionHeader(IDef, "DEFINITION ("+dispLang+")"))
	if showingTrans && in.DefnTranslated != "" {
		for _, para := range strings.Split(in.DefnTranslated, "\n\n") {
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
					fmt.Printf("  %s\"%s\"%s\n", CEx, d.Example, R)
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
	allSyn := dedupe(in.SynSource)
	if in.Defn != nil {
		for _, m := range in.Defn.Meanings {
			for _, s := range m.Syns {
				if !contains(allSyn, s) {
					allSyn = append(allSyn, s)
				}
			}
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
	if showingTrans && in.EtymTranslated != "" {
		etymText = in.EtymTranslated
		etymLang = dispLang
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
	fmt.Printf("  %s-i <lang>%s  install offline pack  (e.g. lexify -i en)\n", CPos, R)
	fmt.Printf("  %s  --kaikki%s  source: kaikki.org JSONL  ~500 MB, ~2 min  %s(default)%s\n", CPos, R, Dim, R)
	fmt.Printf("  %s  --wiki%s   source: en.wiktionary.org XML dump  ~1.2 GB, ~10 min\n", CPos, R)
	fmt.Printf("  %s-o%s         use installed offline pack\n", CPos, R)
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
	fmt.Println(wordWrap("EN lookups are fully offline · translations fire in parallel · no API keys", dividerWidth-4, "  "))
	fmt.Println()
}

// ── Utilities ─────────────────────────────────────────────────────────────────

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(s))
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ── Orchestration ─────────────────────────────────────────────────────────────

func run(word string, translateLangs []string, debug, offline bool) {
	start := time.Now()
	fmt.Printf("\n  %slooking up %s%s%s%s…%s\r", CEx, Bold, word, R, CEx, R)

	// ── Phase 1: optional XDG lookup + word translations — fully parallel ──────────
	// When -o is given, lookupEN is fired alongside translations so gob decode
	// overlaps with network RTT. Without -o the API path is used exclusively.
	var (
		entry      *LexEntry
		defn       *Definition
		synSource  []string
		etym       string
		wordTrans  = make([]*Translation, len(translateLangs))
		tWordTrans = make([]time.Duration, len(translateLangs))
		tEmbed     time.Duration
		tAPI       time.Duration
		tAPIEtym   time.Duration
	)
	var (
		apiDefn     *Definition
		apiEtym     string
		resolvedEN  string // set when input is non-EN and routed through an EN translation
		apiFallback bool   // set when -o pack miss forces an API call
	)

	// synTargets and wg2 are declared here so trans goroutines can chain
	// syns lookups immediately after the translation result arrives,
	// overlapping with defn/etym fetches in Phase 1.
	var (
		synTargets  = make([][]string, len(translateLangs))
		tSynTargets = make([]time.Duration, len(translateLangs))
	)
	var wg2 sync.WaitGroup

	var wg1 sync.WaitGroup
	wg1.Add(len(translateLangs))
	if offline {
		wg1.Add(1)
		go func() {
			defer wg1.Done()
			t := time.Now()
			entry = lookupEN(word)
			tEmbed = time.Since(t)
		}()
	} else {
		// Fire definition + etymology in parallel with translations.
		// Total Phase-1 cost = max(defn_rtt, etym_rtt, trans_rtt).
		wg1.Add(2)
		go func() {
			defer wg1.Done()
			t := time.Now()
			apiDefn = fetchDefinition(word)
			tAPI = time.Since(t)
		}()
		go func() {
			defer wg1.Done()
			t := time.Now()
			apiEtym = fetchEtymologyWiki(word)
			tAPIEtym = time.Since(t)
		}()
	}
	for i, lang := range translateLangs {
		i, lang := i, lang
		go func() {
			defer wg1.Done()
			t := time.Now()
			wordTrans[i] = fetchTranslation(word, lang)
			tWordTrans[i] = time.Since(t)
			// Fire target-language syns immediately — overlaps with defn/etym.
			if wordTrans[i] != nil && wordTrans[i].Word != "" {
				wg2.Add(1)
				go func() {
					defer wg2.Done()
					t2 := time.Now()
					synTargets[i] = fetchTargetSynonyms(wordTrans[i].Word, lang)
					tSynTargets[i] = time.Since(t2)
				}()
			}
		}()
	}
	wg1.Wait()

	// Populate defn/synSource/etym from XDG entry or parallel API results.
	if entry != nil {
		defn = &Definition{Phonetic: entry.IPA, Meanings: entry.Meanings}
		synSource = entry.Syns
		etym = entry.Etym
	} else {
		if apiDefn != nil {
			defn = apiDefn
		}
		etym = apiEtym
	}

	// Non-EN source word routing: if lookup missed, try treating the input as a
	// non-English word. fetchGTX returns nil when the translation equals the
	// input (i.e. the word is already English), so this is safe to call
	// unconditionally — no detectedSrc guard needed. This also covers the case
	// where the only target lang is "en" (fetchTranslation short-circuits to nil
	// for that lang, so wordTrans[0] would be nil and Detected would be unavailable).
	if entry == nil && defn == nil {
		if enTrans := fetchGTX(word, "en"); enTrans != nil {
			resolvedEN = enTrans.Word
			if offline {
				if e := lookupEN(enTrans.Word); e != nil {
					entry = e
					defn = &Definition{Phonetic: e.IPA, Meanings: e.Meanings}
					synSource = e.Syns
					etym = e.Etym
				}
			}
			if entry == nil {
				if d := fetchDefinition(enTrans.Word); d != nil {
					defn = d
					if offline {
						apiFallback = true
					}
				}
			}
		}
	}
	// Direct EN pack miss in offline mode: XDG was queried but returned nothing
	// and no non-EN routing saved us — mark fallback if API was used any other way.
	if offline && entry == nil && defn != nil && !apiFallback {
		apiFallback = true
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

	// ── Phase 2: immediately fan out — no second barrier ─────────────────────
	// Each result spawns its own goroutine as soon as Phase 1 data is ready.
	primaryLang := ""
	if len(translateLangs) > 0 {
		primaryLang = translateLangs[0]
	}

	// Build definition text block for content translation
	defBlock := ""
	if primaryLang != "" && defn != nil && len(defn.Meanings) > 0 {
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

	var (
		defnTranslated string
		etymTranslated string
		tDefnTrans     time.Duration
		tEtymTrans     time.Duration
	)

	// Fire defn/etym text translations — these are the only remaining Phase-2
	// goroutines; syns goroutines were already launched during Phase 1.
	wg2.Add(2)
	go func() {
		defer wg2.Done()
		if primaryLang != "" && defBlock != "" {
			t := time.Now()
			defnTranslated = fetchTextTranslation(defBlock, primaryLang)
			tDefnTrans = time.Since(t)
		}
	}()
	go func() {
		defer wg2.Done()
		if primaryLang != "" && etym != "" {
			t := time.Now()
			etymTranslated = fetchTextTranslation(etym, primaryLang)
			tEtymTrans = time.Since(t)
		}
	}()
	wg2.Wait()

	// Remap synTargets to match the (possibly filtered) translateLangs slice.
	{
		mapped := make([][]string, len(validIdx))
		mappedT := make([]time.Duration, len(validIdx))
		for j, idx := range validIdx {
			mapped[j] = synTargets[idx]
			mappedT[j] = tSynTargets[idx]
		}
		synTargets = mapped
		tSynTargets = mappedT
	}

	// Clear the "looking up…" line
	fmt.Printf("\033[1A\033[2K")

	var fetchLog []string
	if debug {
		if entry != nil {
			fetchLog = append(fetchLog, fmt.Sprintf("  xdg          %dms", tEmbed.Milliseconds()))
		} else {
			fetchLog = append(fetchLog,
				fmt.Sprintf("  defn         %dms", tAPI.Milliseconds()),
				fmt.Sprintf("  etym         %dms", tAPIEtym.Milliseconds()))
		}
		for i, lang := range translateLangs {
			fetchLog = append(fetchLog, fmt.Sprintf("  trans(%s)     %dms", lang, tWordTrans[i].Milliseconds()))
			if tSynTargets[i] > 0 {
				fetchLog = append(fetchLog, fmt.Sprintf("  syns(%s)      %dms", lang, tSynTargets[i].Milliseconds()))
			}
		}
		if tDefnTrans > 0 {
			fetchLog = append(fetchLog, fmt.Sprintf("  defn-trans   %dms", tDefnTrans.Milliseconds()))
		}
		if tEtymTrans > 0 {
			fetchLog = append(fetchLog, fmt.Sprintf("  etym-trans   %dms", tEtymTrans.Milliseconds()))
		}
	}

	render(RenderInput{
		Word:           word,
		ResolvedEN:     resolvedEN,
		APIFallback:    apiFallback,
		TargetLangs:    translateLangs,
		Defn:           defn,
		SynSource:      synSource,
		Etym:           etym,
		Translations:   wordTrans,
		SynTargets:     synTargets,
		DefnTranslated: defnTranslated,
		EtymTranslated: etymTranslated,
		PrimaryLang:    primaryLang,
		Warnings:       warnings,
		Elapsed:        time.Since(start),
		FetchLog:       fetchLog,
	})
}

// ── Install subcommand  (-i / --install) ────────────────────────────────────────────────────────

// cmdInstall downloads and indexes a language pack for offline use.
// source is "kaikki" (default, fast ~500 MB JSONL) or "wiki" (~1.2 GB XML dump).
func cmdInstall(lang, source string) {
	langName, ok := langNames[lang]
	if !ok {
		fmt.Fprintf(os.Stderr, "lexify -i: unsupported language %q\n  supported: en de fr es it pt ru ja zh ko nl pl sv ar tr uk hi\n", lang)
		os.Exit(1)
	}
	newBar := func(max int64, desc string) *progressbar.ProgressBar {
		opts := []progressbar.Option{
			progressbar.OptionSetDescription(fmt.Sprintf("  %-13s", desc)),
			progressbar.OptionSetWidth(32),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "█",
				SaucerPadding: "░",
				BarStart:      "",
				BarEnd:        "",
			}),
			progressbar.OptionSetRenderBlankState(true),
			progressbar.OptionShowBytes(true),
			progressbar.OptionSetWriter(os.Stderr),
			progressbar.OptionOnCompletion(func() { fmt.Fprintln(os.Stderr) }),
			progressbar.OptionUseANSICodes(true),
			progressbar.OptionEnableColorCodes(false),
		}
		return progressbar.NewOptions64(max, opts...)
	}

	dataDir := xdgDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lexify -i: mkdir %s: %v\n", dataDir, err)
		os.Exit(1)
	}

	// Determine download URL and temp file extension from source.
	var dlURL, tmpExt string
	switch source {
	case "wiki":
		dlURL = wiktionaryDumpURL()
		tmpExt = ".xml.bz2.tmp"
	default: // "kaikki"
		source = "kaikki"
		dlURL = kaikkiURL(langName)
		tmpExt = ".jsonl.tmp"
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
				if strings.Contains(exStr, "source: "+dlURL) &&
					strings.Contains(exStr, "last-modified: "+remoteLastMod) {
					if _, err := os.Stat(idxPath); err == nil {
						fmt.Printf("\n  %s%s%s pack is already up to date (%s).\n\n",
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
	dlBar := newBar(resp.ContentLength, "downloading")
	n, err := io.Copy(io.MultiWriter(f, dlBar), resp.Body)
	f.Close()
	_ = dlBar.Finish()
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
	parseBar := newBar(fileSize, "parsing")
	parseRdr := progressbar.NewReader(rdr, parseBar)
	index := make(map[string]LexEntry, 500_000)
	var total int

	switch source {
	case "kaikki":
		scanner := bufio.NewScanner(&parseRdr)
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
		bzRdr := bzip2.NewReader(&parseRdr)
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
	_ = parseBar.Finish()
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

	packBar := newBar(-1, "packing")
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
	_ = packBar.Finish()
	if st, err := os.Stat(datPath); err == nil {
		fmt.Fprintf(os.Stderr, "  %s%.1f MB%s written  →  %s\n",
			Dim, float64(st.Size())/(1024*1024), R, datPath)
	}

	// ── Version file ────────────────────────────────────────────────────
	ver := fmt.Sprintf("source: %s\nbuilt:  %s\nlast-modified: %s\n", dlURL, time.Now().Format(time.RFC3339), remoteLastMod)
	os.WriteFile(verPath, []byte(ver), 0o644) //nolint

	fmt.Printf("\n  %s%s pack installed.%s  run 'lexify <word> -o' to use it.\n\n",
		Bold+CSyn, strings.ToUpper(lang), R)
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		printHelp()
		return
	}
	// Check for -i/--install flag anywhere in args (e.g. lexify -i en, lexify -i en --wiki)
	args := os.Args[1:]
	for j, a := range args {
		if a == "-i" || a == "--install" {
			if j+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "usage: lexify -i <lang> [--kaikki|--wiki]")
				os.Exit(1)
			}
			lang := strings.ToLower(strings.TrimSpace(args[j+1]))
			source := "kaikki" // default: fast ~500MB JSONL
			for _, flag := range args[j+2:] {
				if flag == "--kaikki" {
					source = "kaikki"
				} else if flag == "--wiki" {
					source = "wiki"
				}
			}
			cmdInstall(lang, source)
			return
		}
	}
	word := strings.TrimSpace(os.Args[1])
	debug := false
	offline := false
	var translateLangs []string
	for _, a := range os.Args[2:] {
		if a == "-d" {
			debug = true
			continue
		}
		if a == "-o" {
			offline = true
			continue
		}
		l := strings.ToLower(strings.TrimSpace(a))
		if l != "en" {
			translateLangs = append(translateLangs, l)
		}
	}
	run(word, translateLangs, debug, offline)
}
