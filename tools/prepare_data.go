// tools/prepare_data.go — one-time data preparation script (not shipped).
//
// Downloads the Kaikki.org JSONL slice for a language, extracts the fields
// lexify needs, and writes a gzip-compressed gob-encoded binary to
// ../data/<lang>.bin.gz.
//
// Run from the repo root:
//
//	go run ./tools/prepare_data.go                   # English pack
//	go run ./tools/prepare_data.go -lang de           # German pack
//	go run ./tools/prepare_data.go -url <custom-url>  # custom URL
//
// Output files written to data/:
//
//	<lang>.bin.gz   compact indexed binary
//	<lang>.version  source URL + build timestamp
package main

import (
	"bufio"
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Types (must match main.go exactly for gob round-trip) ────────────────────

type Def struct {
	Text    string
	Example string
}

type Meaning struct {
	POS  string
	Defs []Def
	Syns []string
}

type LexEntry struct {
	IPA      string
	Meanings []Meaning
	Syns     []string
	Etym     string
}

// ── Kaikki JSONL schema ───────────────────────────────────────────────────────

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
		Tags []string `json:"tags"`
	} `json:"senses"`
	Synonyms []struct {
		Word string `json:"word"`
	} `json:"synonyms"`
	EtymologyText string `json:"etymology_text"`
	LangCode      string `json:"lang_code"`
}

// ── Conversion ────────────────────────────────────────────────────────────────

func convert(k kEntry) (string, LexEntry) {
	key := strings.ToLower(strings.TrimSpace(k.Word))

	// IPA: prefer /…/ notation, take first match.
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
	var defs []Def
	seenSyn := map[string]bool{}
	var senseSyns []string

	for _, s := range k.Senses {
		if len(s.Glosses) == 0 {
			continue
		}
		example := ""
		if len(s.Examples) > 0 {
			example = s.Examples[0].Text
		}
		defs = append(defs, Def{Text: s.Glosses[0], Example: example})
		for _, syn := range s.Synonyms {
			w := strings.TrimSpace(syn.Word)
			if w != "" && !seenSyn[w] {
				seenSyn[w] = true
				senseSyns = append(senseSyns, w)
			}
		}
	}

	// Entry-level synonyms (union with sense-level).
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

	etym := strings.TrimSpace(k.EtymologyText)
	return key, LexEntry{IPA: ipa, Meanings: meanings, Syns: senseSyns, Etym: etym}
}

// ── Merge: combine multiple entries for the same canonical word ───────────────

func merge(existing, incoming LexEntry) LexEntry {
	existing.Meanings = append(existing.Meanings, incoming.Meanings...)
	seen := map[string]bool{}
	for _, s := range existing.Syns {
		seen[s] = true
	}
	for _, s := range incoming.Syns {
		if !seen[s] {
			seen[s] = true
			existing.Syns = append(existing.Syns, s)
		}
	}
	if existing.IPA == "" {
		existing.IPA = incoming.IPA
	}
	if existing.Etym == "" {
		existing.Etym = incoming.Etym
	}
	return existing
}

// ── Download ──────────────────────────────────────────────────────────────────

func download(rawURL, dest string) error {
	fmt.Printf("  downloading %s\n  → %s\n", rawURL, dest)
	resp, err := http.Get(rawURL) //nolint:gosec
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, resp.Body)
	fmt.Printf("  wrote %.1f MB\n", float64(n)/(1024*1024))
	return err
}

// ── Main ──────────────────────────────────────────────────────────────────────

// langNames maps BCP-47 codes to Kaikki's English language name component.
var langNames = map[string]string{
	"de": "German", "fr": "French", "es": "Spanish", "it": "Italian",
	"pt": "Portuguese", "ru": "Russian", "ja": "Japanese", "zh": "Chinese",
	"ko": "Korean", "nl": "Dutch", "pl": "Polish", "sv": "Swedish",
	"ar": "Arabic", "tr": "Turkish", "uk": "Ukrainian", "hi": "Hindi",
}

