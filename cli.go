package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const usage = `Cotorra - speak text with local Kokoro or OpenAI; optionally serve an HTTP API.

Usage:
  cotorra [speak] [flags] "Text to speak"
  cotorra [speak] --file notes.txt [flags]
  echo "Hello" | cotorra
  cotorra voices|models|doctor [flags]
  cotorra serve [flags]
  cotorra version

Examples:
  cotorra "The build finished successfully."
  cotorra --voice ef_dora "Hola. El servicio está listo."
  cotorra --provider openai --voice coral "Hello from OpenAI."
  cotorra --no-play --output speech.wav "Save without playing."
  cotorra --stream=on --output speech.wav "Play while also saving."
  cotorra --no-play --format wav --output - "Binary stdout" > speech.wav
  cotorra serve --listen 127.0.0.1:8099

Defaults: Kokoro at http://10.17.17.99:9084/v1, model kokoro, voice af_heart.
OpenAI: explicit --provider openai; reads OPENAI_API_KEY from .env/environment.
Playback is on THIS computer. No cloud fallback or automatic software installs.
--stream=auto uses live PCM when possible, otherwise a native WAV player.
Flags may appear before or after text. Use -- before text beginning with a dash.

Flags:
`

// preliminaryEnv finds --env-file without interpreting a text argument as a flag
// value. Stop at -- so literal speech cannot cause an unrelated file to be loaded.
func preliminaryEnv(args []string) (string, bool, error) {
	takesValue := map[string]bool{"provider": true, "base-url": true, "voice": true, "speaker": true, "model": true, "instructions": true, "speed": true, "timeout": true, "chunk-chars": true, "text": true, "file": true, "output": true, "o": true, "format": true, "player": true, "stream": true, "listen": true, "concurrency": true, "env-file": true}
	path, required := ".env", false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		key, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if key == "env-file" {
			if !hasValue {
				i++
				if i >= len(args) {
					return "", false, errors.New("env-file needs a path")
				}
				value = args[i]
			}
			path, required = value, true
		} else if takesValue[key] && !hasValue {
			i++
		}
	}
	return path, required, nil
}

