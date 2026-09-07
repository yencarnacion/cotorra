# Initial validation — September 7, 2026

## Completed locally

- `go test -race -count=1 -timeout=25s -coverprofile=... ./...`: passed, **69.6% statement coverage**, 21 top-level tests plus table-driven subtests. The toolchain was Go 1.23.2 on Linux/amd64.
- `go vet ./...`: passed.
- `go.sh version`: built and ran the native CLI successfully.
- `CGO_ENABLED=0 go build` passed for **linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64**.
- Integration against the **actual Python HTTP service extracted from the supplied Kokoro launcher**, with only neural synthesis replaced by a deterministic test-tone engine: authenticated voice discovery, model readiness, WAV export, multi-request Spanish input, and streaming through FFplay all passed. FFplay used SDL's dummy audio driver; this was not a physical-speaker listening test. Generated WAV files were independently checked using Python's wave reader.

Tests cover dotenv quoting/precedence, explicit empty text, local/cloud key separation, strict Kokoro request fields and Content-Length, OpenAI request fields, Unicode splitting, cancellation, redirect rejection, upstream errors and key redaction, empty/odd/wrong-format audio, WAV headers, all platform player-selection branches, player subprocess failure, CLI file/stdin/stdout output, no-overwrite/input-alias protection, HTTP authentication, Accept negotiation, WAV/base64 JSON/PCM/SSE, API validation, first-audio delivery before completion, truncated chunked HTTP detection, and concurrency limits.

## Not validated here

- The real endpoint `http://10.17.17.99:9084/health` could not be reached from the build environment. This does not establish that the user's LAN service is down.
- No real OpenAI API call was made; no OpenAI API key was provided to this build environment. That backend was tested with HTTP fixtures.
- No physical speakers, real Kokoro GPU inference, macOS afplay runtime, or Windows SoundPlayer runtime were available for listening tests. Cross-compilation and player command tests do not replace these checks.
- A three-operating-system GitHub Actions workflow is included. Its current result must be checked in the repository's Actions tab; these local results do not assert a CI run passed.

## Checks on the user's computer

```bash
./cotorra doctor
./cotorra voices
./cotorra "Cotorra is ready. This is a speaker test."
./cotorra --stream=on "This checks streaming playback."
./cotorra --no-play --output test.wav "This checks saving audio."
./cotorra --provider openai "This checks the optional OpenAI backend."
```

On Windows replace `./cotorra` with `.\cotorra.exe`. The streaming-only check requires FFplay on macOS/Windows or a listed raw PCM player on Linux. The OpenAI check requires its API key and may incur charges. `doctor` only checks executable availability and model metadata, not audibility or synthesis.

Use a maintained Go toolchain for production builds. No prebuilt binaries from the older local test toolchain are committed.
