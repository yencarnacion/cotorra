# Cotorra

A Go text-to-speech CLI with **local speaker playback** and an optional **Rime-style / OpenAI-style HTTP gateway**. It uses your existing Kokoro server by default, or OpenAI when explicitly selected. No Go dependencies, Python runtime, CGo, local model download, or Rime account is required on the client.

```bash
./go.sh "The build finished successfully."
```

Default backend: `http://10.17.17.99:9084/v1`, model `kokoro`, voice `af_heart`. Audio plays on **the computer running Cotorra**, not on the DGX Spark. Running Cotorra over SSH therefore plays on the remote machine unless that machine's audio is explicitly forwarded.

## Build and run

Use a currently supported Go release. The source requires Go 1.23 or newer; no third-party Go packages are needed.

### Linux and macOS

```bash
git clone https://github.com/yencarnacion/cotorra.git
cd cotorra
./go.sh "Hello from Cotorra."

# Or build once and run directly:
go build -o cotorra .
./cotorra "Hello from Kokoro."
./cotorra doctor
```

On Ubuntu/Debian, install a playback utility if none is already available:

```bash
sudo apt-get update && sudo apt-get install -y pulseaudio-utils
```

`alsa-utils` (aplay) or `ffmpeg` (ffplay) are alternatives. The machine must have a working audio device/session; installing a utility alone does not create one. On macOS, buffered WAV playback uses the built-in `afplay`. Installing FFmpeg enables streaming playback there.

### Windows PowerShell

```powershell
git clone https://github.com/yencarnacion/cotorra.git
cd cotorra
go build -o cotorra.exe .
.\cotorra.exe "Hello from Kokoro."
.\cotorra.exe doctor
```

`go.ps1` is also included as a build-and-run helper where PowerShell script policy permits it. Native Windows playback uses PowerShell's `System.Media.SoundPlayer` after synthesis completes. Install FFmpeg and put `ffplay.exe` on `PATH` for streaming playback. No policy change is required to run the compiled executable.

### Player behavior

| System | Streaming PCM, in preference order | Buffered WAV fallback |
| --- | --- | --- |
| Linux | paplay, aplay, ffplay | paplay, aplay, ffplay |
| macOS | ffplay | afplay, ffplay |
| Windows | ffplay | Windows PowerShell, pwsh, ffplay |

`--stream=auto` prefers streaming when a compatible executable is found. `--stream=on` requires it; `--stream=off` forces buffered WAV. `--player=ffplay` or another listed name selects explicitly. Detection checks executable availability, not whether its audio device works. A selected player failure is reported rather than replaying the text through another player or switching providers. Cotorra never installs software automatically.

## Configuration

```bash
cp .env.example .env
```

Windows: `Copy-Item .env.example .env`. Edit `.env` without committing it. Settings are resolved from CLI flags, then process environment, then the selected dotenv file, then defaults. The default file is `.env` in the **current working directory**, not the executable's directory. `--env-file /path/to/.env` selects another file; an explicitly missing file is an error. Quoted values, comments and `export KEY=value` are supported; shell commands and variable expansion are never executed.

A local backend that requires authentication needs:

```dotenv
API_BASE_URL=http://10.17.17.99:9084/v1
KOKORO_API_KEY=the-same-secret-used-by-your-kokoro-launcher
```

`KOKORO_API_KEY` takes precedence over the launcher's generic `API_KEY` compatibility alias. Omit the key when the backend does not require one. Setting `KOKORO_API_KEY=` explicitly disables the alias fallback.

For OpenAI, add:

```dotenv
OPENAI_API_KEY=your-openai-api-key
```

Then select OpenAI explicitly:

```bash
./cotorra --provider openai "Hello from OpenAI."
./cotorra --provider openai --voice coral --instructions "Speak clearly and warmly." "Your report is ready."
```

OpenAI defaults: model `gpt-4o-mini-tts`, voice `coral`, base `https://api.openai.com/v1`. Override with `OPENAI_TTS_MODEL`, `OPENAI_TTS_VOICE`, `OPENAI_BASE_URL` or corresponding CLI flags. An OpenAI key by itself **does not change the default provider**. There is no automatic cloud fallback. OpenAI requests may incur charges. The local backend never reads `OPENAI_API_KEY`; OpenAI never reads the local `API_BASE_URL` or `API_KEY`.

Other environment settings: `COTORRA_PROVIDER`, `KOKORO_MODEL`, `KOKORO_VOICE`, `COTORRA_PLAYER`, `COTORRA_STREAM`, `COTORRA_INSTRUCTIONS`, `COTORRA_LISTEN`, and `COTORRA_SERVER_API_KEY`. Speed, timeout and chunk size are CLI flags. `--base-url` must include the API prefix, normally `/v1`; it is not a full `/audio/speech` URL.

## Examples

