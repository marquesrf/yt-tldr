// yt-tldr downloads a YouTube video's auto-generated transcript via yt-dlp,
// cleans it up, and asks a local Ollama model for a TL;DR.
package main

import (
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

const defaultDir = "/Users/marquesrf/Documents/youtube-videos-transcripts"

var tagRE = regexp.MustCompile(`<[^>]+>`)

func main() {
	model := flag.String("model", "gemma4:e4b", "ollama model to use")
	dir := flag.String("dir", defaultDir, "directory to read/write transcript and summary files")
	file := flag.String("file", "", "path to an already-downloaded .vtt file to summarize (skips download)")
	flag.Parse()

	switch {
	case *file != "":
		if err := processVTT(*file, *model); err != nil {
			fatal("%v", err)
		}

	case flag.NArg() == 1:
		url := flag.Arg(0)
		vttPath, err := downloadTranscript(url, *dir)
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
		if !strings.HasSuffix(e.Name(), ".en.vtt") {
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
	return strings.TrimSuffix(vttPath, ".en.vtt") + ".summary.md"
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

// downloadTranscript runs yt-dlp to fetch the English auto-sub as .vtt into dir,
// then returns the path of the newly created file.
func downloadTranscript(url, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	before := existingVTTs(dir)

	cmd := exec.Command("yt-dlp",
		"--write-auto-sub", "--skip-download", "--sub-lang", "en",
		"-P", dir,
		url,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	after := existingVTTs(dir)
	for path := range after {
		if !before[path] {
			return path, nil
		}
	}
	// fall back: newest .vtt file, in case it already existed (re-run on same video)
	return newestVTT(dir)
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

func summarize(transcript, model string) (string, error) {
	prompt := fmt.Sprintf(promptTemplate, transcript)

	reqBody, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return "", err
	}

	resp, err := http.Post("http://localhost:11434/api/generate", "application/json", bytes.NewReader(reqBody))
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
