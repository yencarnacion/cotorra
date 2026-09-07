package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This is intentionally a Rime-style subset, not an implementation of Rime's
// models, proprietary voices, cloning, timestamps or WebSocket protocol.
type apiRequest struct {
	Text            string   `json:"text"`
	Input           string   `json:"input"`
	Speaker         string   `json:"speaker"`
	Voice           string   `json:"voice"`
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Speed           *float64 `json:"speed"`
	SpeedAlpha      *float64 `json:"speedAlpha"`
	TimeScaleFactor *float64 `json:"timeScaleFactor"`
	Instructions    *string  `json:"instructions"`
	ResponseFormat  string   `json:"response_format"`
	AudioFormat     string   `json:"audioFormat"`
	SamplingRate    *int     `json:"samplingRate"`
	Lang            string   `json:"lang"`
	Stream          *bool    `json:"stream"`
}

func (a apiRequest) normalize(c config) (string, speechOptions, string, error) {
	opt := c.options()
	fail := func(message string) (string, speechOptions, string, error) { return "", opt, "", errors.New(message) }
	if a.Text != "" && a.Input != "" {
		return fail("use text or input, not both")
	}
	text := a.Text
	if a.Input != "" {
		text = a.Input
	}
	if a.Speaker != "" && a.Voice != "" {
		return fail("use speaker or voice, not both")
	}
	if a.Speaker != "" {
		opt.Voice = a.Speaker
	}
	if a.Voice != "" {
		opt.Voice = a.Voice
	}
	if a.ModelID != "" && a.Model != "" {
		return fail("use modelId or model, not both")
	}
	if a.ModelID != "" {
		opt.Model = a.ModelID
	}
	if a.Model != "" {
		opt.Model = a.Model
	}
	switch opt.Model {
	case "mist", "mistv2", "mistv3", "coda", "arcana":
		return fail("Rime models are unavailable; select a model from the configured Kokoro/OpenAI backend")
	}
	speeds := 0
	for _, value := range []*float64{a.Speed, a.SpeedAlpha, a.TimeScaleFactor} {
		if value != nil {
			speeds++
		}
	}
	if speeds > 1 {
		return fail("use only one of speed, speedAlpha or timeScaleFactor")
	}
	if a.Speed != nil {
		opt.Speed = *a.Speed
	}
	// Modern Rime direction: speedAlpha > 1 is faster. Legacy Mist v2's inverse
	// convention is NOT silently applied to backend model IDs.
	if a.SpeedAlpha != nil {
		opt.Speed = *a.SpeedAlpha
	}
	if a.TimeScaleFactor != nil {
		if *a.TimeScaleFactor <= 0 || math.IsNaN(*a.TimeScaleFactor) || math.IsInf(*a.TimeScaleFactor, 0) {
			return fail("timeScaleFactor must be finite and positive")
		}
		opt.Speed = 1 / *a.TimeScaleFactor
	}
	if a.Instructions != nil {
		opt.Instructions = *a.Instructions
	}
	if a.SamplingRate != nil && *a.SamplingRate != sampleRate {
		return fail("only samplingRate=24000 is supported; no resampling is performed")
	}
	if a.Lang != "" {
		if c.Provider != "kokoro" {
			return fail("OpenAI has no lang parameter: put text in the desired language and omit lang")
		}
		expected := "eng"
		if strings.HasPrefix(opt.Voice, "e") {
			expected = "spa"
		}
		lang := strings.ToLower(a.Lang)
		if lang == "en" {
			lang = "eng"
		}
		if lang == "es" {
			lang = "spa"
		}
		if lang != expected {
			return fail("lang must match the selected Kokoro voice (eng/en or spa/es)")
		}
	}
	if a.ResponseFormat != "" && a.AudioFormat != "" {
		return fail("use response_format or audioFormat, not both")
	}
	format := a.ResponseFormat
	if a.AudioFormat != "" {
		format = a.AudioFormat
	}
	if format != "" && format != "wav" && format != "pcm" {
		return fail("only wav and pcm are supported")
	}
	if err := validateText(text); err != nil {
		return "", opt, "", err
	}
	if err := c.validateOptions(opt.Model, opt.Voice, opt.Speed, opt.Instructions); err != nil {
		return "", opt, "", err
	}
	return text, opt, format, nil
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]any{"error": map[string]string{"message": message, "type": "cotorra_error"}})
}

