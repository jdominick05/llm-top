# Contributing to llm-top

Thanks for helping. Bug reports from real setups are the most useful thing right now: every llama.cpp build, GPU,
terminal and LM Studio version behaves a little differently.

## Reporting a bug

Open an [issue](https://github.com/jdominick05/llm-top/issues/new/choose) with the **Bug report** form. It asks for:

- `llm-top -version`, your OS, and the terminal and font (llm-top is made for MesloLGS NF, see the README).
- The server: llama-server (its version and the flags you start it with) or LM Studio (version, model).
- What you expected, and what you saw. A screenshot helps most.
- A frame as text: `llm-top -once 120 40 > frame.txt`. **Read it before you attach it**: the ACTIVITY panel shows
  your agent's recent prompts, tool calls and file names. Cut anything private.

Please don't paste session logs, LM Studio conversation files or API keys.

**Security problems** (llm-top doing something other than read-only GETs, leaking a key or chat text): use
[a private advisory](https://github.com/jdominick05/llm-top/security/advisories/new) instead of a public issue.

## Asking for a feature

Use the **Feature request** form. Say what you want to see and why; a sketch of the line or panel is welcome.
llm-top stays read-only (it never sends anything but GETs to the server and never writes your logs) and a single
static binary without runtime dependencies, so ideas that need either won't fit.

## Sending a change

1. Open an issue first for anything bigger than a small fix, so we can agree on the approach.
2. Fork, branch from `main`, and keep the change focused.
3. Before you push:

   ```sh
   gofmt -l .        # must print nothing
   go vet ./...
   go test ./...
   go build -o llm-top ./src && ./llm-top -once 120 40
   ```

4. New server behaviour gets a test with a mock server (`src/server_test.go`), LM Studio behaviour a log fixture
   (`src/lmstudio_test.go`). `TestRenderAllSizes` must keep passing: no line may be wider than the terminal.
5. Match the existing style (short names, `fg(c).Render`, flags parsed by hand, no new dependencies) and keep the
   SPDX header on new files.
6. Open a pull request describing what changed and how you tested it. CI runs on Windows and Linux.

By contributing you agree that your contribution is licensed under the project's license,
[AGPL-3.0-or-later](LICENSE).
