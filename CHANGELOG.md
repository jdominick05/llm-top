# Changelog

## Unreleased

- THROUGHPUT: speculative decoding rows named after the drafter(s) in the slot's `speculative.types`
  (`ngram-mod`, `dflash`, `dflash+ngram`, else `draft`) instead of "ngram accept": acceptance over the last 120
  polls that drafted (with a sparkline) next to the rate since server start, accepted tokens per draft (recent and
  since start) and, if there is room, acceptance per draft position. Hidden when the server does not draft.
  Counters going down (a server restart) start the recent window over.
- MACHINE: an NPU row on Windows machines that have one (PnP class ComputeAccelerator): load and memory from the
  GPU performance counters of the compute-only adapter, and the processes using it. With AMD's `xrt-smi` installed
  (Ryzen AI NPUs) also the hardware contexts, how many are active, completed commands per second and their
  processes, read from `xrt-smi examine -r aie-partitions`'s text output (nothing is written). Hidden without an NPU.
- OpenRGB lighting, off by default (`"openrgb": {"enabled": true}`): RGB devices in Direct mode show the agent's
  phase (writing, reading, tool, waiting for you, ...) through OpenRGB's SDK server, with per-phase colors in the
  config; their colors are restored on exit. An `rgb` line in AGENTS shows the connection.
- Fixed: in router mode llm-top could keep asking about a model for up to 10 s after the router unloaded it (with
  autoload that could load it back), and moving to a sleeping model could send it a `/slots` request that woke it.
  `/models` is now read every round in router mode, and a new target starts with no carried-over state.
- Fixed: the frame could be taller than the terminal (always below 110 columns, and on short terminals), pushing
  the header out of view. Panels now give up rows to fit, least-used first (SAVINGS, MACHINE, the older parts of
  CONTEXT, the AGENTS list), whole sections at a time; the AGENTS "now" line and the context gauge stay longest.
- Fixed: one config value of the wrong type (`"vram_gb": "24"`) threw the whole file away, server URL and API key
  included. Now only that field keeps its default. Config errors show in the footer instead of on stderr, which the
  full-screen view hid.
- Fixed: right after a compaction CONTEXT put nearly the whole pre-compaction prompt into "system + tools"; it now
  shows the unscaled estimate until the next request reports a size. A session line appended within the same
  file-time tick as the one before is no longer missed (a tool stayed "running"), and sessions that left the last
  6 hours are forgotten instead of kept in memory for the life of the process.
- Fixed (LM Studio): a chat in LM Studio's own window after a Qwen Code compaction showed as COMPACTING; a
  generation running longer than 15 minutes at INFO verbosity went idle and was never listed in ACTIVITY (it now
  still counts when its end is logged); two logs taking turns as the newest (two instances) were replayed from the
  start on every switch.
- Install and release: `install.ps1 -Dir` with a relative path resolves against the current PowerShell location;
  prerelease tags (`v1.2.3-rc.1`) are published as GitHub prereleases, so the one-line installers never pick them;
  CI also parses the PowerShell scripts with Windows PowerShell 5.1.

## 0.2.0 - 2026-10-17

First public release. Windows (amd64) and Linux (amd64). Licensed AGPL-3.0-or-later.

- Live view of a llama-server: IDLE / READING / WRITING with prompt progress and ETA, plus BUSY, LOADING, SLEEP,
  AUTH, ROUTER, DOWN and HOLD states.
- Works with these llama.cpp setups (checked against the current server README and source, with mock-server tests
  for each): default, `--metrics` on or off, `--no-slots`, `--api-key`, several slots (`-np`), `--sleep-idle-seconds`
  (never wakes the server; `/slots` is skipped while `/metrics` says idle), router mode (watches `router_model` or
  the first loaded model, never asks about unloaded ones), old and current `next_token` shapes, `/health` 503 while
  loading.
- CONTEXT panel: context-window gauge (reused / read / written), estimated composition of the selected
  session's context ("est."), and prompt size per request with compactions marked.
- LM Studio (0.4+) out of the box: found on :1234 when nothing is configured (llama-server's :8080 is tried
  first), loaded model and context length from `/api/v1/models`, live READING / WRITING from LM Studio's server log
  (status lines only), NO MODEL and AUTH (`LM_API_TOKEN`) states.