```bash
# Speak directly; flags can be before or after the text.
./cotorra "The build is complete."
./cotorra speak --text "The build is complete."
./cotorra "El servicio está listo." --voice ef_dora
./cotorra --speed 1.2 "Speak slightly faster."

# Read a file or stdin.
./cotorra --file notes.txt
echo "The report is ready." | ./cotorra
./cotorra --stdin < notes.txt

# Play while saving, or save without playing.
./cotorra --output speech.wav "Hello."
./cotorra --no-play --output speech.wav "Hello."
./cotorra --no-play --output speech.pcm "Raw PCM."
./cotorra --no-play --output - "Binary WAV on stdout." > speech.wav

# Explicit backend / player / latency behavior.
./cotorra --base-url http://127.0.0.1:9084/v1 "Hello."
./cotorra --stream=on --player=ffplay "Start playback while generating."
./cotorra --stream=off "Generate the complete WAV before playing."
./cotorra --file notes.txt --chunk-chars 1000 --timeout 180s

# Inspection.
./cotorra voices
./cotorra --provider openai voices
./cotorra models
./cotorra doctor
./cotorra --help
```

Saved files are WAV or headerless PCM: 24,000 Hz, mono, signed 16-bit little-endian. Audio is spooled into a private temporary WAV and its header is finalized on success. Long input is split sequentially at sentence/whitespace boundaries (default 2,000 Unicode characters per backend request), preserving spoken-word order and producing one valid output WAV. Prosody can change at request boundaries. No automatic request retries are performed.

`--no-play` requires an output destination. `--output -` automatically disables playback and writes **only audio** to stdout; status goes to stderr. Binary stdout is buffered until synthesis succeeds; use HTTP PCM or a streaming player for live output. Avoid legacy Windows PowerShell's text-oriented binary redirection; use `--output file.wav` on Windows. Existing output files require `--force`; synthesis failure does not replace them. An I/O failure while publishing can leave no output file, so retain backups when using `--force`.

Limits: 1 MiB UTF-8 text, 64 MiB PCM output, 1–4,000 characters per backend request, 180-second per-request timeout by default. OpenAI token limits and a launcher's reduced `MAX_INPUT_CHARS` can require a smaller `--chunk-chars`. Cotorra does not estimate tokens or silently truncate input. Ctrl-C cancels synthesis/playback; server mode also handles SIGTERM.

## Your supplied Kokoro launcher

Cotorra targets the launcher's strict subset: `POST /v1/audio/speech` with `model`, `input`, `voice`, `speed`, `response_format=pcm`, and `stream=true`. It does not send unsupported Rime fields or OpenAI-only instructions to Kokoro. Voice discovery calls `/v1/audio/voices`; model discovery calls `/v1/models`.

The supplied script's source defaults to host port **8006** and loopback binding, with the container also listening on 8006. Your stated deployment is **9084**, so Cotorra intentionally defaults to 9084. No deployment or Docker changes are made. For another deployment, set `API_BASE_URL`; a separate computer must be able to reach the published host address and have the matching backend key when enabled. Keep unencrypted LAN HTTP on a trusted network or use a TLS/SSH tunnel.

The launcher's default voices are `af_heart`, `af_bella`, `am_michael`, `bf_emma`, and `ef_dora`. Its live voice list is authoritative. It streams synthesized phrases rather than individual neural frames; Cotorra cannot remove the model's first-phrase latency.

## Optional HTTP API

```bash
./cotorra serve
# Default: http://127.0.0.1:8099; Ctrl-C stops it.
```

The provider is selected when starting the server, not by untrusted request fields. Use `./cotorra serve --provider openai` for an OpenAI-backed gateway. API calls return audio to the caller; **they do not play it on the server's speakers**. Use the CLI for playback.

### OpenAI-style request

```bash
curl --fail-with-body http://127.0.0.1:8099/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"model":"kokoro","input":"Hello from Cotorra.","voice":"af_heart","response_format":"wav"}' \
  --output speech.wav
```

### Rime-style request

```bash
curl --fail-with-body http://127.0.0.1:8099/v1/rime-tts \
  -H 'Content-Type: application/json' -H 'Accept: audio/wav' \
  -d '{"text":"Hello from Cotorra.","speaker":"af_heart","modelId":"kokoro","samplingRate":24000,"speedAlpha":1.0}' \
  --output speech.wav
```

Change `Accept` to `application/json` for `{ "audioContent": "<base64 WAV>", "audioFormat": "wav", "samplingRate": 24000 }`. For live raw audio, use `Accept: audio/pcm` and `audioFormat: pcm`. For SSE use `Accept: text/event-stream` and `audioFormat: pcm`; each `event: chunk` contains `{ "data": "<base64 PCM>" }`, followed by `event: done` with `{ "done": true }`. A failed SSE stream ends with `event: error`, never a success event. After a raw stream starts, errors abort the HTTP stream rather than appending JSON to audio.

