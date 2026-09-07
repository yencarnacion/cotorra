package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func cfgFor(url string) config {
	return config{Provider: "kokoro", BaseURL: url + "/v1", Model: "kokoro", Voice: "af_heart", Speed: 1, Timeout: 2 * time.Second, ChunkChars: 2000, Listen: "127.0.0.1:0"}
}
func fixture(t *testing.T, handler http.HandlerFunc) (*backend, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	b := newBackend(cfgFor(s.URL))
	t.Cleanup(b.close)
	return b, s
}
func pcmHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Audio-Sample-Rate", "24000")
	_, _ = w.Write(bytes.Repeat([]byte{1, 0}, 480))
}
func post(t *testing.T, url, body, accept, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestDotenv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	input := "\ufeff# test\nexport A=abc # comment\nB='has # hash'\nC=\"line\\nnext\"\nD=abc#key\nE=\nF=\"$A $(touch /tmp/nope)\"\n"
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := envFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "abc", "B": "has # hash", "C": "line\nnext", "D": "abc#key", "E": "", "F": "$A $(touch /tmp/nope)"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("%#v", env)
	}
	t.Setenv("COTORRA_TEST_PRECEDENCE", "process")
	if got := envValue(map[string]string{"COTORRA_TEST_PRECEDENCE": "file"}, "COTORRA_TEST_PRECEDENCE", ""); got != "process" {
		t.Fatal(got)
	}
	for _, bad := range []string{"MISSING_EQUALS", "KEY='secret", "1BAD=secret", "KEY=\"secret\"extra"} {
		_ = os.WriteFile(path, []byte(bad), 0600)
		_, err := envFile(path, true)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe/error missing: %v", err)
		}
	}
	if _, err := envFile(path+"missing", false); err != nil {
		t.Fatal(err)
	}
	if _, err := envFile(path+"missing", true); err == nil {
		t.Fatal("explicit missing env accepted")
	}
}

func TestProviderCredentialsAndDefaults(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "KOKORO_API_KEY", "API_KEY", "API_BASE_URL", "OPENAI_BASE_URL", "KOKORO_MODEL", "KOKORO_VOICE", "OPENAI_TTS_MODEL", "OPENAI_TTS_VOICE"} {
		old, ok := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if ok {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	env := map[string]string{"OPENAI_API_KEY": "cloud-secret", "API_KEY": "local-secret", "API_BASE_URL": "http://192.0.2.1:9084/v1"}
	c := config{Provider: "kokoro", Speed: 1, Timeout: time.Second, ChunkChars: 2000}
	if err := c.resolve(env); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "local-secret" || c.Model != "kokoro" || c.Voice != "af_heart" {
		t.Fatalf("%+v", c)
	}
	c = config{Provider: "openai", Speed: 1, Timeout: time.Second, ChunkChars: 2000}
	if err := c.resolve(env); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "cloud-secret" || c.BaseURL != defaultOpenAIURL || c.Voice != "coral" || c.Model != "gpt-4o-mini-tts" {
		t.Fatalf("bad cloud config: %s %s", c.BaseURL, c.Model)
	}
	c = config{Provider: "kokoro", Speed: 1, Timeout: time.Second, ChunkChars: 2000}
	if err := c.resolve(nil); err != nil || c.BaseURL != defaultBaseURL || c.APIKey != "" {
		t.Fatalf("default config: %v", err)
	}
	c = config{Provider: "openai", BaseURL: "http://192.0.2.1/v1", Speed: 1, Timeout: time.Second, ChunkChars: 2000}
	if err := c.resolve(env); err == nil {
		t.Fatal("cloud key allowed over LAN HTTP")
	}
	for _, address := range []string{":8099", "0.0.0.0:8099", "[::]:8099", "example.com:8099"} {
		if validateListen(address, "") == nil {
			t.Fatal(address)
		}
		if validateListen(address, "secret") != nil {
			t.Fatal(address)
		}
	}
	for _, address := range []string{"127.0.0.1:8099", "localhost:8099", "[::1]:8099"} {
		if err := validateListen(address, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSplitText(t *testing.T) {
	for _, text := range []string{"Hello. A second sentence! A verylongwordwithoutspaces.", "áéíóú ñ 中文 😀 words", "AAAAAAAAAAAAAAAAAAAAAAAAA"} {
		for limit := 1; limit <= 20; limit++ {
			chunks := splitText(text, limit)
			var combined string
			for _, s := range chunks {
				if !utf8.ValidString(s) || utf8.RuneCountInString(s) > limit {
					t.Fatalf("bad chunk %q", s)
				}
				combined += strings.Join(strings.Fields(s), "")
			}
			if combined != strings.Join(strings.Fields(text), "") {
				t.Fatalf("text dropped: %q %q", text, combined)
			}
		}
	}
}

func TestBackendContractAndLongText(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" || r.ContentLength <= 0 || r.Header.Get("Authorization") != "Bearer local-only" {
			t.Errorf("incorrect request contract: %s %d", r.URL.Path, r.ContentLength)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		pcmHandler(w, r)
	})
	b.cfg.APIKey = "local-only"
	b.cfg.ChunkChars = 8
	reader, err := b.stream(context.Background(), "Hello world. More words.", b.cfg.options())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 || len(data) != 960*len(bodies) {
		t.Fatalf("chunks=%d bytes=%d", len(bodies), len(data))
	}
	for _, body := range bodies {
		if len(body) != 6 || body["stream"] != true || body["response_format"] != "pcm" || body["model"] != "kokoro" {
			t.Fatalf("unsupported Kokoro fields: %#v", body)
		}
	}
}

func TestOpenAIContract(t *testing.T) {
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, exists := body["stream"]; exists {
			t.Error("sent Kokoro stream flag to OpenAI")
		}
		if body["instructions"] != "Speak calmly." || body["voice"] != "coral" || r.Header.Get("Authorization") != "Bearer cloud-only" {
			t.Errorf("bad OpenAI body: %#v", body)
		}
		pcmHandler(w, r)
	})
	b.cfg.Provider = "openai"
	b.cfg.APIKey = "cloud-only"
	b.cfg.Model = "gpt-4o-mini-tts"
	b.cfg.Voice = "coral"
	b.cfg.Instructions = "Speak calmly."
	reader, err := b.stream(context.Background(), "Hello world", b.cfg.options())
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBackendFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"auth", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad key secret-token", 401) }},
		{"busy", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "busy", 429) }},
		{"empty", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
		}},
		{"odd", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{1})
		}},
		{"wrong type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"error":"wrong"}`))
		}},
		{"wrong rate", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/pcm")
			w.Header().Set("X-Audio-Sample-Rate", "16000")
			_, _ = w.Write([]byte{1, 0})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := fixture(t, tc.handler)
			b.cfg.APIKey = "secret-token"
			reader, err := b.stream(context.Background(), "Hello", b.cfg.options())
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(reader)
			reader.Close()
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("missing or unsafe error: %v", err)
			}
		})
	}
}