// API errors never disclose an upstream credential or an upstream echoed prompt.
func streamError(w http.ResponseWriter, err error) {
	var upstream *upstreamError
	switch {
	case errors.As(err, &upstream):
		if upstream.Status == 429 {
			w.Header().Set("Retry-After", "1")
			apiError(w, 429, "backend is busy or rate limited; retry later")
			return
		}
		apiError(w, 502, fmt.Sprintf("backend rejected speech (HTTP %d); check configured credentials, voice and model", upstream.Status))
	case errors.Is(err, context.DeadlineExceeded):
		apiError(w, 504, "speech request timed out")
	case errors.Is(err, context.Canceled):
		apiError(w, 408, "request canceled")
	default:
		apiError(w, 502, "speech backend failed or returned invalid audio; check backend connectivity and logs")
	}
}

// negotiate supports quality-weighted Accept lists, including */*.
func negotiate(header string) (string, error) {
	if strings.TrimSpace(header) == "" {
		return "", nil
	}
	best, bestQ := "", -1.0
	for _, item := range strings.Split(header, ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			continue
		}
		q := 1.0
		if s, ok := params["q"]; ok {
			q, err = strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(q) || q <= 0 || q > 1 {
				continue
			}
		}
		switch strings.ToLower(media) {
		case "application/json", "text/event-stream", "audio/wav", "audio/x-wav", "audio/l16", "audio/pcm", "application/octet-stream", "*/*":
			if q > bestQ {
				best, bestQ = strings.ToLower(media), q
			}
		}
	}
	if bestQ < 0 {
		return "", errors.New("Accept must allow audio/wav, audio/pcm, audio/L16, application/octet-stream, application/json or text/event-stream")
	}
	if best == "*/*" {
		best = ""
	}
	return best, nil
}

func newHandler(b *backend, concurrency int) http.Handler {
	slots := make(chan struct{}, concurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/health" || r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				apiError(w, 405, "use GET")
				return
			}
			jsonResponse(w, 200, map[string]string{"status": "ok", "service": "cotorra", "version": version})
			return
		}
		if b.cfg.ServerKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.cfg.ServerKey)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			apiError(w, 401, "a valid Cotorra Bearer key is required")
			return
		}
		if r.Header.Get("Origin") != "" {
			apiError(w, 403, "browser-origin requests are disabled; use a trusted server-side client")
			return
		}
		// Limit all backend operations, not just synthesis, to avoid unbounded work.
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", "1")
			apiError(w, 429, "Cotorra is busy; retry later")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		r = r.WithContext(ctx)
		switch r.URL.Path {
		case "/readyz":
			if r.Method != http.MethodGet {
				apiError(w, 405, "use GET")
				return
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := b.readiness(ctx); err != nil {
				apiError(w, 503, "configured speech backend/model is not ready")
				return
			}
			jsonResponse(w, 200, map[string]string{"status": "ready", "provider": b.cfg.Provider, "model": b.cfg.Model})
		case "/voices", "/v1/voices", "/v1/audio/voices":
			if r.Method != http.MethodGet {
				apiError(w, 405, "use GET")
				return
			}
			v, err := b.voices(ctx)
			if err != nil {
				streamError(w, err)
				return
			}
			jsonResponse(w, 200, v)
		case "/v1/models":
			if r.Method != http.MethodGet {
				apiError(w, 405, "use GET")
				return
			}
			data, err := b.models(ctx)
			if err != nil {
				streamError(w, err)
				return
			}
			jsonResponse(w, 200, data)
		case "/v1/audio/speech", "/v1/rime-tts", "/rime-tts":
			if r.Method != http.MethodPost {
				apiError(w, 405, "use POST")
				return
			}
			handleSpeech(w, r, b)
		default:
			apiError(w, 404, "use /healthz, /readyz, /voices, /v1/models, /v1/audio/speech or /v1/rime-tts")
		}
	})
}

