package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type speechOptions struct {
	Model, Voice, Instructions string
	Speed                      float64
}

func (c config) options() speechOptions {
	return speechOptions{Model: c.Model, Voice: c.Voice, Instructions: c.Instructions, Speed: c.Speed}
}

type backend struct {
	cfg    config
	client *http.Client
}

type upstreamError struct {
	Status  int
	Message string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("backend HTTP %d: %s", e.Status, e.Message)
}

func newBackend(c config) *backend {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = c.Timeout
	transport.MaxIdleConnsPerHost = 4
	// Local TTS stays on the LAN even when a system HTTP proxy is configured.
	if c.Provider == "kokoro" && !c.UseProxy {
		transport.Proxy = nil
	}
	return &backend{cfg: c, client: &http.Client{
		Transport: transport, Timeout: c.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("backend redirects are disabled to protect API keys")
		},
	}}
}

func (b *backend) close() { b.client.CloseIdleConnections() }

func (b *backend) request(ctx context.Context, path string, payload any) (*http.Response, error) {
	if b.cfg.Provider == "openai" && b.cfg.APIKey == "" {
		return nil, errors.New("OPENAI_API_KEY is missing; put it in .env or the environment")
	}
	var body io.Reader
	method := http.MethodGet
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data) // Sends Content-Length, required by the supplied Kokoro server.
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cotorra/"+version)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("contact %s backend: %w", b.cfg.Provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		msg := strings.TrimSpace(string(data))
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
			msg = envelope.Error.Message
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if b.cfg.APIKey != "" {
			msg = strings.ReplaceAll(msg, b.cfg.APIKey, "[REDACTED]")
		}
		return nil, &upstreamError{Status: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

// splitText never splits a UTF-8 code point or silently truncates long words.
// Sentence/whitespace boundaries are preferred. Requests are sequential so Kokoro's
// one inference worker is not overloaded and audio order is preserved.
func splitText(text string, limit int) []string {
	runes := []rune(strings.TrimSpace(text))
	var chunks []string
	for len(runes) > 0 {
		n := len(runes)
		if n > limit {
			n = limit
			for i := limit; i > limit/2; i-- {
				if unicode.IsSpace(runes[i-1]) && i >= 2 && strings.ContainsRune(".!?。！？", runes[i-2]) {
					n = i
					break
				}
			}
			if n == limit {
				for i := limit; i > 0; i-- {
					if unicode.IsSpace(runes[i-1]) {
						n = i
						break
					}
				}
			}
		}
		part := strings.TrimSpace(string(runes[:n]))
		if part != "" {
			chunks = append(chunks, part)
		}
		runes = runes[n:]
	}
	return chunks
}

func (b *backend) stream(ctx context.Context, text string, options speechOptions) (io.ReadCloser, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("input is not valid UTF-8")
	}
	if err := validateText(text); err != nil {
		return nil, err
	}
	if err := b.cfg.validateOptions(options.Model, options.Voice, options.Speed, options.Instructions); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		defer cancel()
		var err error
		var total int64
		for _, chunk := range splitText(text, b.cfg.ChunkChars) {
			payload := map[string]any{"model": options.Model, "input": chunk, "voice": options.Voice, "speed": options.Speed, "response_format": "pcm"}
			if b.cfg.Provider == "kokoro" {
				payload["stream"] = true
			}
			if options.Instructions != "" {
				payload["instructions"] = options.Instructions
			}
			var resp *http.Response
			resp, err = b.request(ctx, "/audio/speech", payload)
			if err != nil {
				break
			}
			err = validatePCMHeaders(resp.Header)
			if err == nil {
				var n int64
				n, err = io.Copy(writer, io.LimitReader(resp.Body, maxAudioBytes-total+1))
				total += n
				if err == nil && total > maxAudioBytes {
					err = errors.New("audio exceeds 64 MiB limit")
				}
				if err == nil && n == 0 {
					err = errors.New("backend returned empty audio")
				}
				if err == nil && n%2 != 0 {
					err = errors.New("backend returned an incomplete PCM sample")
				}
			}
			resp.Body.Close()
			if err != nil {
				break
			}
		}
		writer.CloseWithError(err)
	}()
	return &cancelReader{ReadCloser: reader, cancel: cancel}, nil
}

type cancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelReader) Close() error { r.cancel(); return r.ReadCloser.Close() }

func validatePCMHeaders(h http.Header) error {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(h.Get("Content-Type"), ";")[0]))
	switch ct {
	case "application/octet-stream", "audio/pcm", "audio/l16", "audio/raw":
	default:
		return fmt.Errorf("backend returned %q instead of raw PCM", ct)
	}
	for name, value := range map[string]string{"X-Audio-Sample-Rate": "24000", "X-Audio-Channels": "1", "X-Audio-Format": "pcm_s16le"} {
		if actual := h.Get(name); actual != "" && actual != value {
			return fmt.Errorf("unsupported %s: expected %s", name, value)
		}
	}
	return nil
}

type voiceList struct {
	Voices  []string `json:"voices"`
	Default string   `json:"default"`
	Source  string   `json:"source,omitempty"`
}

func (b *backend) voices(ctx context.Context) (voiceList, error) {
	if b.cfg.Provider == "openai" {
		// The speech API has no public built-in-voice listing endpoint.
		voices := []string{"alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer", "verse", "marin", "cedar"}
		if b.cfg.Model == "tts-1" || b.cfg.Model == "tts-1-hd" {
			voices = []string{"alloy", "ash", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer"}
		}
		return voiceList{Voices: voices, Default: b.cfg.Voice, Source: "OpenAI documented built-in voices (2026-09-07); model/project availability may differ"}, nil
	}
	resp, err := b.request(ctx, "/audio/voices", nil)
	if err != nil {
		return voiceList{}, err
	}
	defer resp.Body.Close()
	var result voiceList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return result, fmt.Errorf("decode voices: %w", err)
	}
	result.Source = "live Kokoro backend"
	return result, nil
}

func (b *backend) models(ctx context.Context) (json.RawMessage, error) {
	resp, err := b.request(ctx, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 || !json.Valid(data) {
		return nil, errors.New("invalid or oversized models response")
	}
	return json.RawMessage(data), nil
}

func (b *backend) readiness(ctx context.Context) error {
	// Check the configured model, not merely HTTP reachability.
	data, err := b.models(ctx)
	if err != nil {
		return err
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	for _, m := range result.Data {
		if m.ID == b.cfg.Model || (b.cfg.Provider == "kokoro" && m.ID == "kokoro" && b.cfg.Model == "kokoro-82m") {
			return nil
		}
	}
	return errors.New("backend is reachable but configured model is absent from /models")
}

func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}