- THROUGHPUT panel: read and write tok/s sparklines (summed over slots), cache reuse, n-gram draft acceptance,
  totals and averages since server start, plus the inference process's fallbacks (VRAM spilled to system RAM,
  page file / swap and disk read-back, GPU load, CPU cores) and a bottleneck verdict: paging, VRAM full,
  CPU-bound, GPU-bound (normal), or busy-wait while idle.
- SAVINGS panel: API cost of all Qwen Code traffic vs. estimated GPU electricity, all time and today, from
  session-log token counts (Claude Sonnet 5.5 defaults).
- MACHINE panel: VRAM used/total (total detected from the registry on Windows, amdgpu sysfs or `nvidia-smi` on
  Linux), RAM, and the server process's memory. Windows counters come from CIM, so they work in any display
  language.
- AGENTS and ACTIVITY panels: Qwen Code sessions from the last 6 h (named after their working directory),
  tab / shift+tab to switch, live feed of prompts, tool calls with duration and result, replies and compactions.
- `llm-top.json` config for what can't be detected: `server_url`, `backend`, `api_key` (or `$LLAMA_API_KEY` /
  `$LM_API_TOKEN`), `router_model`, `server_config`, `server_process`, `sessions_dir`, `hold_file`,
  `lmstudio_logs`, `vram_gb`, `context`, and the savings prices. Found via `-config`, `$LLMTOP_CONFIG`, next to the exe, or the user config dir.
- `-config <path>`, `-once [w h]` (one frame without a TTY), `-version`.
- `install.ps1` and `install.sh`: download a release, verify `SHA256SUMS`, install and put it on PATH.
- GitHub Actions: CI on Windows and Linux (gofmt, vet, tests, smoke run); tag pushes publish the release.
- COMPACTING state: a Qwen Code compaction (its 20000-token summary budget, seen in `/slots` or LM Studio's
  logged request) shows as "✂ COMPACTING" (reading the history / writing the summary) instead of READING /
  WRITING, and AGENTS says the session is compacting.
- Robust on odd setups: any terminal size (laid out at 40 columns minimum), integrated GPUs and unified memory
  (no false "VRAM full"), CPU-only servers (GPU load "unknown", not 0%), Ryzen iGPU + NVIDIA on Linux, mmap'd model
  loads on Windows (no false "paging"), slow or saturated servers (no DOWN flash), crashes mid-request, LM Studio
  restarts, truncated session logs, zero or negative prices in the config.
- tab / shift+tab stay on the chosen session while others become more active.
- Without an agent session CONTEXT describes the loaded model (name, size, quantization, context, slots,
  capabilities) and ACTIVITY lists the server's requests since llm-top started (new / cached prompt, written,
  read and write speed, time, context), for llama-server and LM Studio alike, including chats in LM Studio's window.
- LM Studio chats get the context breakdown too: the newest saved conversation (`~/.lmstudio/conversations`) shows
  what fills it (prompts, thinking, replies with LM Studio's own token counts, system prompt) and the prompt size
  per reply, while no agent session is active. Only sizes are read into llm-top's state; no text is shown.
- Finds a llama-server on any port without config: with no `server_url`, the port the running `server_process`
  listens on is tried after :8080 and :1234 (Windows: TCP table, Linux: /proc/net/tcp).
- A clean slate on startup: an agent session quiet for more than 10 minutes no longer fills CONTEXT and ACTIVITY
  (AGENTS lists it, tab shows it with its age; a request only brings back a session active when it started); LM Studio's log is read back only to see whether a request is
  running, so earlier requests' numbers, and those of a model that has since been swapped out, are not shown.
- Made for the MesloLGS NF font (README → Font): every glyph llm-top draws is in it (a test checks), the spinner
  no longer uses braille, which that font lacks.
- LM Studio: chats in LM Studio's own window and any API (`/api/v1/chat`, OpenAI, Anthropic endpoints) show
  READING / WRITING with speed and context use, from the engine's slot lines; the model list is asked every 10 s
  (LM Studio logs every request) with a silent TCP check in between; MACHINE and the bottleneck line watch LM
  Studio's engine (its own llama-server under `~/.lmstudio`), which is never mistaken for your llama-server.
- No stale picture: until the server first answers the header says CONNECTING, and while there is no live model
  (no server, loading, asleep, no model loaded, API key needed, or a server process that doesn't answer) CONTEXT,
  THROUGHPUT and the bottleneck line say why instead of showing zeros; the context breakdown from the session log
  says how old it is.
