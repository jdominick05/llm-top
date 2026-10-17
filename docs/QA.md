# Release QA checklist

A manual pass before a release, run on a real Windows machine as a new user would. Tick each box; anything that
doesn't match "expect" is a bug (note what you saw). Automated tests cover the server setups and states that are
hard to produce by hand (router mode, `--no-slots`, API keys, multi-slot, LM Studio logs).

**Two passes.** Pass 1 runs a local build before the release (sections 2-7). Pass 2 runs after the release is
public (sections 1, 8, 9, and a quick repeat of 2-3 with the installed exe).

## 0. Start clean

- [ ] No old config: `Test-Path $env:APPDATA\llm-top\llm-top.json` → `False` (rename it away if it exists).
- [ ] llama-server is running, and you know its port (below: `<port>`).
- [ ] Pass 1 only, from the repo: `go build -o $env:TEMP\llm-top-qa\llm-top.exe ./src`, then use
      `$env:TEMP\llm-top-qa\llm-top.exe` wherever this list says `llm-top`.

- [ ] The terminal uses **MesloLGS NF** (README → Font). Once, also look at a frame in Cascadia Mono: no panel
      border may be pushed out of line.

## 1. Install (pass 2)

In a new **Windows PowerShell 5.1** window (not pwsh), not logged in to `gh`, no `GITHUB_TOKEN`:

- [ ] `irm https://raw.githubusercontent.com/jdominick05/llm-top/main/install.ps1 | iex`
      expect: "installed llm-top 0.2.0 to …\Programs\llm-top\llm-top.exe", "added … to the user PATH", a config line.
- [ ] Same window afterwards: `$ErrorActionPreference` is still `Continue`.
- [ ] A **new** terminal: `llm-top -version` → `llm-top 0.2.0`.
- [ ] Your other PATH entries are intact: `(Get-Item HKCU:\Environment).GetValue('Path','', 'DoNotExpandEnvironmentNames')`
      still shows the `%…%` entries unexpanded.
- [ ] Run the one-liner again: it replaces the exe and does not add PATH a second time.
- [ ] With `llm-top` open in another window, run it again: expect "… is running (pid …); close it first", nothing changed.

## 2. First run

- [ ] `llm-top` with no config: a full-screen frame within a second; `%APPDATA%\llm-top\llm-top.json` now exists.
- [ ] llama-server on another port (e.g. `--port 8091`), no `server_url` set: llm-top finds it by itself within a few
      seconds (header shows `:8091`).
- [ ] No server running at all and LM Studio's server off: `DOWN`, naming the addresses it tried.
- [ ] With LM Studio's server on (Developer → Start Server) and no `server_url`: it finds LM Studio on `:1234` by
      itself (`NO MODEL` until a model is loaded, then the model and its context length).
- [ ] With no server or no model: CONTEXT says "no live context: …", THROUGHPUT shows "— tok/s" and the `▸` line
      gives the same reason (no "0 / 131k?", no "GPU-bound"); the breakdown says "what filled it · … · Nm ago".
- [ ] Start it when no agent has run for 10+ minutes: CONTEXT and ACTIVITY are empty ("no agent active …"), AGENTS
      lists the old sessions; `tab` brings one back with "N m ago". With LM Studio: "no request since llm-top
      started", context 0 until you chat; after switching models in LM Studio the old model's numbers are gone.
- [ ] Right after start, before the server answers, the header briefly says `CONNECTING`, not `DOWN`.
- [ ] `q` quits and the terminal is restored (prompt back, no leftover colors or alt screen). Same for `esc` and `ctrl+c`.
- [ ] Set `"server_url": "http://127.0.0.1:<port>"` in the config, start `llm-top` again: `IDLE`, the model alias and
      file name in the header, the context size in CONTEXT matches the server's `-c`.
- [ ] `llm-top -once 120 40` prints one frame after a few seconds and exits; `llm-top -once 30 20` too (cut off, no crash).

## 3. Live states (one agent request)

Start a Qwen Code request against the server and watch:

- [ ] `READING` with a progress bar, percentage, "new + reused" and an ETA that counts down; the header spinner moves.
- [ ] `WRITING` with tokens and tok/s; the write sparkline in THROUGHPUT rises.
- [ ] Back to `IDLE` when the reply is done; "for X" times looked right during the request.
- [ ] CONTEXT: the gauge grows during the request; "what fills it" shows the session's project name and "est.".
- [ ] In Qwen Code run `/compress` (or `/compress-fast`): the header shows `✂ COMPACTING` ("reading the history",
      then "writing the summary"), AGENTS says "compacting", and after it ACTIVITY shows the compaction and the
      prompt-size chart gets a red bar.
- [ ] A normal request right after that shows plain `READING` / `WRITING` again (not COMPACTING).

## 4. Sessions

- [ ] AGENTS lists your recent Qwen Code sessions (last 6 h) with project names that make sense.
- [ ] `tab` / `shift+tab` move the selection; CONTEXT and ACTIVITY follow it.
- [ ] With session B selected, let session A do something: the selection **stays on B**.
- [ ] ACTIVITY shows tool calls with a spinner while running, then ✓ / ✗ with a duration.

## 5. Window size

- [ ] Drag the window down to a few columns and rows and back up: never crashes, no wrapped or garbled lines.
- [ ] Below 110 columns everything stacks in one column; at 110+ two columns.
- [ ] Maximize on a large screen: layout caps at 200 columns, still readable.

## 6. Machine panel and bottleneck

Compare with Task Manager → Performance → GPU:

- [ ] MACHINE VRAM used/total within ~0.5 GB of "Dedicated GPU memory"; RAM matches Memory.
- [ ] THROUGHPUT `load`: GPU % high while writing, near 0 idle; CPU cores small for a full-GPU server.
- [ ] The `▸` verdict is green "GPU-bound (normal)" while busy and "idle · no fallbacks · N GB VRAM headroom" idle.
      While a model **loads**, it must not say "paging" if plenty of RAM is free.
- [ ] `spill RAM` ≈ Task Manager's "Shared GPU memory" for the server (about 0.35 GB is normal).

## 7. Edge states (as far as your setup allows)

- [ ] Stop llama-server: `DOWN` within a few seconds; AGENTS stops spinning; no COMPACTING left over.
- [ ] Start it again: `LOADING` while the model loads, then `IDLE`, without restarting llm-top.
- [ ] Hold file: `Set-Content $env:APPDATA\llm-top\llm-watchdog.hold "qa test"` → purple `HOLD` with "qa test"
      within 5 s; delete the file → back to normal.
- [ ] Break the config (e.g. delete a quote): llm-top still starts with defaults and does **not** overwrite the file.
- [ ] Optional, needs free VRAM: with LM Studio's server on and a model loaded, `backend: auto` and no `server_url`
      finds it on :1234; a chat from another app shows READING / WRITING.

## 8. Linux (pass 2, optional: WSL or a Linux box)

- [ ] `curl -fsSL https://raw.githubusercontent.com/jdominick05/llm-top/main/install.sh | sh` → installed to
      `~/.local/bin/llm-top`, PATH note if needed.
- [ ] `llm-top -once 120 40` runs.

## 9. Uninstall (pass 2)

- [ ] Delete `%LOCALAPPDATA%\Programs\llm-top` and remove it from the user PATH (Settings → "Edit environment
      variables for your account"); `%APPDATA%\llm-top` for the config. A new terminal no longer finds `llm-top`.
