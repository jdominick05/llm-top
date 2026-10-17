// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// LM Studio: GET /api/v1/models (loaded instances, context length) plus its server log for live state.
//
// The API has no live progress, but LM Studio logs each request to ~/.lmstudio/server-logs/YYYY-MM/<day>.N.log:
// "Running chat completion", "Prompt processing progress: P%", "Finished streaming response" at INFO, and with a
// higher runtime log verbosity llama.cpp's slot lines (n_gen, tg_3s, prompt eval, n_tokens). The log also holds
// request bodies (the chat itself). Only lines that start with a "[date][LEVEL]" header or a llama.cpp timestamp
// are looked at, and only the status patterns below are kept; message text never leaves this file.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type lmModels struct {
	Models []struct {
		Type    string `json:"type"`
		Key     string `json:"key"`
		Display string `json:"display_name"`
		MaxCtx  int    `json:"max_context_length"`
		Params  string `json:"params_string"`
		Size    int64  `json:"size_bytes"`
		Quant   struct {
			Name string `json:"name"`
		} `json:"quantization"`
		Caps struct {
			Vision bool `json:"vision"`
			Tools  bool `json:"trained_for_tool_use"`
		} `json:"capabilities"`
		Instances []struct {
			ID     string `json:"id"`
			Config struct {
				Ctx       int  `json:"context_length"`
				Parallel  int  `json:"parallel"`
				FlashAttn bool `json:"flash_attention"`
			} `json:"config"`
		} `json:"loaded_instances"`
	} `json:"models"`
}

// lmPick is the loaded instance to show: want (router_model) by key or instance id, else the one the log is
// talking about, else the first loaded LLM.
type lmPick struct {
	Key     string
	Info    modelInfo
	Ctx     int
	Loaded  int // loaded LLM instances
	Unknown bool
}

func pickLM(b []byte, want, logKey string) (lmPick, bool) {
	var m lmModels
	if json.Unmarshal(b, &m) != nil || m.Models == nil {
		return lmPick{}, false
	}
	var p lmPick
	score := -1
	for _, md := range m.Models {
		if md.Type != "" && md.Type != "llm" && md.Type != "vlm" {
			continue
		}
		for _, in := range md.Instances {
			p.Loaded++
			s := 0
			switch {
			case want != "" && (md.Key == want || in.ID == want):
				s = 3
			case logKey != "" && (md.Key == logKey || in.ID == logKey):
				s = 2
			case score < 0:
				s = 1
			}
			if s > score {
				score = s
				p.Key, p.Ctx = in.ID, in.Config.Ctx
				if p.Key == "" {
					p.Key = md.Key
				}
				p.Info = modelInfo{Name: md.Display, Params: md.Params, Quant: md.Quant.Name, SizeGB: float64(md.Size) / 1e9,
					MaxCtx: md.MaxCtx, Slots: in.Config.Parallel, FlashAttn: in.Config.FlashAttn, Vision: md.Caps.Vision, Tools: md.Caps.Tools}
			}
		}
	}
	return p, true
}

// ---------- server log ----------

type lmState struct {
	Phase     string  // "", "reading", "writing"
	Pct       float64 // prompt processing
	Decoded   int
	GenRate   float64 // tok/s while writing (tg_3s)
	Prompt    int     // tokens of the last finished prompt
	ReadRate  float64 // its speed
	Ctx       int     // tokens in the slot at the last stop
	Model     string  // from the [INFO][model] tag
	Debug     bool    // llama.cpp slot lines seen: write speed and context use are known
	MaxTokens int     // max_tokens of the running request (from the logged request body, debug verbosity)
	inBody    bool
	pending   int
	Reqs      int
	Found     bool      // a log file exists
	Started   time.Time // when the running (or last) request began
	since     time.Time
}

type lmLog struct {
	dir  string
	path string
	off  int64
	rest []byte
	st   lmState
}