func handleSpeech(w http.ResponseWriter, r *http.Request, b *backend) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		apiError(w, 415, "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var request apiRequest
	if err := dec.Decode(&request); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apiError(w, 413, "request JSON exceeds 2 MiB")
			return
		}
		apiError(w, 400, "invalid JSON, wrong field type or unsupported request field")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		apiError(w, 400, "request must contain exactly one JSON object")
		return
	}
	text, opt, format, err := request.normalize(b.cfg)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	accept, err := negotiate(r.Header.Get("Accept"))
	if err != nil {
		apiError(w, 406, err.Error())
		return
	}
	required := ""
	switch accept {
	case "audio/pcm", "audio/l16", "application/octet-stream", "text/event-stream":
		required = "pcm"
	case "audio/wav", "audio/x-wav", "application/json":
		required = "wav"
	}
	if format != "" && required != "" && format != required {
		apiError(w, 400, "requested audio format conflicts with Accept")
		return
	}
	if format == "" {
		format = required
	}
	if format == "" {
		format = "wav"
	}
	if request.Stream != nil && *request.Stream && format != "pcm" {
		apiError(w, 400, "stream=true requires PCM; WAV and JSON WAV are buffered")
		return
	}
	if request.Stream != nil && !*request.Stream && accept == "text/event-stream" {
		apiError(w, 400, "stream=false conflicts with text/event-stream")
		return
	}
	source, err := b.stream(r.Context(), text, opt)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	defer source.Close()
	w.Header().Set("X-Audio-Sample-Rate", "24000")
	w.Header().Set("X-Audio-Channels", "1")
	w.Header().Set("X-Audio-Format", "pcm_s16le")
	if format == "wav" || (request.Stream != nil && !*request.Stream) {
		pcm, err := io.ReadAll(io.LimitReader(source, maxAudioBytes+1))
		if err != nil {
			streamError(w, err)
			return
		}
		if len(pcm) == 0 || int64(len(pcm)) > maxAudioBytes || len(pcm)%2 != 0 {
			apiError(w, 502, "backend returned invalid audio")
			return
		}
		data := pcm
		contentType := "application/octet-stream"
		if format == "wav" {
			data, err = asWAV(pcm)
			if err != nil {
				streamError(w, err)
				return
			}
			contentType = "audio/wav"
		}
		if accept == "application/json" {
			jsonResponse(w, 200, map[string]any{"audioContent": base64.StdEncoding.EncodeToString(data), "audioFormat": "wav", "samplingRate": sampleRate})
		} else {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(200)
			_, _ = w.Write(data)
		}
		return
	}
	// Delay status 200 until at least one complete sample is available.
	first := make([]byte, 2)
	if _, err := io.ReadFull(source, first); err != nil {
		streamError(w, err)
		return
	}
	sse := accept == "text/event-stream"
	if sse {
		w.Header().Set("Content-Type", "text/event-stream")
	} else if accept == "audio/l16" {
		// Rime uses audio/L16 for little-endian PCM, unlike RFC L16's byte order.
		w.Header().Set("Content-Type", "audio/L16;rate=24000;channels=1")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	controller := http.NewResponseController(w)
	emit := func(data []byte) error {
		if sse {
			_, err = fmt.Fprintf(w, "event: chunk\ndata: {\"data\":\"%s\"}\n\n", base64.StdEncoding.EncodeToString(data))
		} else {
			_, err = w.Write(data)
		}
		if err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := emit(first); err != nil {
		return
	}
	buffer := make([]byte, 8193)
	pending := 0
	for {
		n, readErr := source.Read(buffer[pending:])
		n += pending
		complete := n &^ 1
		if complete > 0 {
			if err := emit(buffer[:complete]); err != nil {
				return
			}
		}
		pending = n - complete
		if pending != 0 {
			buffer[0] = buffer[complete]
		}
		if readErr == nil {
			continue
		}
		// Read (not ReadFull) preserves an upstream io.ErrUnexpectedEOF. Treating
		// that as an ordinary last partial buffer would hide a truncated HTTP stream.
		if readErr != io.EOF || pending != 0 {
			if sse {
				_, _ = io.WriteString(w, "event: error\ndata: {\"error\":\"audio stream failed\"}\n\n")
				_ = controller.Flush()
				return
			}
			panic(http.ErrAbortHandler)
		}
		if sse {
			_, _ = io.WriteString(w, "event: done\ndata: {\"done\":true}\n\n")
			_ = controller.Flush()
		}
		return
	}

}

func serve(ctx context.Context, c config, b *backend, concurrency int, stderr io.Writer) error {
	if concurrency < 1 || concurrency > 16 {
		return errors.New("concurrency must be in 1..16")
	}
	if err := validateListen(c.Listen, c.ServerKey); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: newHandler(b, concurrency), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 16 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintf(stderr, "Cotorra API: http://%s (provider=%s, model=%s). Ctrl-C stops it.\n", listener.Addr(), c.Provider, c.Model)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
		<-done
		return nil
	}
}