// The standard flag package stops at positional arguments. Reorder recognized
// flags while preserving positional order and the literal -- delimiter.
func interspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			pos = append(pos, arg)
			continue
		}
		key, _, equals := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(key)
		if f == nil {
			return nil, fmt.Errorf("unknown flag %s", arg)
		}
		flags = append(flags, arg)
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !equals && !(ok && boolean.IsBoolFlag()) {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("flag %s needs a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	return append(append(flags, "--"), pos...), nil
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	// Help/version remain available even with a broken .env file.
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, "cotorra", version, runtime.GOOS+"/"+runtime.GOARCH)
		return nil
	}
	helpOnly := len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")
	envPath, required, err := preliminaryEnv(args)
	if err != nil {
		return err
	}
	env := map[string]string{}
	if !helpOnly {
		env, err = envFile(envPath, required)
		if err != nil {
			return err
		}
	}
	get := func(key, fallback string) string { return envValue(env, key, fallback) }
	var c config
	var text, file, output, format, player, mode string
	var noPlay, force, quiet, readStdin, help bool
	var concurrency int
	fs := flag.NewFlagSet("cotorra", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&envPath, "env-file", envPath, "dotenv file (default .env in current directory)")
	fs.StringVar(&c.Provider, "provider", get("COTORRA_PROVIDER", "kokoro"), "kokoro (local) or openai")
	fs.StringVar(&c.BaseURL, "base-url", "", "override selected provider's API base URL (including /v1)")
	fs.StringVar(&c.Model, "model", "", "backend model; defaults are provider-specific")
	fs.StringVar(&c.Voice, "voice", "", "backend voice; af_heart for Kokoro, coral for OpenAI")
	fs.StringVar(&c.Voice, "speaker", "", "alias for --voice")
	fs.StringVar(&c.Instructions, "instructions", get("COTORRA_INSTRUCTIONS", ""), "delivery instructions (OpenAI gpt-4o-mini-tts only)")
	fs.Float64Var(&c.Speed, "speed", 1, "speed multiplier: >1 faster; Kokoro 0.5..2, OpenAI 0.25..4")
	fs.DurationVar(&c.Timeout, "timeout", 180*time.Second, "timeout for each backend HTTP request")
	fs.IntVar(&c.ChunkChars, "chunk-chars", 2000, "maximum Unicode characters per sequential backend request (1..4000)")
	fs.BoolVar(&c.UseProxy, "use-proxy", false, "allow environment HTTP proxy for the local backend")
	fs.StringVar(&text, "text", "", "text to speak (alternative to positional text)")
	fs.StringVar(&file, "file", "", "UTF-8 text file; - reads stdin")
	fs.BoolVar(&readStdin, "stdin", false, "read UTF-8 text from stdin")
	fs.StringVar(&output, "output", "", "save WAV/PCM; - writes binary stdout")
	fs.StringVar(&output, "o", "", "alias for --output")
	fs.StringVar(&format, "format", "", "wav or pcm; inferred from output extension, otherwise wav")
	fs.BoolVar(&noPlay, "no-play", false, "save audio without speaker playback; requires output")
	fs.BoolVar(&force, "force", false, "replace an existing output file after successful synthesis")
	fs.BoolVar(&quiet, "quiet", false, "suppress progress messages")
	fs.StringVar(&player, "player", get("COTORRA_PLAYER", "auto"), "auto, paplay, aplay, afplay, powershell, pwsh or ffplay")
	fs.StringVar(&mode, "stream", get("COTORRA_STREAM", "auto"), "auto, on or off; on requires a raw PCM player")
	fs.StringVar(&c.Listen, "listen", get("COTORRA_LISTEN", "127.0.0.1:8099"), "HTTP API listen address (serve only)")
	fs.IntVar(&concurrency, "concurrency", 2, "maximum simultaneous API backend operations (1..16)")
	fs.BoolVar(&help, "help", false, "show help")
	fs.BoolVar(&help, "h", false, "show help")
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	reordered, err := interspersed(fs, args)
	if err != nil {
		return err
	}
	if err := fs.Parse(reordered); err != nil {
		return err
	}
	textSet, fileSet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "text" {
			textSet = true
		}
		if f.Name == "file" {
			fileSet = true
		}
	})
	if fileSet && file == "" {
		return errors.New("--file requires a nonempty path")
	}
	pos := fs.Args()
	command := "speak"
	if len(pos) > 0 {
		switch pos[0] {
		case "speak", "serve", "voices", "models", "doctor", "help", "version":
			command = pos[0]
			pos = pos[1:]
		}
	}
	if help || command == "help" {
		fs.Usage()
		return nil
	}
	if command == "version" {
		fmt.Fprintln(stdout, "cotorra", version)
		return nil
	}
	c.ServerKey = get("COTORRA_SERVER_API_KEY", "")
	if err := c.resolve(env); err != nil {
		return err
	}
	b := newBackend(c)
	defer b.close()
	if command != "speak" && (len(pos) > 0 || textSet || fileSet || readStdin || output != "" || noPlay) {
		return fmt.Errorf("%s does not accept speech text or output flags", command)
	}
	switch command {
	case "serve":
		if c.Provider == "openai" && c.APIKey == "" {
			return errors.New("OPENAI_API_KEY is missing")
		}
		return serve(ctx, c, b, concurrency, stderr)
	case "voices":
		voices, err := b.voices(ctx)
		if err != nil {
			return err
		}
		return writeJSON(stdout, voices)
	case "models":
		models, err := b.models(ctx)
		if err != nil {
			return err
		}
		return writeJSON(stdout, models)
	case "doctor":
		fmt.Fprintf(stdout, "Cotorra %s (%s/%s)\nProvider: %s\nBackend: %s\nModel: %s\nVoice: %s\n", version, runtime.GOOS, runtime.GOARCH, c.Provider, safeURL(c.BaseURL), c.Model, c.Voice)
		fmt.Fprintf(stdout, "Backend key configured: %t (value never displayed)\n", c.APIKey != "")
		p, playErr := playerFor(runtime.GOOS, player, mode, exec.LookPath)
		if playErr != nil {
			fmt.Fprintln(stdout, "Playback:", playErr)
		} else {
			fmt.Fprintf(stdout, "Player: %s (streaming=%t)\n", p.Name, p.Streaming)
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := b.readiness(checkCtx); err != nil {
			return fmt.Errorf("backend check: %w", err)
		}
		fmt.Fprintln(stdout, "Backend model check: OK. This does not synthesize or test physical speakers.")
		return playErr
	}
	if format == "" {
		switch strings.ToLower(filepath.Ext(output)) {
		case "", ".wav":
			format = "wav"
		case ".pcm":
			format = "pcm"
		default:
			return errors.New("output extension must be .wav or .pcm (or explicitly set --format)")
		}
	}
	if format != "wav" && format != "pcm" {
		return errors.New("format must be wav or pcm; MP3 is not implemented")
	}
	if noPlay && output == "" {
		return errors.New("--no-play requires --output")
	}
	if output == "-" {
		noPlay = true
	} // Binary stdout is a pipeline, never implicit playback.
	sources := 0
	if len(pos) > 0 {
		sources++
	}
	if textSet {
		sources++
	}
	if fileSet {
		sources++
	}
	if readStdin {
		sources++
	}
	if sources > 1 {
		return errors.New("use exactly one of positional text, --text, --file or --stdin")
	}
	if len(pos) > 0 {
		if len(pos) == 1 && pos[0] == "-" {
			readStdin = true
		} else {
			text = strings.Join(pos, " ")
		}
	}
	if file == "-" {
		readStdin = true
		file = ""
	}
	if file != "" {
		if output != "" && output != "-" {
			inAbs, _ := filepath.Abs(file)
			outAbs, _ := filepath.Abs(output)
			sameFile := inAbs == outAbs
			inInfo, inErr := os.Stat(file)
			outInfo, outErr := os.Stat(output)
			if inErr == nil && outErr == nil && os.SameFile(inInfo, outInfo) {
				sameFile = true
			}
			if sameFile {
				return errors.New("input and output paths must differ")
			}
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		text, err = readText(f)
		f.Close()
		if err != nil {
			return err
		}
	} else if readStdin || sources == 0 {
		if f, ok := stdin.(*os.File); ok && !readStdin {
			info, err := f.Stat()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeCharDevice != 0 {
				fs.Usage()
				return errors.New("provide text, --file, or pipe text through stdin")
			}
		}
		text, err = readText(stdin)
		if err != nil {
			return err
		}
	}
	text = strings.TrimPrefix(text, "\ufeff")
	if err := validateText(text); err != nil {
		return err
	}
	var selected *playerSpec
	if !noPlay {
		p, err := playerFor(runtime.GOOS, player, mode, exec.LookPath)
		if err != nil {
			return err
		}
		selected = &p
	}
	if !quiet {
		playback := "save only"
		if selected != nil {
			playback = selected.Name
			if selected.Streaming {
				playback += " (streaming PCM)"
			} else {
				playback += " (buffered WAV)"
			}
		}
		fmt.Fprintf(stderr, "Cotorra: %s / %s / %s; %s\n", c.Provider, c.Model, c.Voice, playback)
	}
	started := time.Now()
	n, err := speak(ctx, b, text, c.options(), selected, output, format, force, stdout)
	if err != nil {
		return err
	}
	if !quiet {
		fmt.Fprintf(stderr, "Done: %.2f seconds of audio; elapsed %.2fs.\n", float64(n)/bytesPerSecond, time.Since(started).Seconds())
	}
	return nil
}

func readText(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxTextBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > maxTextBytes {
		return "", errors.New("text exceeds the 1 MiB input limit")
	}
	return string(data), nil
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