func main() {
	lang := flag.String("lang", "en", "language code (en, de, fr, …)")
	customURL := flag.String("url", "", "override download URL")
	skipDownload := flag.Bool("skip-download", false, "skip download, reuse existing JSONL")
	flag.Parse()

	// Resolve data/ relative to the working directory (always the repo root).
	repoRoot, err := os.Getwd()
	if err != nil {
		fatalf("getwd: %v", err)
	}
	dataDir := filepath.Join(repoRoot, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fatalf("mkdir data: %v", err)
	}

	jsonlPath := filepath.Join(dataDir, *lang+".jsonl")
	binPath := filepath.Join(dataDir, *lang+".bin.gz")
	verPath := filepath.Join(dataDir, *lang+".version")

	// ── Step 1: download ──────────────────────────────────────────────────────
	dlURL := *customURL
	if dlURL == "" {
		if *lang == "en" {
			dlURL = "https://kaikki.org/dictionary/English/kaikki.org-dictionary-English.jsonl"
		} else {
			name, ok := langNames[*lang]
			if !ok {
				fatalf("unknown lang %q — use -url to provide the kaikki.org JSONL URL directly", *lang)
			}
			dlURL = fmt.Sprintf(
				"https://kaikki.org/dictionary/%s/kaikki.org-dictionary-%s.jsonl",
				name, name,
			)
		}
	}

	if !*skipDownload {
		if err := download(dlURL, jsonlPath); err != nil {
			fatalf("download: %v", err)
		}
	} else {
		fmt.Printf("  skipping download, using %s\n", jsonlPath)
	}

	// ── Step 2: stream-parse JSONL ────────────────────────────────────────────
	fmt.Printf("  parsing %s …\n", jsonlPath)
	t0 := time.Now()

	f, err := os.Open(jsonlPath)
	if err != nil {
		fatalf("open jsonl: %v", err)
	}
	defer f.Close()

	index := make(map[string]LexEntry, 1_000_000)
	scanner := bufio.NewScanner(f)
	const maxScanBuf = 4 * 1024 * 1024 // 4 MB per-line buffer
	scanner.Buffer(make([]byte, maxScanBuf), maxScanBuf)

	var total, skipped int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var k kEntry
		if err := json.Unmarshal(line, &k); err != nil {
			skipped++
			continue
		}
		// Keep only entries in the target language.
		if k.LangCode != *lang {
			continue
		}
		if strings.TrimSpace(k.Word) == "" {
			continue
		}
		key, entry := convert(k)
		if existing, ok := index[key]; ok {
			index[key] = merge(existing, entry)
		} else {
			index[key] = entry
		}
		total++
		if total%100_000 == 0 {
			fmt.Printf("    %d entries indexed…\n", total)
		}
	}
	if err := scanner.Err(); err != nil {
		fatalf("scan: %v", err)
	}
	fmt.Printf("  parsed %d entries (%d skipped) in %.1fs\n",
		total, skipped, time.Since(t0).Seconds())

	// ── Step 3: serialise ─────────────────────────────────────────────────────
	fmt.Printf("  writing %s …\n", binPath)
	out, err := os.Create(binPath)
	if err != nil {
		fatalf("create bin: %v", err)
	}
	defer out.Close()

	gw, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		fatalf("gzip writer: %v", err)
	}
	if err := gob.NewEncoder(gw).Encode(index); err != nil {
		fatalf("gob encode: %v", err)
	}
	if err := gw.Close(); err != nil {
		fatalf("gzip close: %v", err)
	}
	stat, _ := out.Stat()
	fmt.Printf("  wrote %.1f MB (%d unique words)\n",
		float64(stat.Size())/(1024*1024), len(index))

	// ── Step 4: version file ──────────────────────────────────────────────────
	ver := fmt.Sprintf("source: %s\nbuilt:  %s\n", dlURL, time.Now().Format(time.RFC3339))
	if err := os.WriteFile(verPath, []byte(ver), 0o644); err != nil {
		fatalf("write version: %v", err)
	}
	fmt.Printf("  wrote %s\n", verPath)
	fmt.Println("done.")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "prepare_data: "+format+"\n", args...)
	os.Exit(1)
}
