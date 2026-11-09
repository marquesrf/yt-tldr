// yt-tldr downloads a YouTube video's auto-generated transcript via yt-dlp,
// cleans it up, and asks a local Ollama model for a TL;DR.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var tagRE = regexp.MustCompile(`<[^>]+>`)

// defaultDir is where transcripts and summaries live. Override with the
// YT_TLDR_DIR env var or the -dir flag; falls back to ~/Documents/
// youtube-videos-transcripts, or the current directory if $HOME is unknown.
func defaultDir() string {
	if d := os.Getenv("YT_TLDR_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "transcripts"
	}
	return filepath.Join(home, "Documents", "youtube-videos-transcripts")
}

func main() {
	model := flag.String("model", "gemma4:12b", "ollama model to use")
	dir := flag.String("dir", defaultDir(), "directory to read/write transcript and summary files")
	file := flag.String("file", "", "path to an already-downloaded .vtt file to summarize (skips download)")
	lang := flag.String("lang", "", "transcript language code (e.g. en, pt); if unset, prompts, defaulting to English")
	flag.Parse()

	switch {
	case *file != "":
		if err := processVTT(*file, *model); err != nil {
			fatal("%v", err)
		}

	case flag.NArg() == 1:
		url := flag.Arg(0)
		vttPath, err := downloadTranscript(url, *dir, resolveLang(*lang))
		if err != nil {
			fatal("download transcript: %v", err)
		}
		fmt.Println("downloaded:", vttPath)
		if err := processVTT(vttPath, *model); err != nil {
			fatal("%v", err)
		}

	case flag.NArg() == 0:
		if err := processMissingSummaries(*dir, *model); err != nil {
			fatal("%v", err)
		}

	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  yt-tldr [-model NAME] [-dir PATH] <youtube-url>   download + summarize")
	fmt.Fprintln(os.Stderr, "  yt-tldr [-model NAME] -file <path.vtt>            summarize an existing .vtt")
	fmt.Fprintln(os.Stderr, "  yt-tldr [-model NAME] [-dir PATH]                 summarize every .vtt in dir missing a .summary.md")
	os.Exit(1)
}

// processMissingSummaries scans dir for .en.vtt transcripts that don't yet
// have a matching .summary.md, and summarizes each one.
func processMissingSummaries(dir, model string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var pending []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".vtt") {
			continue
		}
		vttPath := filepath.Join(dir, e.Name())
		if _, err := os.Stat(summaryPathFor(vttPath)); err == nil {
			continue // summary already exists
		}
		pending = append(pending, vttPath)
	}

	if len(pending) == 0 {
		fmt.Println("nothing to do: every transcript already has a summary")
		return nil
	}

	for _, vttPath := range pending {
		if err := processVTT(vttPath, model); err != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", vttPath, err)
		}
	}
	return nil
}

func summaryPathFor(vttPath string) string {
	return strings.TrimSuffix(vttPath, ".vtt") + ".summary.md"
}

// processVTT cleans a transcript file, summarizes it, and writes the result.
func processVTT(vttPath, model string) error {
	fmt.Println("processing:", vttPath)

	clean, err := cleanVTT(vttPath)
	if err != nil {
		return fmt.Errorf("clean transcript: %w", err)
	}

	fmt.Println("asking", model, "for tl;dr...")
	summary, err := summarize(clean, model)
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}

	summaryPath := summaryPathFor(vttPath)
	if err := os.WriteFile(summaryPath, []byte(summary), 0644); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	fmt.Println("summary written:", summaryPath)
	return nil
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// resolveLang decides which transcript language to request. An explicit -lang
// flag wins. Otherwise, on an interactive terminal, it asks (default English);
// when stdin isn't a terminal (piped/cron) it silently defaults to English.
func resolveLang(flagLang string) string {
	if l := strings.TrimSpace(flagLang); l != "" {
		return l
	}
	if !isTerminal(os.Stdin) {
		return "en"
	}
	fmt.Print("Transcript language — default is English [en].\n" +
		"Enter a language code (e.g. pt, es, fr) or press Enter for English: ")
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			return l
		}
	}
	return "en"
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// langCandidates lists subtitle tracks to try for the chosen language, in
// priority order: the manual/original track first, then the auto-generated one
// (which for a translated track YouTube rate-limits with HTTP 429). English is
// appended as a fallback when another language was requested. Each entry is a
// concrete code — never a regex — so yt-dlp downloads one track, not every
// original-language caption the video has.
func langCandidates(lang string) []string {
	c := []string{lang + "-orig", lang}
	if lang != "en" {
		c = append(c, "en-orig", "en")
	}
	return c
}

