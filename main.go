// lexify — word lookup: definition · synonyms · etymology · translation
// Usage: lexify <word> [lang ...]
// APIs: dictionaryapi.dev · datamuse.com · en.wiktionary.org · google gtx
// Deps: stdlib only
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

func fetchSynonyms(word string) []string {
	var raw []struct {
		Word string `json:"word"`
	}
	if err := fetchJSON("https://api.datamuse.com/words?rel_syn="+url.QueryEscape(word)+"&max=14", &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.Word)
	}
	return out
}

// fetchEnWiktionary fetches the English Wiktionary page once and extracts both
// the etymology text and a synonym list in a single HTTP round-trip, replacing
// the separate datamuse + wiktionary calls used in Phase 1.
// Falls back to datamuse for synonyms when the Wiktionary page has no Synonyms section.
func fetchEnWiktionary(word string) (etym string, syns []string) {
	const base = "https://en.wiktionary.org/w/api.php"
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
		return "", nil
	}
	var fullText string
	for _, p := range resp.Query.Pages {
		if len(p.Revisions) > 0 {
			fullText = p.Revisions[0].Text
		}
	}
	if fullText == "" {
		return "", nil
	}

	const (stNone = iota; stEtym; stSyn)
	isEtymHeading := func(h string) bool {
		l := strings.ToLower(h)
		return l == "etymology" || strings.HasPrefix(l, "etymology")
	}
	state := stNone
	etymDone := false // stop after first non-empty paragraph (blank line ends it)
	var etymLines, synLines []string
	for _, line := range strings.Split(fullText, "\n") {
		_, heading := parseWikiHeading(line)
		if heading != "" {
			state = stNone
			if !etymDone && isEtymHeading(heading) {
				state = stEtym
			} else if synHeadingPat.MatchString(heading) {
				state = stSyn
			}
			continue
		}
		switch state {
		case stEtym:
			if strings.TrimSpace(line) == "" {
				if len(etymLines) > 0 {
					etymDone = true
					state = stNone // stop collecting etymology after first blank line
				}
			} else {
				etymLines = append(etymLines, line)
			}
		case stSyn:
			synLines = append(synLines, line)
		}
	}

	// Process etymology
	if len(etymLines) > 0 {
		clean := strings.TrimSpace(stripWikitext(strings.Join(etymLines, "\n")))
		if runes := []rune(clean); len(runes) > 700 {
			clean = string(runes[:700])
		}
		etym = clean
	}

	// Extract synonym links from Wiktionary
	if len(synLines) > 0 {
		linkRe := regexp.MustCompile(`\[\[(?:[^\]|]+\|)?([^\]|]+)\]\]`)
		digitSlashRe := regexp.MustCompile(`[\d/]`)
		seen := map[string]bool{}
		for _, m := range linkRe.FindAllStringSubmatch(strings.Join(synLines, "\n"), -1) {
			w := strings.TrimSpace(m[1])
			rs := []rune(w)
			if len(rs) < 2 || len(rs) > 30 || unicode.IsUpper(rs[0]) || digitSlashRe.MatchString(w) {
				continue
			}
			if !seen[w] {
				seen[w] = true
				syns = append(syns, w)
			}
			if len(syns) >= 14 {
				break
			}
		}
	}

	return etym, syns
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

// English: Datamuse. Other languages: Wiktionary synonym section via action=parse.
func fetchTargetSynonyms(word, lang string) []string {
	if lang == "en" {
		return fetchSynonyms(word)
	}

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
	fmt.Printf("  %s%s%s  %s%s%s  %s[%s]%s\n",
		CWord, Bold+in.Word, R, CEx, phonetic, R, Dim, langTag, R)
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

	fmt.Printf("  %sfetched in %dms%s\n", Dim, in.Elapsed.Milliseconds(), R)
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
		{IDef + "Definition", "dictionaryapi.dev  (English source words)"},
		{ISyn + "Synonyms", "datamuse.com  (EN) · Wiktionary (other langs)"},
		{IEty + "Etymology", "en.wiktionary.org  (action=parse, wikitext)"},
		{ITrans + "Translation", "Google Translate gtx  · MyMemory fallback (no key)"},
	}
	for _, s := range sources {
		fmt.Printf("  %s%s%s%s\n", CHead, Bold, s[0], R)
		fmt.Println(wordWrap(s[1], dividerWidth-4, "    "))
		fmt.Println()
	}
	fmt.Println(divider())
	fmt.Println(wordWrap("all requests fire in parallel · no API keys · no local deps", dividerWidth-4, "  "))
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