func TestRedirectBlocked(t *testing.T) {
	var called atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	defer target.Close()
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) })
	b.cfg.APIKey = "secret-token"
	reader, _ := b.stream(context.Background(), "Hello", b.cfg.options())
	_, err := io.ReadAll(reader)
	reader.Close()
	if err == nil || called.Load() {
		t.Fatal("redirect followed")
	}
}

func TestCancellation(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		// Consume the request body so net/http can observe connection closure.
		io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
		close(stopped)
	})
	reader, err := b.stream(context.Background(), "Hello", b.cfg.options())
	if err != nil {
		t.Fatal(err)
	}
	<-started
	reader.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel upstream request")
	}
}

func TestWAVAndPlayerSelection(t *testing.T) {
	data, err := asWAV([]byte{1, 0, 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "RIFF" || binary.LittleEndian.Uint32(data[4:]) != 40 || binary.LittleEndian.Uint32(data[24:]) != 24000 || binary.LittleEndian.Uint32(data[40:]) != 4 {
		t.Fatal("bad WAV header")
	}
	if _, err := asWAV([]byte{1}); err == nil {
		t.Fatal("odd PCM accepted")
	}
	lookup := func(available string) func(string) (string, error) {
		return func(name string) (string, error) {
			if name == available {
				return "/bin/" + name, nil
			}
			return "", os.ErrNotExist
		}
	}
	for _, tc := range []struct {
		goos, available string
		stream          bool
	}{{"linux", "paplay", true}, {"linux", "aplay", true}, {"darwin", "afplay", false}, {"windows", "powershell", false}, {"windows", "ffplay", true}, {"darwin", "ffplay", true}} {
		p, err := playerFor(tc.goos, "auto", "auto", lookup(tc.available))
		if err != nil || p.Name != tc.available || p.Streaming != tc.stream {
			t.Fatalf("%+v %v", p, err)
		}
	}
	if _, err := playerFor("darwin", "auto", "on", lookup("afplay")); err == nil {
		t.Fatal("afplay selected for raw PCM")
	}
}

func TestCLIEndToEnd(t *testing.T) {
	_, s := fixture(t, pcmHandler)
	dir := t.TempDir()
	output := filepath.Join(dir, "out.wav")
	var out, log bytes.Buffer
	args := []string{"speak", "Hello from the CLI.", "--base-url", s.URL + "/v1", "--no-play", "--output", output}
	if err := run(context.Background(), args, strings.NewReader(""), &out, &log); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1004 || string(data[:4]) != "RIFF" || out.Len() != 0 {
		t.Fatalf("output length %d", len(data))
	}
	if err := run(context.Background(), args, strings.NewReader(""), &out, &log); err == nil {
		t.Fatal("overwrote existing output")
	}
	if err := run(context.Background(), append(args, "--force"), strings.NewReader(""), &out, &log); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	args = []string{"--base-url", s.URL + "/v1", "--format", "pcm", "--output", "-", "--quiet"}
	if err := run(context.Background(), args, strings.NewReader("Piped text"), &out, &log); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 960 {
		t.Fatal(out.Len())
	}
	textPath := filepath.Join(dir, "input.txt")
	_ = os.WriteFile(textPath, []byte("Text from a file."), 0600)
	args = []string{"--base-url", s.URL + "/v1", "--file", textPath, "--output", "-", "--quiet"}
	out.Reset()
	if err := run(context.Background(), args, strings.NewReader(""), &out, &log); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("RIFF")) {
		t.Fatal("file input failed")
	}
}

func TestCLIRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{{"--no-play", "Hello"}, {"--file", "input", "also text"}, {"--format", "mp3", "Hello"}, {"--bogus"}, {"--provider", "wrong", "Hello"}} {
		if err := run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	path, required, err := preliminaryEnv([]string{"--text", "--env-file=not-a-file"})
	if err != nil || required || path != ".env" {
		t.Fatal("text parsed as dotenv option")
	}
	if _, required, _ := preliminaryEnv([]string{"--", "--env-file", "not-a-file"}); required {
		t.Fatal("literal flag parsed")
	}
}

func TestHTTPFormatsAndAuth(t *testing.T) {
	b, _ := fixture(t, pcmHandler)
	b.cfg.ServerKey = "gateway-secret"
	gateway := httptest.NewServer(newHandler(b, 2))
	defer gateway.Close()
	for _, tc := range []struct{ accept, format string }{{"audio/wav", "wav"}, {"application/json", "json"}, {"audio/L16", "pcm"}, {"text/event-stream", "sse"}} {
		t.Run(tc.format, func(t *testing.T) {
			resp := post(t, gateway.URL+"/v1/rime-tts", `{"text":"Hello","speaker":"af_heart","modelId":"kokoro","samplingRate":24000,"speedAlpha":1}`, tc.accept, "gateway-secret")
			data, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%d %s %v", resp.StatusCode, data, err)
			}
			switch tc.format {
			case "wav":
				if len(data) != 1004 || string(data[:4]) != "RIFF" {
					t.Fatal("bad WAV")
				}
			case "pcm":
				if len(data) != 960 {
					t.Fatal("bad PCM")
				}
			case "json":
				var v struct {
					AudioContent string `json:"audioContent"`
				}
				if err := json.Unmarshal(data, &v); err != nil {
					t.Fatal(err)
				}
				wav, err := base64.StdEncoding.DecodeString(v.AudioContent)
				if err != nil || !bytes.HasPrefix(wav, []byte("RIFF")) {
					t.Fatal("bad base64 WAV")
				}
			case "sse":
				if !bytes.Contains(data, []byte("event: chunk")) || !bytes.Contains(data, []byte("event: done")) {
					t.Fatal("bad SSE")
				}
			}
		})
	}
	resp := post(t, gateway.URL+"/v1/audio/speech", `{"input":"Hello"}`, "audio/wav", "")
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
}