| Route | Purpose |
| --- | --- |
| `GET /health`, `/healthz` | Public Cotorra liveness, not backend readiness |
| `GET /readyz` | Check the configured model in the backend's `/models`; not a synthesis test |
| `GET /voices`, `/v1/voices`, `/v1/audio/voices` | Kokoro live list or explicitly labeled OpenAI documented built-in catalog |
| `GET /v1/models` | Backend's model list |
| `POST /v1/audio/speech` | OpenAI-style speech fields |
| `POST /v1/rime-tts`, `/rime-tts` | Rime-style field aliases and response negotiation |

Default response is WAV. `response_format`/`audioFormat` support `wav` and `pcm`; `stream=true` requires PCM. `stream=false` buffers PCM. `Accept` also supports `audio/L16` and `application/octet-stream` for raw PCM. **The Rime-style `audio/L16` alias contains little-endian PCM, not RFC L16 big-endian audio**; prefer `audio/pcm` to avoid ambiguity. Cotorra's JSON envelope is its documented contract, not a promise of complete Rime SDK compatibility.

### Scope of compatibility

This is a **Rime-inspired subset, not a drop-in replacement for the full Rime service**. Use backend voice IDs and model IDs, not Rime's proprietary voices/models. Supported aliases are `text`/`input`, `speaker`/`voice`, `modelId`/`model`, and `audioFormat`/`response_format`; conflicting aliases and unknown fields are errors. Only 24 kHz output is supported; no resampling, MP3, cloning, word timestamps, SSML, pronunciation dictionaries, advanced normalization, WebSockets, or Rime model emulation is implemented.

`speed` and `speedAlpha` greater than 1 mean faster; `timeScaleFactor` greater than 1 means slower. This follows the modern direction rather than legacy Mist v2's inverse `speedAlpha`. Kokoro accepts 0.5–2; OpenAI accepts 0.25–4. `lang=eng/en` or `spa/es` is accepted only with a matching Kokoro voice; OpenAI uses the input text's language and requires omitting `lang`.

### Server security and concurrency

Set `COTORRA_SERVER_API_KEY` to protect all routes except liveness, including on loopback. Clients send `Authorization: Bearer <Cotorra key>`. Binding outside loopback requires this key:

```dotenv
COTORRA_SERVER_API_KEY=choose-a-long-random-secret-different-from-backend-keys
```

```bash
./cotorra serve --listen 0.0.0.0:8099 --concurrency 2
```

The gateway never forwards a caller's Authorization header to the backend. Keys come from server configuration, are not logged, and known key environment variables are stripped from player subprocesses. Redirects are disabled; OpenAI credentials require HTTPS except explicit loopback testing. Local backend requests bypass environment HTTP proxies unless `--use-proxy` is supplied. Browser-origin requests/CORS are disabled. Use TLS termination before exposing the server beyond a trusted network; it is not an Internet-hardened public service.

The default limit is two simultaneous backend operations, matching the supplied launcher's default admission capacity. Additional requests receive 429. HTTP JSON bodies are limited to 2 MiB; speech text/output limits still apply. A gateway request has a 15-minute deadline. Readiness, liveness, and `doctor` never assert that a physical speaker is audible.

## Development and validation

```bash
go test -race -count=1 -timeout=60s -cover ./...
go vet ./...
bash scripts/build.sh
```

The build script produces Linux, macOS, and Windows binaries for amd64 and arm64 under `dist/`. Cross-compilation does not establish device-level playback compatibility. The CI workflow tests on all three operating systems and cross-builds all six targets using the stable Go toolchain.

Initial validation details and remaining hardware checks are in [TESTING.md](TESTING.md). The source has no module dependencies, so there is intentionally no `go.sum`.

## Protocol references

Implementation references checked September 7, 2026:

- [Rime introduction](https://docs.rime.ai/docs/introduction), [HTTP streaming](https://docs.rime.ai/api-reference/mistv2/http), [JSON WAV](https://docs.rime.ai/api-reference/mistv2/json-wav), [SSE](https://docs.rime.ai/api-reference/mistv2/sse), and [speed semantics](https://docs.rime.ai/docs/speed).
- [OpenAI text-to-speech guide](https://developers.openai.com/api/docs/guides/text-to-speech), including raw PCM format and model-specific options. For applications used by others, clearly disclose that the voice is AI-generated.
- [FFplay documentation](https://ffmpeg.org/ffplay-all.html) and [SoundPlayer.PlaySync](https://learn.microsoft.com/en-us/dotnet/api/system.media.soundplayer.playsync).
- The user-supplied `run-kokoro82m-dgx-spark-docker-v1(2).sh`, specifically its embedded speech request parser and HTTP handler. The launcher itself is not redistributed or changed here.
