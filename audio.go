package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type playerSpec struct {
	Name, Path string
	Args       []string
	Streaming  bool
}

func playerFor(goos, requested, mode string, lookup func(string) (string, error)) (playerSpec, error) {
	if mode == "true" {
		mode = "on"
	}
	if mode == "false" {
		mode = "off"
	}
	if mode != "auto" && mode != "on" && mode != "off" {
		return playerSpec{}, errors.New("stream must be auto, on or off")
	}
	raw := []playerSpec{
		{Name: "paplay", Args: []string{"--raw", "--format=s16le", "--rate=24000", "--channels=1"}, Streaming: true},
		{Name: "aplay", Args: []string{"-q", "-t", "raw", "-f", "S16_LE", "-r", "24000", "-c", "1"}, Streaming: true},
		{Name: "ffplay", Args: []string{"-nodisp", "-autoexit", "-loglevel", "error", "-f", "s16le", "-sample_rate", "24000", "-ch_layout", "mono", "-i", "pipe:0"}, Streaming: true},
	}
	file := []playerSpec{}
	switch goos {
	case "linux":
		file = []playerSpec{{Name: "paplay"}, {Name: "aplay", Args: []string{"-q"}}, {Name: "ffplay", Args: []string{"-nodisp", "-autoexit", "-loglevel", "error", "-i"}}}
	case "darwin":
		raw = raw[2:]
		file = []playerSpec{{Name: "afplay"}, {Name: "ffplay", Args: []string{"-nodisp", "-autoexit", "-loglevel", "error", "-i"}}}
	case "windows":
		raw = raw[2:]
		file = []playerSpec{{Name: "powershell"}, {Name: "pwsh"}, {Name: "ffplay", Args: []string{"-nodisp", "-autoexit", "-loglevel", "error", "-i"}}}
	default:
		return playerSpec{}, fmt.Errorf("unsupported playback OS %s; use --no-play --output speech.wav", goos)
	}
	var candidates []playerSpec
	if mode != "off" {
		candidates = append(candidates, raw...)
	}
	if mode != "on" {
		candidates = append(candidates, file...)
	}
	for _, p := range candidates {
		if requested != "auto" && requested != p.Name {
			continue
		}
		if path, err := lookup(p.Name); err == nil {
			p.Path = path
			return p, nil
		}
	}
	return playerSpec{}, fmt.Errorf("no %s audio player found (player=%s); install FFmpeg/ffplay, or use --stream=off for native WAV playback; on Linux install pulseaudio-utils or alsa-utils; saving alone works with --no-play --output speech.wav", mode, requested)
}

// limitedLog bounds error output from external programs.
type limitedLog struct{ bytes.Buffer }

func (l *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	if remain := 8192 - l.Len(); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = l.Buffer.Write(p)
	}
	return n, nil
}

func playerEnv() []string {
	var result []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		switch strings.ToUpper(key) {
		case "OPENAI_API_KEY", "KOKORO_API_KEY", "API_KEY", "COTORRA_SERVER_API_KEY", "COTORRA_WAV_PATH":
			continue
		}
		result = append(result, v)
	}
	return result
}

func playStream(ctx context.Context, p playerSpec, source io.ReadCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Path, p.Args...)
	cmd.Env = playerEnv()
	var log limitedLog
	cmd.Stderr = &log
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("start %s: %w", p.Name, err)
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		// An exited/broken player must cancel a blocked network read as well.
		source.Close()
		done <- err
	}()
	_, copyErr := io.CopyBuffer(stdin, source, make([]byte, 8192))
	stdin.Close()
	if copyErr != nil {
		cancel()
	}
	waitErr := <-done
	if copyErr != nil && !errors.Is(copyErr, io.ErrClosedPipe) {
		return fmt.Errorf("stream playback: %w", copyErr)
	}
	if waitErr != nil {
		return fmt.Errorf("%s failed: %w: %s", p.Name, waitErr, strings.TrimSpace(log.String()))
	}
	return copyErr
}

func playWAV(ctx context.Context, p playerSpec, path string) error {
	args := append([]string(nil), p.Args...)
	env := playerEnv()
	if p.Name == "powershell" || p.Name == "pwsh" {
		// Fixed code + an environment variable, not an interpolated shell command.
		args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
			"$ErrorActionPreference='Stop'; $p=New-Object System.Media.SoundPlayer; try { $p.SoundLocation=$env:COTORRA_WAV_PATH; $p.Load(); $p.PlaySync() } finally { $p.Dispose() }"}
		env = append(env, "COTORRA_WAV_PATH="+path)
	} else {
		args = append(args, path)
	}
	cmd := exec.CommandContext(ctx, p.Path, args...)
	cmd.Env = env
	var log limitedLog
	cmd.Stderr = &log
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s failed: %w: %s", p.Name, err, strings.TrimSpace(log.String()))
	}
	return nil
}

func wavHeader(n int64) ([]byte, error) {
	if n < 0 || n > maxAudioBytes || n%2 != 0 {
		return nil, errors.New("invalid PCM size for WAV")
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(n+36))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], sampleRate)
	binary.LittleEndian.PutUint32(h[28:], bytesPerSecond)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(n))
	return h, nil
}

func asWAV(pcm []byte) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, errors.New("empty audio")
	}
	h, err := wavHeader(int64(len(pcm)))
	if err != nil {
		return nil, err
	}
	return append(h, pcm...), nil
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

// speak spools PCM into a private temporary WAV while optionally playing it live.
// Header sizes are finalized only on success, including for multi-request input.
func speak(ctx context.Context, b *backend, text string, opt speechOptions, p *playerSpec, output, format string, force bool, stdout io.Writer) (int64, error) {
	if output != "" && output != "-" && !force {
		if _, err := os.Lstat(output); err == nil {
			return 0, errors.New("output exists; use --force to replace it")
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	f, err := os.CreateTemp("", "cotorra-*.wav")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h, _ := wavHeader(0)
	if _, err := f.Write(h); err != nil {
		return 0, err
	}
	source, err := b.stream(ctx, text, opt)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	reader := &teeReadCloser{Reader: io.TeeReader(source, f), Closer: source}
	if p != nil && p.Streaming {
		err = playStream(ctx, *p, reader)
	} else {
		_, err = io.Copy(io.Discard, reader)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, err
	}
	position, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	n := position - 44
	if n <= 0 {
		return 0, errors.New("no audio received")
	}
	h, err = wavHeader(n)
	if err != nil {
		return 0, err
	}
	if _, err := f.WriteAt(h, 0); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	// Close the file before a Windows player opens it.
	if err := f.Close(); err != nil {
		return 0, err
	}
	if output != "" {
		if err := publishAudio(f.Name(), output, format, force, stdout); err != nil {
			return 0, err
		}
	}
	if p != nil && !p.Streaming {
		if err := playWAV(ctx, *p, f.Name()); err != nil {
			return n, err
		}
	}
	return n, nil
}

func publishAudio(temp, output, format string, force bool, stdout io.Writer) error {
	input, err := os.Open(temp)
	if err != nil {
		return err
	}
	defer input.Close()
	if format == "pcm" {
		if _, err := input.Seek(44, io.SeekStart); err != nil {
			return err
		}
	}
	if output == "-" {
		_, err := io.Copy(stdout, input)
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	out, err := os.OpenFile(output, flags, 0600)
	if err != nil {
		return fmt.Errorf("save audio: %w", err)
	}
	_, copyErr := io.Copy(out, input)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(output)
		return fmt.Errorf("save audio: %w", errors.Join(copyErr, closeErr))
	}
	return nil
}
