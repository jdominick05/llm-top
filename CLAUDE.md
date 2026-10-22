# llm-top

Full-screen Go TUI (Bubble Tea v2 + Lip Gloss v2) that watches a llama.cpp `llama-server` or an LM Studio server
and the Qwen Code agents using it. Windows and Linux (amd64 releases). Module `llmtop`, go.mod says `go 1.26.0` (CI and the
release workflow use that), local builds use Go 1.27.1. License AGPL-3.0-or-later; every source and script
starts with the SPDX header, keep it on new files.

Machine-specific notes (ports, paths, what must not be started) live in an untracked `CLAUDE.local.md`; nothing
machine-specific belongs in the code or the committed files.

## Data sources: read-only

| Source | Where | Cadence |
|---|---|---|
| `/health` | `server_url` (default `http://127.0.0.1:<port from server_config>`, else 8080) | 500 ms |
| `/props` | n_ctx, total_slots, alias, model path, `is_sleeping` | 2 s |
| `/models` | router-mode detection (entries with `status`); in router mode also the target and whether it sleeps, read before any `?model=` question | 10 s, every round in router mode |
| `/metrics` | Prometheus text, `llamacpp:` prefix stripped; `requests_processing` or moved token counters = busy; labelled series skipped except `spec_decode_num_accepted_tokens_per_pos_total` (kept as `name.N`); `specHist` keeps per-poll `spec_decode_*` deltas | 500 ms (5 s retry when off) |
| `/slots` | only while busy (or every round without `--metrics`), plus once at startup and once per router target switch (never while the target sleeps); `params["speculative.types"]` names the drafter (`/props` always says `none`) | 500 ms |
| Qwen Code logs | `sessions_dir/*/chats/*.jsonl`, tailed by byte offset; last 6 h, newest 6 | 1.5 s |
| Savings ledger | the same `.jsonl` files, all of them, only `usageMetadata` lines | 10 s |
| LM Studio | `/api/v1/models` (loaded instance, context_length); in between a TCP connect (not logged) checks liveness | 10 s (`lmModelsEvery`), at once when a request starts |
| LM Studio chats | newest `~/.lmstudio/conversations/**/*.conversation.json` (sizes per kind, `tokenCount`) | 2 s |
| LM Studio log | newest `lmstudio_logs/*/*.log`, tailed by offset (256 KB backfill on start, offsets kept per file); status lines only | 500 ms |
| Machine counters | Windows: one `powershell` call (CIM GPU memory/engine/process + registry, one `gpu` line per card matched to its counters by `DEVPKEY_Gpu_Luid`, plus the NPU: PnP ComputeAccelerator, its compute-only adapter's engine and memory counters); Linux: `/proc` (the server's `fdinfo`: `drm-memory-vram`/`gtt` per `drm-pdev`, one count per `drm-client-id`), amdgpu sysfs (`PCI_SLOT_NAME` from `uevent`), `nvidia-smi` | 5 s |
| `xrt-smi examine -r aie-partitions` | Windows, only when the probe found an NPU and `%SystemRoot%\System32\AMD\xrt-smi.exe` (or one on PATH) exists; text table parsed from stdout (its JSON needs `-o <file>`, which would break the write-nothing rule) | 5 s |
| Hold file | `hold_file` (first line shown as HOLD) | 5 s |
| OpenRGB SDK | `openrgb.host` (TCP, protocol <= 5), only with `openrgb.enabled` and not in `-once`; the one connection llm-top writes over (UPDATELEDS to Direct-mode devices, restored on exit) | 80 ms frames, sent on change |

Why `/slots` is gated: in llama.cpp's source, `GET /slots` waits for a sleeping server to wake and its
`SLOT_GET` task resets the `--sleep-idle-seconds` timer; only `METRICS` tasks don't. `/health`, `/props`,
`/models`, `/metrics` never wake it. Don't poll `/slots` unconditionally, and never ask a router about a model
that isn't `loaded`/`sleeping` (that could load it).

Never write to the server (GET only, no slot actions) or to the session logs; the opt-in OpenRGB connection is the
only thing llm-top sends commands to, and it never switches device modes. The only file the tool writes is
its config, and only when it does not exist yet (`loadConfig`).