func run(word string, translateLangs []string, debug bool) {
	start := time.Now()
	fmt.Printf("\n  %slooking up %s%s%s%s…%s\r", CEx, Bold, word, R, CEx, R)

	// ── Phase 1: parallel source fetches + word translations ─────────────────
	var (
		defn      *Definition
		synSource []string
		etym      string
		wordTrans = make([]*Translation, len(translateLangs))
		// per-goroutine timings — each written by exactly one goroutine, no mutex needed
		tDefn, tWiki time.Duration
		tWordTrans   = make([]time.Duration, len(translateLangs))
	)

	var wg1 sync.WaitGroup
	wg1.Add(2 + len(translateLangs))

	go func() {
		defer wg1.Done()
		t := time.Now()
		defn = fetchDefinition(word)
		tDefn = time.Since(t)
	}()
	go func() {
		// Single en.wiktionary.org call extracts both etymology and EN synonyms.
		defer wg1.Done()
		t := time.Now()
		etym, synSource = fetchEnWiktionary(word)
		tWiki = time.Since(t)
	}()
	for i, lang := range translateLangs {
		i, lang := i, lang
		go func() {
			defer wg1.Done()
			t := time.Now()
			wordTrans[i] = fetchTranslation(word, lang)
			tWordTrans[i] = time.Since(t)
		}()
	}
	wg1.Wait()

	// ── Validate languages: drop any whose word translation fully failed ───────
	var warnings []string
	{
		var validLangs []string
		var validTrans []*Translation
		for i, lang := range translateLangs {
			if wordTrans[i] == nil {
				warnings = append(warnings, fmt.Sprintf("language %q not recognised — falling back to English", lang))
			} else {
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
		synTargets     = make([][]string, len(translateLangs))
		defnTranslated string
		etymTranslated string
		tSynTargets    = make([]time.Duration, len(translateLangs))
		tDefnTrans     time.Duration
		tEtymTrans     time.Duration
	)

	var wg2 sync.WaitGroup
	wg2.Add(len(translateLangs) + 2)

	for i, lang := range translateLangs {
		i, lang := i, lang
		go func() {
			defer wg2.Done()
			if wordTrans[i] != nil && wordTrans[i].Word != "" {
				t := time.Now()
				synTargets[i] = fetchTargetSynonyms(wordTrans[i].Word, lang)
				tSynTargets[i] = time.Since(t)
			}
		}()
	}
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

	// Clear the "looking up…" line
	fmt.Printf("\033[1A\033[2K")

	var fetchLog []string
	if debug {
		fetchLog = append(fetchLog,
			fmt.Sprintf("phase 1 (parallel):"),
			fmt.Sprintf("  definition   %dms", tDefn.Milliseconds()),
			fmt.Sprintf("  wiktionary   %dms", tWiki.Milliseconds()),
		)
		for i, lang := range translateLangs {
			fetchLog = append(fetchLog, fmt.Sprintf("  trans(%s)     %dms", lang, tWordTrans[i].Milliseconds()))
		}
		fetchLog = append(fetchLog, fmt.Sprintf("phase 2 (parallel):"))
		for i, lang := range translateLangs {
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

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		printHelp()
		return
	}
	word := strings.TrimSpace(os.Args[1])
	debug := false
	var translateLangs []string
	for _, a := range os.Args[2:] {
		if a == "-d" {
			debug = true
			continue
		}
		l := strings.ToLower(strings.TrimSpace(a))
		if l != "en" {
			translateLangs = append(translateLangs, l)
		}
	}
	run(word, translateLangs, debug)
}
