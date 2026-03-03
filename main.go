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
}

// etymTemplates: structure is {{name|dest|src|word|gloss?}} — word is parts[3]
var etymTemplates = map[string]bool{
	"inh": true, "inh+": true,
	"bor": true, "bor+": true,
	"der": true, "der+": true,
	"cog": true, "noncog": true,
	"inherited": true, "borrowed": true, "derived": true,
}

// combinerTemplates produce "A + -B" or "A- + B" display text.
var combinerTemplates = map[string]bool{
	"suffix": true, "prefix": true, "confix": true, "compound": true, "affix": true,
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

	if mentionTemplates[name] {
		// {{m|lang|word|gloss?}} — word is parts[2]
		if len(parts) >= 3 {
			if w := strings.TrimSpace(parts[2]); w != "" {
				return w
			}
		}
		if len(parts) >= 4 {
			return strings.TrimSpace(parts[3])
		}
		return ""
	}

	if etymTemplates[name] {
		// {{der|dest|src|word|gloss?}} — word is parts[3], gloss parts[4]
		if len(parts) >= 4 {
			if w := strings.TrimSpace(parts[3]); w != "" {
				return w
			}
		}
		if len(parts) >= 5 {
			return strings.TrimSpace(parts[4])
		}
		return ""
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

// fetchEtymology uses the stable action=parse two-step API:
// 1. Get section list, find "Etymology" index.
// 2. Fetch that section's wikitext and strip markup.
func fetchEtymology(word string) string {
	// Step 1: section index
	var secResp struct {
		Parse struct {
			Sections []struct {
				Line  string `json:"line"`
				Index string `json:"index"`
			} `json:"sections"`
		} `json:"parse"`
	}
	secURL := "https://en.wiktionary.org/w/api.php?action=parse&page=" +
		url.QueryEscape(word) + "&prop=sections&format=json"
	if err := fetchJSON(secURL, &secResp); err != nil {
		return ""
	}
	idx := ""
	for _, s := range secResp.Parse.Sections {
		if strings.EqualFold(s.Line, "Etymology") || strings.HasPrefix(strings.ToLower(s.Line), "etymology") {
			idx = s.Index
			break
		}
	}
	if idx == "" {
		return ""
	}

	// Step 2: wikitext of that section
	var wtResp struct {
		Parse struct {
			Wikitext struct {
				Text string `json:"*"`
			} `json:"wikitext"`
		} `json:"parse"`
	}
	wtURL := "https://en.wiktionary.org/w/api.php?action=parse&page=" +
		url.QueryEscape(word) + "&prop=wikitext&section=" + idx + "&format=json"
	if err := fetchJSON(wtURL, &wtResp); err != nil {
		return ""
	}
	clean := stripWikitext(wtResp.Parse.Wikitext.Text)
	// Drop the section heading line itself (===Etymology===)
	if i := strings.Index(clean, "\n"); i != -1 {
		clean = strings.TrimSpace(clean[i+1:])
	}
	if len(clean) > 700 {
		clean = clean[:700]
	}
	return clean
}

// fetchGTX hits the undocumented Google Translate gtx endpoint.
// Response is a heterogeneous JSON array; we unmarshal to []json.RawMessage.
func fetchGTX(word, lang string) *Translation {
	endpoint := "https://translate.google.com/translate_a/single" +
		"?client=gtx&sl=auto&tl=" + url.QueryEscape(lang) +
		"&dt=t&dt=bd&q=" + url.QueryEscape(word)

	var raw []json.RawMessage
	if err := fetchJSON(endpoint, &raw); err != nil || len(raw) == 0 {
		return nil
	}

	// data[0]: [[translated_chunk, original, ...], ...]
	var segments [][]json.RawMessage
	translated := ""
	if err := json.Unmarshal(raw[0], &segments); err == nil {
		for _, seg := range segments {
			if len(seg) > 0 {
				var s string
				if json.Unmarshal(seg[0], &s) == nil {
					translated += s
				}
			}
		}
	}
	translated = strings.TrimSpace(translated)

	// data[2]: detected language string
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
	endpoint := "https://translate.google.com/translate_a/single" +
		"?client=gtx&sl=auto&tl=" + url.QueryEscape(lang) +
		"&dt=t&q=" + url.QueryEscape(text)
	var raw []json.RawMessage
	if err := fetchJSON(endpoint, &raw); err != nil || len(raw) == 0 {
		return ""
	}
	var segments [][]json.RawMessage
	out := ""
	if err := json.Unmarshal(raw[0], &segments); err == nil {
		for _, seg := range segments {
			if len(seg) > 0 {
				var s string
				if json.Unmarshal(seg[0], &s) == nil {
					out += s
				}
			}
		}
	}
	return strings.TrimSpace(out)
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

	// Find synonym section index in target-language Wiktionary
	var secResp struct {
		Parse struct {
			Sections []struct {
				Line  string `json:"line"`
				Index string `json:"index"`
			} `json:"sections"`
		} `json:"parse"`
	}
	wikiBase := "https://" + lang + ".wiktionary.org/w/api.php"
	secURL := wikiBase + "?action=parse&page=" + url.QueryEscape(word) + "&prop=sections&format=json"
	if err := fetchJSON(secURL, &secResp); err != nil {
		return nil
	}

	synIdx := ""
	for _, s := range secResp.Parse.Sections {
		if synHeadingPat.MatchString(s.Line) {
			synIdx = s.Index
			break
		}
	}
	if synIdx == "" {
		return nil
	}

	var wtResp struct {
		Parse struct {
			Wikitext struct {
				Text string `json:"*"`
			} `json:"wikitext"`
		} `json:"parse"`
	}
	wtURL := wikiBase + "?action=parse&page=" + url.QueryEscape(word) +
		"&prop=wikitext&section=" + synIdx + "&format=json"
	if err := fetchJSON(wtURL, &wtResp); err != nil {
		return nil
	}

	// Extract [[word]] or [[word|display]] from wikitext — these are the synonyms
	linkRe := regexp.MustCompile(`\[\[(?:[^\]|]+\|)?([^\]|]+)\]\]`)
	matches := linkRe.FindAllStringSubmatch(wtResp.Parse.Wikitext.Text, -1)

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

func run(word string, translateLangs []string) {
	fmt.Printf("\n  %slooking up %s%s%s%s…%s\r", CEx, Bold, word, R, CEx, R)

	// ── Phase 1: parallel source fetches + word translations ─────────────────
	var (
		mu        sync.Mutex
		defn      *Definition
		synSource []string
		etym      string
		wordTrans = make([]*Translation, len(translateLangs))
	)

	var wg1 sync.WaitGroup
	wg1.Add(3 + len(translateLangs))

	go func() {
		defer wg1.Done()
		defn = fetchDefinition(word)
	}()
	go func() {
		defer wg1.Done()
		s := fetchSynonyms(word)
		mu.Lock()
		synSource = s
		mu.Unlock()
	}()
	go func() {
		defer wg1.Done()
		e := fetchEtymology(word)
		mu.Lock()
		etym = e
		mu.Unlock()
	}()
	for i, lang := range translateLangs {
		i, lang := i, lang
		go func() {
			defer wg1.Done()
			t := fetchTranslation(word, lang)
			mu.Lock()
			wordTrans[i] = t
			mu.Unlock()
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
	)

	var wg2 sync.WaitGroup
	wg2.Add(len(translateLangs) + 2)

	for i, lang := range translateLangs {
		i, lang := i, lang
		go func() {
			defer wg2.Done()
			if wordTrans[i] != nil && wordTrans[i].Word != "" {
				s := fetchTargetSynonyms(wordTrans[i].Word, lang)
				mu.Lock()
				synTargets[i] = s
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer wg2.Done()
		if primaryLang != "" && defBlock != "" {
			t := fetchTextTranslation(defBlock, primaryLang)
			mu.Lock()
			defnTranslated = t
			mu.Unlock()
		}
	}()
	go func() {
		defer wg2.Done()
		if primaryLang != "" && etym != "" {
			t := fetchTextTranslation(etym, primaryLang)
			mu.Lock()
			etymTranslated = t
			mu.Unlock()
		}
	}()
	wg2.Wait()

	// Clear the "looking up…" line
	fmt.Printf("\033[1A\033[2K")

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
	})
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		printHelp()
		return
	}
	word := strings.TrimSpace(os.Args[1])
	var translateLangs []string
	for _, a := range os.Args[2:] {
		l := strings.ToLower(strings.TrimSpace(a))
		if l != "en" {
			translateLangs = append(translateLangs, l)
		}
	}
	run(word, translateLangs)
}
