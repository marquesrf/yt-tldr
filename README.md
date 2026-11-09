# yt-tldr

Turn any YouTube video into a clean, structured TL;DR — **100% locally, no API keys, no cloud, no cost.**

It grabs a video's captions with [`yt-dlp`](https://github.com/yt-dlp/yt-dlp), strips the timing junk, and asks a local [Ollama](https://ollama.com) model for a markdown summary. The transcript and the summary are saved side by side so you build a searchable library of everything you watch.

```
$ yt-tldr "https://youtu.be/9nySJBfaIh4"
downloaded: .../Selling EBooks with Claude Changed My Life [9nySJBfaIh4].en-orig.vtt
processing: ...
asking gemma3:12b for tl;dr...
summary written: .../Selling EBooks with Claude Changed My Life [9nySJBfaIh4].en-orig.summary.md
```

The `.summary.md` is a ready-to-read overview with headings and bullet points — no need to open the video again.

## Why

Watching a 20-minute video to extract two minutes of signal is a bad trade. Paid "AI summarizer" sites charge per video and send your viewing history to a third party. `yt-tldr` runs entirely on your machine: your data never leaves it, and the marginal cost per video is zero.

## Requirements

- [Go](https://go.dev) 1.21+ (to build)
- [`yt-dlp`](https://github.com/yt-dlp/yt-dlp) on your `PATH` — `brew install yt-dlp`
- [Ollama](https://ollama.com) running locally with a model pulled — e.g. `ollama pull gemma3:12b`

> Install `ffmpeg` too (`brew install ffmpeg`) to silence a yt-dlp warning. Not required for subtitles.

## Install

```sh
git clone https://github.com/marquesrf/yt-tldr.git
cd yt-tldr
go build -o yt-tldr .
```

## Usage

```sh
# Download a video's transcript and summarize it
yt-tldr "https://youtu.be/VIDEO_ID"

# Summarize a .vtt you already have (skips download)
yt-tldr -file "path/to/transcript.en.vtt"

# Summarize every transcript in the folder that doesn't have a summary yet
yt-tldr

# Pick a different model
yt-tldr -model qwen2.5:14b "https://youtu.be/VIDEO_ID"
```

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-model` | `gemma4:12b` | Ollama model to use. Change to whatever you've pulled (`ollama list`). |
| `-dir`   | `$YT_TLDR_DIR`, else `~/Documents/youtube-videos-transcripts` | Where transcripts and summaries are read/written. |
| `-file`  | — | Summarize an existing `.vtt` instead of downloading. |

Set the output folder once instead of passing `-dir` every time:

```sh
export YT_TLDR_DIR="$HOME/notes/yt"
```

## How it works

1. **Download** — tries subtitle languages in priority order: `en-orig` (manual/original English) → `en` (auto-translation, which YouTube rate-limits with HTTP 429) → `*-orig` (the video's original language, which never gets throttled). A non-English transcript summarizes fine.
2. **Clean** — strips the `WEBVTT` header, timing cues, inline tags, and the duplicate lines that rolling captions produce, leaving plain text.
3. **Summarize** — sends the text to Ollama's `/api/generate` with a context window sized to the transcript length, and writes the markdown response to `<transcript>.summary.md`.

## Limitations

- Only works with videos that have captions (most do; auto-generated counts).
- Summary quality tracks the model you run. A 12B model is a good laptop default; smaller models are faster but blunter.
- Duplicate-line cleanup only drops *consecutive* exact repeats, so a little caption overlap can survive. It hasn't hurt summary quality in practice.

## License

MIT