## Layout

Repo root: `go.mod`/`go.sum`, scripts, workflows, docs. All Go code (package `main`) and `testdata/` live in `src/`;
build and test with `./src` or `./...` from the root.

- `main.go`: palette, session parsing (`session.apply`, `tail`, `estimate`), `state`, `scanSessions`,
  `projectName`, drawing helpers (`panel`, `gauge`, `sparkline`, `columns`), the Bubble Tea `model`, `render()`,
  `main()` (hand-parsed flags).
- `server.go`: `srvClient.round()` does the I/O for one poll and returns a `probe`; `serverState.apply()` is
  pure (slot pick, per-slot rates, states). `routerTarget`, `parseMetrics`.
- `lmstudio.go`: `pickLM` (models JSON), `lmLog.tail` + `lmState.line` (log state machine; anchored at column 0
  so request bodies, i.e. the user's chats, never match). `srvClient.detect` tells the backends apart,
  `lmRound` / `applyLM` in server.go. Auto mode tries `:8080` then `:1234`, rotating while nothing answers.
- `specRows` (main.go) / `specHist`, `drafters`, `specPos` (server.go): the speculative decoding rows in THROUGHPUT.
- `bottleneck.go`: `bottleneck()` (pure verdict, thresholds as consts) and `fallbackRows()` for THROUGHPUT.
- `sys.go`: `probeSys` switches on `runtime.GOOS`; parsers (`parseWinProbe`, `probeLinux`, `parseNvidiaSmi`) are
  plain functions so tests run on any OS. No build tags. Several GPUs (Windows): `useCards` sums the cards holding
  at least `modelCardGB` of the process's VRAM; `overflow` (bottleneck.go) judges spill per card. Linux: the same via
  `amdGPUs` + `drmMem` (amdgpu); NVIDIA: biggest card only.
- `npu.go`: `npuState` (filled by the Windows probe's `npu_*` lines), `parseXrtSmi` (text table, fixture
  `testdata/xrt_smi_aie_partitions.txt`), `npuRate` (completions/s between probes, in `rates()`), `npuLine`. No NPU power or
  temperature (the drivers expose neither) and no clock (AMD's needs XRT's C++ API, i.e. cgo).
- `openrgb.go`: SDK client (`parseController`, `ledsPacket`), `rgbPhase` (from `agentPhase`, shared with the
  AGENTS "now" line), `looks` per phase, `rgbLights.run` (reconnects, re-lists on DEVICE_LIST_UPDATED, restores on
  `stop`). Tests: `openrgb_test.go` with a mock SDK server.
- `config.go`: `config` (embeds `priceCfg`), lookup order, `rel()` path resolution, `server()`.
- `savings.go`: `priceCfg` defaults, the per-day token `ledger` (`update`), `cost`.
- Tests: `server_test.go` (mock llama-server per setup: idle, busy, no metrics, `--no-slots`, api key,
  loading/down, multi-slot, router, sleeping, speculative decoding), `sys_test.go` (fake /proc + /sys), `config_test.go`,
  `session_test.go`, `lmstudio_test.go` (log states, rotation, mock LM Studio, candidates), `bottleneck_test.go`. `frame()` renders and fails on any line wider than the terminal. `testdata/` holds real
  `/slots`, `/metrics`, `/props` captures (anonymized).
- Panels: CONTEXT (left), THROUGHPUT, SAVINGS, MACHINE, AGENTS (right), ACTIVITY (full width, fills the rest).
  Below 110 columns everything stacks. The frame never exceeds the terminal height: panels are `box`es that
  `shrink()` trims (SAVINGS, MACHINE, CONTEXT, AGENTS list, THROUGHPUT, each down to its `min` first) before
  ACTIVITY drops below 3 rows; `frame()` in tests fails on too many lines as well as too wide ones.
- `install.ps1` / `install.sh` (oneshot installers), `release.ps1`, `.github/workflows/{ci,release}.yml`.

All pollers write into `state` under `st.mu`; `render()` holds the lock for the whole frame. Session files are
parsed on a copy outside the lock and swapped in.

## Build and verify

```powershell
gofmt -l .                      # must print nothing
go vet ./...
go test ./...
go build -o llm-top.exe ./src   # plain `go build ./src` would name it src.exe
.\llm-top.exe -config <your llm-top.json> -once 120 40   # live frame, no TTY needed
.\llm-top.exe -config <your llm-top.json> -once 90 40    # narrow layout
```

Pass `-config` for live checks: a dev build in the repo has no config next to it and would otherwise create
`%APPDATA%\llm-top\llm-top.json` with defaults (tries :8080 and :1234). New server behaviour gets a mock scenario
in `server_test.go`; LM Studio behaviour a log fixture in `lmstudio_test.go`. `TestRenderAllSizes` renders every
terminal size. Do not add dependencies.

Never start LM Studio, load models or run `lms` commands to test (`lms ls` starts LM Studio's service); use the
mock. Never kill a running llm-top; a build onto its exe fails while it runs. Install tests that would touch the
user PATH use `-NoPath`. Windows probe notes: shared GPU memory ~0.35 GB at rest is normal, `PrivateBytes` includes
WDDM-committed VRAM (so it can't measure paging), GPU load is on the Compute engine, WQL `LIKE` needs `[_]`.
LM Studio 0.4 runs its engine as `llama-server` under `~/.lmstudio/extensions/backends` (parent: LM Studio); it
holds the model's VRAM. Its log's continuation lines start with `h.mm.sss.uuu` (one-digit hours early on). Chats
from LM Studio's own window log only those `slot ...` lines; every API request is logged, so poll sparingly.

## Install

Users: `install.ps1` / `install.sh` (README). Developers: `go build -o <dir>\llm-top.exe ./src`, or `release.ps1`
with `-InstallTo` / `$env:LLMTOP_INSTALL`.

## Config

See the table in README.md (keep both in sync with `defaultConfig`). `LLMTOP_VRAM_GB` overrides `vram_gb`,
`LLAMA_API_KEY` fills an empty `api_key`. Context size: slot `n_ctx`, else `/props`, else `context`, else 131072
shown as "131k?".

## Design rules

- The context composition (waffle + legend) is an estimate: characters per part type since the last
  compaction / 3.6, scaled to the session's last reported prompt size. Keep it labelled "est.".
- Read-only: never send anything but GETs to the server, never write the session logs.
- One static binary (CGO_ENABLED=0), no runtime files besides `llm-top.json`, no new dependencies.
- Match the existing style: short names, `fg(c).Render`, `fmt.Sprintf` rows passed to `panel()`, flags parsed by
  hand from `os.Args` (no `flag` package), section comments `// ---------- name ----------`, gofmt.

## Releases

1. Add the changes under a new version heading in `CHANGELOG.md` (`## 0.2.0 - date`; the release workflow
   refuses without it and uses that section as the release notes) and commit. The tree must be clean.
2. `.\release.ps1 0.2.0` (semver, no `v`): refuses on a dirty tree or existing tag, runs `go vet`, builds
   windows/amd64, writes `dist\llm-top-0.2.0-windows-amd64.zip` + `dist\SHA256SUMS`, creates the annotated tag
   `v0.2.0`, and copies the exe to `-InstallTo` / `$env:LLMTOP_INSTALL` if given (a file or a folder, relative to
   where you run it; refuses if that exe is in use, `-SkipInstall` then). The copy happens before the tag, so a
   failed install leaves no tag. A `-rc.N` version is published as a prerelease.
3. Only when the user asks to publish: `git push origin main v0.2.0`. `release.yml` reruns CI, builds
   windows/amd64 (zip) and linux/amd64 (tar.gz) with `-trimpath -ldflags "-s -w -X main.version=..."`, writes
   `SHA256SUMS`, creates the GitHub Release, then runs both install scripts on fresh runners.
4. Check the installed exe's `-version` and the release page.

Remote: `origin` = github.com/jdominick05/llm-top. CI runs gofmt, vet, test (`-race` on Linux), build, a `-once`
smoke run and an install-script syntax check on windows-latest and ubuntu-latest for every push to main. Push main freely
after committing; push tags only when the user asks for a release.

Commits end with the `Co-Authored-By` trailer given in the session instructions.
