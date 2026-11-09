package main

import (
	"os"
	"path/filepath"
	"testing"
)

// cleanVTT must strip the WEBVTT header, timing cues and inline tags, and drop
// the consecutive-duplicate lines that YouTube's rolling captions produce.
func TestCleanVTT(t *testing.T) {
	raw := `WEBVTT
Kind: captions
Language: en

00:00:00.000 --> 00:00:02.000
Hello <c>world</c>

00:00:02.000 --> 00:00:04.000
Hello world

00:00:04.000 --> 00:00:06.000
this is a test
`
	path := filepath.Join(t.TempDir(), "sample.en.vtt")
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := cleanVTT(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "Hello world this is a test"
	if got != want {
		t.Fatalf("cleanVTT:\n got: %q\nwant: %q", got, want)
	}
}

func TestContextSizeFor(t *testing.T) {
	if n := contextSizeFor("short"); n != 4096 {
		t.Errorf("short prompt: got %d, want floor 4096", n)
	}
	long := make([]byte, 40000) // ~10k tokens
	if n := contextSizeFor(string(long)); n <= 4096 {
		t.Errorf("long prompt: got %d, want > 4096", n)
	}
}
