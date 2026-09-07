package main

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultBaseURL         = "http://10.17.17.99:9084/v1"
	defaultOpenAIURL       = "https://api.openai.com/v1"
	sampleRate             = 24000
	bytesPerSecond         = sampleRate * 2
	maxAudioBytes    int64 = 64 << 20
	maxTextBytes     int64 = 1 << 20
)

type config struct {
	Provider, BaseURL, APIKey, Model, Voice, Instructions string
	Speed                                                 float64
	Timeout                                               time.Duration
	ChunkChars                                            int
	Listen, ServerKey                                     string
	UseProxy                                              bool
}

// envFile parses a deliberately small dotenv dialect without executing shell code
// or interpolating variables. Existing process variables take precedence.
func envFile(path string, required bool) (map[string]string, error) {
	values := make(map[string]string)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return values, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open environment file: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		s := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimSpace(strings.TrimPrefix(s, "export "))
		key, value, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			return nil, fmt.Errorf("invalid dotenv assignment at line %d", line)
		}
		value, err = parseEnvValue(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid dotenv value at line %d (values are not logged)", line)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read environment file: %w", err)
	}
	return values, nil
}

func validEnvKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r != '_' && !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func parseEnvValue(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if s[0] != '\'' && s[0] != '"' {
		for i, r := range s {
			if r == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				s = s[:i]
				break
			}
		}
		return strings.TrimSpace(s), nil
	}
	quote := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if ch == quote {
			tail := strings.TrimSpace(s[i+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return "", errors.New("trailing data")
			}
			return b.String(), nil
		}
		if quote == '"' && ch == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"', '\\':
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		} else {
			b.WriteByte(ch)
		}
	}
	return "", errors.New("unterminated quote")
}

func envValue(file map[string]string, key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	if value, ok := file[key]; ok {
		return value
	}
	return fallback
}

func (c *config) resolve(env map[string]string) error {
	get := func(k, fallback string) string { return envValue(env, k, fallback) }
	c.Provider = strings.ToLower(c.Provider)
	if c.Provider == "local" {
		c.Provider = "kokoro"
	}
	switch c.Provider {
	case "kokoro":
		if c.BaseURL == "" {
			c.BaseURL = get("API_BASE_URL", defaultBaseURL)
		}
		c.APIKey = get("KOKORO_API_KEY", get("API_KEY", ""))
		if c.Model == "" {
			c.Model = get("KOKORO_MODEL", "kokoro")
		}
		if c.Voice == "" {
			c.Voice = get("KOKORO_VOICE", "af_heart")
		}
	case "openai":
		// API_BASE_URL and API_KEY belong to the local backend, never to OpenAI.
		if c.BaseURL == "" {
			c.BaseURL = get("OPENAI_BASE_URL", defaultOpenAIURL)
		}
		c.APIKey = get("OPENAI_API_KEY", "")
		if c.Model == "" {
			c.Model = get("OPENAI_TTS_MODEL", "gpt-4o-mini-tts")
		}
		if c.Voice == "" {
			c.Voice = get("OPENAI_TTS_VOICE", "coral")
		}
	default:
		return errors.New("provider must be kokoro (or local), or openai")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base URL must be an http(s) API base, without credentials, query or fragment")
	}
	if c.Provider == "openai" && u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return errors.New("OpenAI credentials require HTTPS (HTTP is permitted only for loopback testing)")
	}
	for _, key := range []string{c.APIKey, c.ServerKey} {
		if strings.ContainsAny(key, "\r\n") {
			return errors.New("API keys cannot contain line breaks")
		}
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if c.ChunkChars < 1 || c.ChunkChars > 4000 {
		return errors.New("chunk-chars must be in 1..4000")
	}
	return c.validateOptions(c.Model, c.Voice, c.Speed, c.Instructions)
}

func (c config) validateOptions(model, voice string, speed float64, instructions string) error {
	if strings.TrimSpace(model) == "" || strings.TrimSpace(voice) == "" {
		return errors.New("model and voice cannot be empty")
	}
	if c.Provider == "kokoro" && model != "kokoro" && model != "kokoro-82m" {
		return errors.New("Kokoro model must be kokoro or kokoro-82m; Rime models are not available")
	}
	lo, hi := 0.25, 4.0
	if c.Provider == "kokoro" {
		lo, hi = 0.5, 2.0
	}
	if math.IsNaN(speed) || math.IsInf(speed, 0) || speed < lo || speed > hi {
		return fmt.Errorf("speed must be in %.2f..%.2f for %s", lo, hi, c.Provider)
	}
	if instructions != "" && (c.Provider != "openai" || !strings.HasPrefix(model, "gpt-4o-mini-tts")) {
		return errors.New("instructions require the OpenAI gpt-4o-mini-tts model")
	}
	if len(instructions) > 16000 {
		return errors.New("instructions exceed 16000 bytes")
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateListen(address, key string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("listen must be host:port, for example 127.0.0.1:8099")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("invalid listen port")
	}
	if !isLoopback(host) && key == "" {
		return errors.New("non-loopback listening requires COTORRA_SERVER_API_KEY")
	}
	return nil
}

func validateText(text string) error {
	if int64(len(text)) > maxTextBytes {
		return errors.New("text exceeds the 1 MiB input limit")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("text cannot be empty")
	}
	speakable := false
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return errors.New("text contains unsupported control characters")
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			speakable = true
		}
	}
	if !speakable {
		return errors.New("text must contain words or numbers")
	}
	return nil
}