func TestHTTPValidation(t *testing.T) {
	b, _ := fixture(t, pcmHandler)
	gateway := httptest.NewServer(newHandler(b, 2))
	defer gateway.Close()
	cases := []struct {
		body, accept string
		status       int
	}{
		{`{"input":"Hello","unknown":true}`, "", 400},
		{`{"input":"Hello","speed":0}`, "", 400},
		{`{"input":"Hello","samplingRate":16000}`, "", 400},
		{`{"input":"Hello","modelId":"mistv2"}`, "", 400},
		{`{"input":"Hello","stream":true,"response_format":"wav"}`, "", 400},
		{`{"input":"Hello","response_format":"pcm"}`, "audio/wav", 400},
		{`{"input":"Hello","response_format":"mp3"}`, "", 400},
		{`{"input":"Hello","speed":1,"speedAlpha":1}`, "", 400},
		{`{"input":"Hola","speaker":"ef_dora","lang":"eng"}`, "", 400},
		{`{"input":"Hello"} {}`, "", 400},
		{`{"input":"Hello"}`, "audio/mpeg", 406},
	}
	for _, tc := range cases {
		resp := post(t, gateway.URL+"/v1/rime-tts", tc.body, tc.accept, "")
		if resp.StatusCode != tc.status {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s -> %d %s", tc.body, resp.StatusCode, data)
		}
	}
	alpha := 2.0
	_, opt, _, err := (apiRequest{Text: "Hello", TimeScaleFactor: &alpha}).normalize(b.cfg)
	if err != nil || opt.Speed != 0.5 {
		t.Fatalf("inverse time factor: %v %v", opt.Speed, err)
	}
	if media, err := negotiate("audio/mpeg;q=1, audio/pcm;q=0.8, audio/wav;q=0.2"); err != nil || media != "audio/pcm" {
		t.Fatal(media, err)
	}
}

func TestHTTPStreamsBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{1, 0})
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write([]byte{2, 0})
		case <-r.Context().Done():
		}
	})
	gateway := httptest.NewServer(newHandler(b, 2))
	defer gateway.Close()
	resp := post(t, gateway.URL+"/v1/audio/speech", `{"input":"Hello","response_format":"pcm"}`, "", "")
	first := make([]byte, 2)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || len(rest) != 2 {
		t.Fatal(rest, err)
	}
}

func TestHTTPTruncatedUpstream(t *testing.T) {
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n\x01\x00\r\n")
		_ = rw.Flush() // Deliberately omit the terminal zero-sized HTTP chunk.
	})
	gateway := httptest.NewServer(newHandler(b, 2))
	defer gateway.Close()
	resp := post(t, gateway.URL+"/v1/audio/speech", `{"input":"Hello","response_format":"pcm"}`, "", "")
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("truncated upstream was reported as success")
	}
	resp = post(t, gateway.URL+"/v1/rime-tts", `{"text":"Hello"}`, "text/event-stream", "")
	data, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Contains(data, []byte("event: error")) || bytes.Contains(data, []byte("event: done")) {
		t.Fatalf("bad SSE failure %s %v", data, err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			pcmHandler(w, r)
		case <-r.Context().Done():
		}
	})
	gateway := httptest.NewServer(newHandler(b, 1))
	defer gateway.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", gateway.URL+"/v1/audio/speech", strings.NewReader(`{"input":"Hello"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-entered
	resp := post(t, gateway.URL+"/v1/audio/speech", `{"input":"Second"}`, "", "")
	close(release)
	<-done
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatal(resp.StatusCode)
	}
}

func TestPlayerChild(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "cotorra-test-drain":
		n, err := io.Copy(io.Discard, os.Stdin)
		if err != nil || n == 0 {
			os.Exit(11)
		}
		os.Exit(0)
	case "cotorra-test-fail":
		fmt.Fprintln(os.Stderr, "intentional player failure")
		os.Exit(17)
	}
}
func TestPlayerProcessLifecycle(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cotorra-test-drain", "cotorra-test-fail"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		p := playerSpec{Name: "test-player", Path: executable, Args: []string{"-test.run=TestPlayerChild", "--", mode}, Streaming: true}
		err := playStream(ctx, p, io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{1, 0}, 1024))))
		cancel()
		if mode == "cotorra-test-drain" && err != nil {
			t.Fatal(err)
		}
		if mode == "cotorra-test-fail" && err == nil {
			t.Fatal("player failure ignored")
		}
	}
}

func TestSpeakFailureDoesNotPublish(t *testing.T) {
	b, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write([]byte{1})
	})
	out := filepath.Join(t.TempDir(), "out.wav")
	_, err := speak(context.Background(), b, "Hello", b.cfg.options(), nil, out, "wav", false, io.Discard)
	if err == nil {
		t.Fatal("odd audio succeeded")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial output was published")
	}
}

func TestExplicitEmptyTextDoesNotReadStdin(t *testing.T) {
	err := run(context.Background(), []string{"--text", "", "--no-play", "-o", "-"}, strings.NewReader("This must not be read"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty text error, got %v", err)
	}
}

func TestNoOverwriteThroughInputAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.txt")
	alias := path + ".wav"
	if err := os.WriteFile(path, []byte("Hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	err := run(context.Background(), []string{"--file", path, "--no-play", "-o", alias, "--force"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "paths must differ") {
		t.Fatalf("input alias not protected: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "Hello" {
		t.Fatal("input was changed")
	}
}