// downloadTranscript runs yt-dlp to fetch an auto/manual subtitle as .vtt into
// dir, trying each candidate track for lang until one produces a file, then
// returns that file's path.
func downloadTranscript(url, dir, lang string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	candidates := langCandidates(lang)
	var lastErr error
	for _, sub := range candidates {
		before := existingVTTs(dir)

		cmd := exec.Command("yt-dlp",
			"--write-auto-sub", "--write-sub", "--skip-download",
			"--sub-langs", sub,
			"--retries", "10", "--retry-sleep", "5",
			"-P", dir,
			url,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// A 429 on a throttled (translated) track exits non-zero; remember it
		// and fall through to the next candidate rather than giving up.
		lastErr = cmd.Run()

		after := existingVTTs(dir)
		for path := range after {
			if !before[path] {
				return path, nil
			}
		}
	}

	// Nothing new written. Maybe the file already exists (re-run on same video).
	if path, err := newestVTT(dir); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("yt-dlp wrote no subtitle for languages [%s]; last error: %w",
		strings.Join(candidates, ", "), lastErr)
}

func existingVTTs(dir string) map[string]bool {
	set := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".vtt") {
			set[filepath.Join(dir, e.Name())] = true
		}
	}
	return set
}

func newestVTT(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	type f struct {
		path string
		mod  time.Time
	}
	var vtts []f
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".vtt") {
			info, err := e.Info()
			if err != nil {
				continue
			}
			vtts = append(vtts, f{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	if len(vtts) == 0 {
		return "", fmt.Errorf("no .vtt file found in %s", dir)
	}
	sort.Slice(vtts, func(i, j int) bool { return vtts[i].mod.After(vtts[j].mod) })
	return vtts[0].path, nil
}

// cleanVTT strips VTT timing/header lines and rolling-caption duplicates,
// returning the plain transcript text.
func cleanVTT(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	var out []string
	seen := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "WEBVTT") ||
			strings.HasPrefix(line, "Kind:") || strings.HasPrefix(line, "Language:") ||
			strings.Contains(line, "-->") {
			continue
		}
		clean := strings.TrimSpace(tagRE.ReplaceAllString(line, ""))
		if clean == "" || clean == seen {
			continue
		}
		out = append(out, clean)
		seen = clean
	}
	return strings.Join(out, " "), nil
}

const promptTemplate = `You are summarizing a YouTube video transcript. Produce a clear, well-structured TL;DR in markdown: a short overview, then the key points/steps as bullet points or numbered lists, grouped under headings where it makes sense. Be concise but complete - a reader should not need the full transcript afterward. Do not invent facts not present in the transcript.

Transcript:
%s

TL;DR (markdown):`

// contextSizeFor estimates a token budget (~4 chars/token) for prompt plus
// headroom for the response, so long transcripts don't starve generation.
func contextSizeFor(prompt string) int {
	tokens := len(prompt)/4 + 1024
	if tokens < 4096 {
		return 4096
	}
	return tokens
}

func summarize(transcript, model string) (string, error) {
	prompt := fmt.Sprintf(promptTemplate, transcript)

	reqBody, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
		"think":  false,
		"options": map[string]any{
			"num_ctx": contextSizeFor(prompt),
		},
	})
	if err != nil {
		return "", err
	}

	// Generation on a big model + long transcript can take minutes; cap it so a
	// hung Ollama doesn't block forever.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("calling ollama (is it running?): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned %s: %s", resp.Status, body)
	}

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Response, nil
}