var (
	lmHead     = regexp.MustCompile(`^\[\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\]\[([A-Z]+)\](?:\[([^\]]+)\])? ?(.*)$`)
	lmCont     = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+ I (.*)$`) // llama.cpp's h.mm.sss.uuu since start: 3.28.112.331
	lmProgress = regexp.MustCompile(`^Prompt processing progress: ([\d.]+)%`)
	lmTiming   = regexp.MustCompile(`^(?:slot )?print_timing: .*n_gen = +(\d+), tg = +([\d.]+) t/s(?:, tg_3s = +([\d.]+) t/s)?`)
	lmPromptT  = regexp.MustCompile(`^(?:slot )?print_timing: .*\| +prompt eval time = +[\d.]+ ms / +(\d+) tokens \(.*?([\d.]+) tokens per second\)`)
	lmEvalT    = regexp.MustCompile(`^(?:slot )?print_timing: .*\| +eval time = +[\d.]+ ms / +(\d+) tokens \(.*?([\d.]+) tokens per second\)`)
	lmStop     = regexp.MustCompile(`^(?:slot +)?release: .*\| stop processing: n_tokens = (\d+)`)
	lmLaunch   = regexp.MustCompile(`^slot +launch_slot_: .*\| processing task`)
	lmMaxTok   = regexp.MustCompile(`^  "max_tokens": (\d+),?$`) // a top-level field of a logged request body
)

const lmBackfill = 256 << 10 // on startup, read the end of today's log to know the current state

// tail reads what was appended to the newest log; a new file (next day, rotation) is read from the start.
func (l *lmLog) tail(now time.Time) lmState {
	files, _ := filepath.Glob(filepath.Join(l.dir, "*", "*.log"))
	newest, nt := "", time.Time{}
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(nt) {
			newest, nt = f, fi.ModTime()
		}
	}
	l.st.Found = newest != ""
	if newest == "" {
		return l.st
	}
	f, err := os.Open(newest)
	if err != nil {
		return l.st
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return l.st
	}
	skip, startup := false, l.path == ""
	if newest != l.path {
		first := l.path == ""
		l.path, l.off, l.rest = newest, 0, nil
		l.st.Phase = "" // LM Studio restarted (a new numbered log) or the day changed: no request carries over
		if first && fi.Size() > lmBackfill {
			l.off, skip = fi.Size()-lmBackfill, true // the first line is cut
		}
	}
	if fi.Size() < l.off {
		l.off, l.rest = 0, nil
	}
	if _, err := f.Seek(l.off, io.SeekStart); err != nil {
		return l.st
	}
	rd := bufio.NewReaderSize(f, 1<<16)
	for {
		chunk, err := rd.ReadBytes('\n')
		l.off += int64(len(chunk))
		if len(chunk) > 0 {
			if chunk[len(chunk)-1] != '\n' {
				l.rest = append(l.rest, chunk...)
			} else {
				line := append(l.rest, chunk...)
				l.rest = nil
				if skip {
					skip = false
				} else {
					l.st.line(string(line), now)
				}
			}
		}
		if err != nil {
			break
		}
	}
	if l.st.Phase != "" && now.Sub(l.st.since) > 15*time.Minute { // missed the end of a request
		l.st.Phase = ""
	}
	if startup { // the backfill is only for a request that is running right now
		l.st.clearStats()
		if l.st.Phase == "" {
			l.st.Decoded, l.st.GenRate, l.st.Pct = 0, 0, 0
		}
	}
	return l.st
}

// clearStats forgets the finished requests' numbers (startup backfill, another model): only what happens from now
// on is shown. What the log taught about its verbosity stays.
func (s *lmState) clearStats() {
	*s = lmState{Phase: s.Phase, Pct: s.Pct, Model: s.Model, Debug: s.Debug, Found: s.Found, since: s.since,
		inBody: s.inBody, pending: s.pending, MaxTokens: s.MaxTokens, Decoded: s.Decoded, GenRate: s.GenRate}
}

func atoi(s string) int     { n, _ := strconv.Atoi(s); return n }
func atof(s string) float64 { x, _ := strconv.ParseFloat(s, 64); return x }

func (s *lmState) line(raw string, now time.Time) {
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\r') {
		raw = raw[:len(raw)-1]
	}
	msg := ""
	if m := lmHead.FindStringSubmatch(raw); m != nil {
		if m[2] != "" && m[1] == "INFO" && !strings.Contains(m[2], "=") { // [Client=lms-cli] is not a model
			s.Model = m[2]
		}
		msg = m[3]
		if m[1] == "DEBUG" {
			if c := lmCont.FindStringSubmatch(msg); c != nil {
				msg = c[1]
			}
		}
	} else if c := lmCont.FindStringSubmatch(raw); c != nil {
		msg = c[1]
	} else {
		// request bodies and other multi-line output: only a body's own max_tokens line is read. JSON escapes
		// newlines, so message text can never form such a line.
		if s.inBody {
			if m := lmMaxTok.FindStringSubmatch(raw); m != nil {
				s.pending = atoi(m[1])
			}
			if raw == "}" {
				s.inBody = false
			}
		}
		return
	}
	if strings.HasPrefix(msg, "Received request: ") && strings.HasSuffix(msg, "{") {
		s.inBody, s.pending = true, 0
		return
	}
	start := func(ph string) {
		if s.Phase == "" { // a new request: its numbers start from zero
			s.Started, s.Prompt, s.ReadRate = now, 0, 0
		}
		s.since = now // last sign of life: a long request with progress lines never goes stale
		s.Phase = ph
	}
	switch {
	case lmLaunch.MatchString(msg): // a slot takes a task: also LM Studio's own chat window, which logs no API lines
		if s.Phase == "" {
			s.Pct, s.Decoded, s.GenRate = 0, 0, 0
		}
		start("reading")
	case msg == "Running chat completion" || len(msg) > 23 && msg[:23] == "Running chat completion":
		start("reading")
		s.Pct, s.Decoded, s.GenRate = 0, 0, 0
		s.MaxTokens, s.pending, s.inBody = s.pending, 0, false
	case lmProgress.MatchString(msg):
		start("reading")
		s.Pct = atof(lmProgress.FindStringSubmatch(msg)[1])
		if s.Pct >= 100 {
			s.Phase = "writing"
		}
	case lmPromptT.MatchString(msg):
		m := lmPromptT.FindStringSubmatch(msg)
		s.Prompt, s.ReadRate, s.Debug = atoi(m[1]), atof(m[2]), true
	case lmTiming.MatchString(msg):
		m := lmTiming.FindStringSubmatch(msg)
		start("writing")
		s.Decoded, s.GenRate, s.Debug = atoi(m[1]), atof(m[2]), true
		if m[3] != "" {
			s.GenRate = atof(m[3])
		}
	case lmEvalT.MatchString(msg):
		m := lmEvalT.FindStringSubmatch(msg)
		s.Decoded, s.GenRate, s.Debug = atoi(m[1]), atof(m[2]), true
	case lmStop.MatchString(msg): // the slot is released: the request is over (also for non-streamed ones)
		s.Ctx, s.Debug = atoi(lmStop.FindStringSubmatch(msg)[1]), true
		if s.Phase != "" {
			s.Phase = ""
			s.Reqs++
		}
	case msg == "Finished streaming response":
		if s.Phase != "" {
			s.Phase = ""
			s.Reqs++
		}
	}
}

// ---------- LM Studio conversations ----------

// pollLMChat keeps st.lmChat on LM Studio's newest conversation while the backend is LM Studio.
func (st *state) pollLMChat(dir string) {
	path, mod := "", time.Time{}
	for {
		st.mu.Lock()
		lm := st.srv.Kind == "lmstudio"
		st.mu.Unlock()
		if lm {
			if p, t := newestConv(dir); p != "" && (p != path || !t.Equal(mod)) {
				if b, err := os.ReadFile(p); err == nil {
					if s := parseConv(b, p, t); s != nil {
						st.mu.Lock()
						st.lmChat = s
						st.mu.Unlock()
						path, mod = p, t
					}
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// lmConv is the part of an LM Studio conversation file llm-top reads: sizes per kind, never the text itself.
type lmConv struct {
	Name       string `json:"name"`
	TokenCount int    `json:"tokenCount"`
	Model      struct {
		ID string `json:"identifier"`
	} `json:"lastUsedModel"`
	Messages []struct {
		Sel      int `json:"currentlySelected"`
		Versions []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Steps []struct {
				Type  string `json:"type"`
				InCtx *bool  `json:"shouldIncludeInContext"`
				Style struct {
					Type string `json:"type"`
				} `json:"style"`
				Content []struct {
					Text   string `json:"text"`
					Tokens int    `json:"tokensCount"`
				} `json:"content"`
			} `json:"steps"`
		} `json:"versions"`
	} `json:"messages"`
}

// newestConv is the most recently written conversation file under dir (LM Studio keeps folders inside).
func newestConv(dir string) (string, time.Time) {
	best, bt := "", time.Time{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".conversation.json") {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(bt) {
			best, bt = p, fi.ModTime()
		}
		return nil
	})
	return best, bt
}

// parseConv turns a conversation into a session for CONTEXT: user text counts as prompts, thinking steps as
// thinking, the rest of the replies as replies (their exact token counts); the system prompt and chat template are
// what remains of tokenCount. prompts holds the estimated prompt size of each reply.
func parseConv(b []byte, path string, mod time.Time) *session {
	var c lmConv
	if json.Unmarshal(b, &c) != nil || len(c.Messages) == 0 {
		return nil
	}
	s := newSession(path, c.Name)
	if s.Project == "" {
		s.Project = "LM Studio chat"
	}
	s.named, s.Mod, s.lastT, s.model = true, mod, mod, c.Model.ID
	tok := 0.0 // running estimate of the context, in tokens
	var before []float64
	for _, m := range c.Messages {
		if len(m.Versions) == 0 {
			continue
		}
		v := m.Versions[min(max(m.Sel, 0), len(m.Versions)-1)]
		if v.Role == "user" {
			for _, p := range v.Content {
				if p.Type == "text" {
					s.cats[catUser] += len(p.Text)
					tok += float64(len(p.Text)) / 3.6
				}
			}
			continue
		}
		if v.Role != "assistant" {
			continue
		}
		before = append(before, tok)
		s.turns++
		for _, st := range v.Steps {
			if st.Type != "contentBlock" || st.InCtx != nil && !*st.InCtx {
				continue
			}
			cat := catReply
			if st.Style.Type == "thinking" {
				cat = catThink
			}
			for _, p := range st.Content {
				n := p.Tokens
				if n == 0 {
					n = int(float64(len(p.Text)) / 3.6)
				}
				s.cats[cat] += int(float64(n) * 3.6) // cats are characters, estimate() divides by 3.6
				tok += float64(n)
			}
		}
	}
	total := c.TokenCount
	if total <= 0 {
		total = int(tok)
	}
	s.lastUsage.Prompt = total
	k := 1.0 // spread the system prompt and template over the replies
	if tok > 0 {
		k = float64(total) / tok
	}
	for _, x := range before {
		s.prompts = append(s.prompts, int(x*k))
	}
	return s
}
