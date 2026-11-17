// patai-transcribe is OpenAI's /v1/audio/transcriptions for the speech-to-text service (docs/apps.md, Voice), with the
// model the owner chose. Whisper, a .bin, runs in whisper.cpp's own server on the GPU, which this becomes. Whistle, a
// .cact, runs in Cactus's engine on the CPU, a process for each recording, behind this.
//
//	patai-transcribe <model>
//
//	POST /v1/audio/transcriptions    a form: the recording as `file`, `language`, and `response_format`: json or text
//	GET  /health                     200 once the model has heard a second of silence
//
// Each recording goes to FFmpeg for 16 kHz mono WAV first: browsers record Opus in WebM or Ogg, and AAC in MP4. One at
// a time, as each takes 4 cores. Nothing logs what was said.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tmp = "/tmp"
	// A recording, as OpenAI's API takes it.
	largest = 25 << 20
	longest = 10 * time.Minute
)

// Whistle's languages. Another one named is found in the recording, as if none were.
var languages = map[string]bool{"en": true, "de": true, "fr": true, "es": true, "it": true, "nl": true, "pl": true}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: patai-transcribe <model>")
		os.Exit(2)
	}
	model := os.Args[1]
	if !strings.HasSuffix(model, ".cact") {
		// The language found in each recording unless a request names it; --convert has FFmpeg read what a browser
		// recorded.
		err := syscall.Exec("/usr/local/bin/whisper-server", []string{"whisper-server", "--host", "0.0.0.0",
			"--port", "8080", "-m", model, "--inference-path", "/v1/audio/transcriptions", "--convert",
			"--tmp-dir", tmp, "-l", "auto", "-t", "4"}, os.Environ())
		log.Fatalf("couldn't start whisper.cpp: %v", err)
	}
	whistle := &engine{model: model}
	if err := whistle.warm(); err != nil {
		log.Fatalf("Whistle didn't start: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/audio/transcriptions", whistle.transcriptions)
	log.Printf("Whistle on :8080 with %s", filepath.Base(model))
	log.Fatal(http.ListenAndServe(":8080", mux))
}

type engine struct {
	model string
	busy  sync.Mutex
}

// What Cactus prints of a recording, last, as a line of JSON.
type heard struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

// warm has the engine hear a second of silence, so the server is ready only once it works.
func (e *engine) warm() error {
	silence := filepath.Join(tmp, "silence.wav")
	if err := os.WriteFile(silence, wav(make([]byte, 32000)), 0o600); err != nil {
		return err
	}
	defer os.Remove(silence)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := e.hear(ctx, silence, "")
	return err
}

func (e *engine) hear(ctx context.Context, recording, language string) (heard, error) {
	e.busy.Lock()
	defer e.busy.Unlock()
	args := []string{"--model", e.model, "--audio", recording}
	if languages[language] {
		args = append(args, "--audio-language", language)
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/local/bin/cactus", args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return heard{}, fmt.Errorf("cactus: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var got heard
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		return heard{}, errors.New("cactus gave no transcript")
	}
	got.Text = strings.TrimSpace(got.Text)
	return got, nil
}

func (e *engine) transcriptions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, largest)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			refuse(w, http.StatusRequestEntityTooLarge, "A recording can be 25 MB at most.")
			return
		}
		refuse(w, http.StatusBadRequest, "Send the recording as a form's file.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	format := r.FormValue("response_format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "text" {
		refuse(w, http.StatusBadRequest, "response_format can be json or text.")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		refuse(w, http.StatusBadRequest, "Send the recording as the form's file.")
		return
	}
	defer file.Close()
	dir, err := os.MkdirTemp(tmp, "recording-")
	if err != nil {
		refuse(w, http.StatusInternalServerError, "Couldn't keep the recording.")
		return
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(r.Context(), longest)
	defer cancel()
	sent, recording := filepath.Join(dir, "sent"), filepath.Join(dir, "recording.wav")
	if err := keep(file, sent); err != nil {
		refuse(w, http.StatusInternalServerError, "Couldn't keep the recording.")
		return
	}
	convert := exec.CommandContext(ctx, "/usr/local/bin/ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", sent, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", recording)
	if err := convert.Run(); err != nil {
		refuse(w, http.StatusBadRequest, "That isn't a recording this can read: WebM, Ogg, MP4, MP3, FLAC or WAV.")
		return
	}
	got, err := e.hear(ctx, recording, strings.ToLower(r.FormValue("language")))
	if err != nil {
		log.Printf("a recording wasn't transcribed: %v", err)
		refuse(w, http.StatusInternalServerError, "Couldn't transcribe the recording.")
		return
	}
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, got.Text+"\n")
		return
	}
	answer(w, http.StatusOK, map[string]string{"text": got.Text})
}

func keep(from io.Reader, path string) error {
	to, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(to, from); err != nil {
		to.Close()
		return err
	}
	return to.Close()
}

// wav is 16 kHz mono 16-bit PCM in a RIFF header.
func wav(samples []byte) []byte {
	var head bytes.Buffer
	head.WriteString("RIFF")
	binary.Write(&head, binary.LittleEndian, uint32(36+len(samples)))
	head.WriteString("WAVEfmt ")
	for _, field := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		binary.Write(&head, binary.LittleEndian, field)
	}
	head.WriteString("data")
	binary.Write(&head, binary.LittleEndian, uint32(len(samples)))
	return append(head.Bytes(), samples...)
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// refuse answers in OpenAI's shape.
func refuse(w http.ResponseWriter, status int, message string) {
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	answer(w, status, map[string]map[string]string{"error": {"message": message, "type": kind}})
}
