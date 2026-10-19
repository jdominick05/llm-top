// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// llama-server client: GETs only, never anything that changes the server.
//
// Each 500 ms round asks /health (public). Every 2 s it asks /props (n_ctx, slots, alias, sleep state; never wakes
// a sleeping server) and every 10 s /models (router mode: list entries carry a status). /metrics says whether
// a slot is busy without resetting the idle timer; /slots is only polled while something runs, because each GET
// /slots wakes a sleeping server and resets its --sleep-idle-seconds timer. Without --metrics, /slots is polled
// every round. 401 means an API key is needed (api_key or $LLAMA_API_KEY), 501 that the endpoint is disabled.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

type slotJSON struct {
	ID         int             `json:"id"`
	NCtx       int             `json:"n_ctx"`
	Processing bool            `json:"is_processing"`
	Task       int             `json:"id_task"`
	NPrompt    int             `json:"n_prompt_tokens"`
	NProcessed int             `json:"n_prompt_tokens_processed"`
	NCache     int             `json:"n_prompt_tokens_cache"`
	Next       json.RawMessage `json:"next_token"`
	Params     struct {
		NPredict  int    `json:"n_predict"`
		SpecTypes string `json:"speculative.types"` // "none,ngram-mod"; /props always says "none"
	} `json:"params"`
}

// Qwen Code asks for at most this many tokens when it compacts a session (COMPACT_MAX_OUTPUT_TOKENS in its
// chatCompressionService), less only when the history nearly fills the window (window - input - 1024). Its normal
// turns ask for more. A busy request with that budget is shown as COMPACTING.
const compactBudget = 20000

func isCompaction(nPredict, prompt, nCtx int) bool {
	switch {
	case nPredict == compactBudget:
		return true
	case nPredict > 0 && nPredict < compactBudget && nCtx > 0 && prompt > 0:
		return prompt+nPredict >= nCtx*9/10 // clamped to what is left of the window
	}
	return false
}

type nextTok struct {
	NRemain  int `json:"n_remain"`
	NDecoded int `json:"n_decoded"`
}

// next reads next_token, an array in current servers and an object in older ones.
func (sl slotJSON) next() nextTok {
	var nt nextTok
	if len(sl.Next) > 0 && sl.Next[0] == '[' {
		var arr []nextTok
		if json.Unmarshal(sl.Next, &arr) == nil && len(arr) > 0 {
			nt = arr[0]
		}
	} else {
		_ = json.Unmarshal(sl.Next, &nt)
	}
	return nt
}

type propsJSON struct {
	Gen struct {
		NCtx   int `json:"n_ctx"`
		Params struct {
			NPredict int `json:"n_predict"` // the server's -n; -1 = unlimited
		} `json:"params"`
	} `json:"default_generation_settings"`
	Slots    int    `json:"total_slots"`
	Alias    string `json:"model_alias"`
	Path     string `json:"model_path"`
	Build    string `json:"build_info"`
	Sleeping bool   `json:"is_sleeping"`
}

type ring struct{ v []float64 }

func (r *ring) push(x float64, max int) {
	r.v = append(r.v, x)
	if len(r.v) > max {
		r.v = r.v[len(r.v)-max:]
	}
}

type slotPrev struct {
	task, proc, dec int
	prompt, cache   int
	busy            bool
	t, start, decAt time.Time // start: request first seen busy; decAt: first token written
}

// modelInfo describes the loaded model (CONTEXT shows it when no agent session is).
type modelInfo struct {
	Name, Params, Quant string
	SizeGB              float64
	MaxCtx, Slots       int
	FlashAttn           bool
	Vision, Tools       bool
}

// reqEvent is one finished request, for ACTIVITY when no agent session is shown.
type reqEvent struct {
	T                        time.Time // started
	Dur                      time.Duration
	Prompt, Cached, Out, Ctx int
	ReadRate, GenRate        float64
	Slot                     int
}

const maxDone = 200

func (s *serverState) finished(e reqEvent) {
	s.Done = append(s.Done, e)
	if len(s.Done) > maxDone {
		s.Done = s.Done[len(s.Done)-maxDone:]
	}
}

type serverState struct {
	Polled          bool // at least one round applied: before that nothing is known
	Up, Loading     bool
	Auth            bool // 401 from a monitored endpoint
	Sleeping        bool
	Router          bool
	RouterNote      string // router mode without a loaded model to watch
	Model           string // router: the model id being watched
	SlotsOff        bool   // --no-slots
	HaveSlots       bool
	Slot            slotJSON // the slot shown in CONTEXT
	NSlots, Busy    int
	Decoded, Remain int
	Metrics         map[string]float64
	Props           propsJSON
	ReadRate        ring
	GenRate         ring
	curRead, curGen float64
	prev            map[int]slotPrev
	busySince       time.Time
	Spec            specHist

	Info modelInfo  // LM Studio's model details
	Done []reqEvent // finished requests since llm-top started, oldest first

	Kind     string // "llama.cpp" or "lmstudio"
	Base     string // URL that answered (or was tried last)
	LM       lmState
	LMLoaded int // LM Studio: loaded LLM instances
}

// offline says why there is no live model to show ("" when there is one): every panel says so instead of showing
// zeros or the last picture as if it were current.
func (s *serverState) offline() string {
	switch {
	case !s.Polled:
		return "looking for the server"
	case s.Loading:
		return "model loading"
	case !s.Up:
		return "no server running"
	case s.Auth:
		return "server wants an API key"
	case s.Sleeping:
		return "model asleep (unloaded)"
	case s.Router && s.Model == "", s.Kind == "lmstudio" && s.LMLoaded == 0 && s.LM.Phase == "":
		return "no model loaded"
	}
	return ""
}

// compacting says whether the request shown is a Qwen Code compaction.
func (s *serverState) compacting() bool {
	if !s.Up {
		return false
	}
	if s.Kind == "lmstudio" {
		return s.LM.Phase != "" && isCompaction(s.LM.MaxTokens, max(s.LM.Prompt, 1), s.Props.Gen.NCtx)
	}
	sl := s.Slot
	// a server started with -n 20000 gives every request that budget: only a client-set one counts
	return s.HaveSlots && sl.Processing && sl.Params.NPredict != s.Props.Gen.Params.NPredict &&
		isCompaction(sl.Params.NPredict, sl.NPrompt, max(sl.NCtx, s.Props.Gen.NCtx))
}

// phase is "", "reading" or "writing" for the request shown.
func (s *serverState) phase() string {
	if s.Kind == "lmstudio" {
		return s.LM.Phase
	}
	if !s.HaveSlots || !s.Slot.Processing {
		return ""
	}
	if s.Decoded == 0 {
		return "reading"
	}
	return "writing"
}

// busy says whether the server is working on something, from /slots or else /metrics.
func (s *serverState) busy() bool {
	if !s.Up {
		return false
	}
	if s.Kind == "lmstudio" {
		return s.LM.Phase != ""
	}
	if s.HaveSlots {
		return s.Busy > 0
	}
	return s.Metrics["requests_processing"] > 0
}

// ---------- client ----------

type srvClient struct {
	base, want string // want: router_model from the config
	cl         *http.Client
	bases      []string // candidates, tried in turn while nothing answers
	bi         int
	keys       map[string]string // per kind
	backend    string            // "auto", "llama.cpp" or "lmstudio"
	kind       string            // detected for base, "" = not yet
	lm         *lmLog

	router                 bool
	target, note           string
	lastProps, lastModels  time.Time
	lastMetrics            time.Time
	metricsOK, slotsActive bool
	sleeping               bool            // from the last /props
	work                   float64         // token counters at the last scrape: a change means something ran; -1 = none yet
	seen                   bool            // base has answered at least once
	lmBusy                 bool            // LM Studio: the log showed a request at the last round
	lmModel                string          // LM Studio: the model watched
	more                   func() []string // extra candidate bases (the server process's listening ports), may be nil
	misses                 int             // rounds in a row without an answer
}

// rotateAfter is how many rounds without an answer move on to the next candidate once a server has answered: one
// slow answer (a 2 s timeout while the server is saturated) is not a server that went away.
const rotateAfter = 3

func newClient(bases []string, keys map[string]string, want, backend, lmLogs string) *srvClient {
	backend = normBackend(backend)
	// slotsActive: one look at /slots on startup for the idle picture
	return &srvClient{base: bases[0], bases: bases, keys: keys, want: want, backend: backend, lm: &lmLog{dir: lmLogs},
		cl: &http.Client{Timeout: 2 * time.Second}, slotsActive: true}
}

func (c *srvClient) get(path string) (int, []byte) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil
	}
	if k := c.keys[c.kind]; k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := c.cl.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b
}

// q adds ?model= in router mode.
func (c *srvClient) q(path string) string {
	if c.router && c.target != "" {
		return path + "?model=" + url.QueryEscape(c.target)
	}
	return path
}

// probe is one round of answers; codes are 0 when the endpoint was not asked (or did not answer).
type probe struct {
	T                                 time.Time
	Kind, Base                        string
	LM                                lmState
	LMPick                            lmPick
	Health                            int
	Router                            bool
	Target, Note                      string
	PropsCode, MetricsCode, SlotsCode int
	Props                             propsJSON
	Metrics                           map[string]float64
	Slots                             []slotJSON
	Miss                              bool // no answer, but too soon to call the server gone: keep the last picture
}

func (c *srvClient) round(now time.Time) probe {
	if c.kind == "" {
		c.kind = c.detect()
	}
	var p probe
	if c.kind == "lmstudio" {
		p = c.lmRound(now)
	} else {
		p = c.llamaRound(now)
	}
	p.Kind, p.Base = c.kind, c.base
	if p.Health != 0 {
		c.seen, c.misses = true, 0
		return p
	}
	c.misses++
	if c.more != nil {
		c.addBases(c.more())
	}
	if !c.seen || c.misses >= rotateAfter { // nothing there: try the next candidate and detect again
		c.bi = (c.bi + 1) % len(c.bases)
		c.base, c.kind, c.seen, c.misses = c.bases[c.bi], "", false, 0
		return p
	}
	p.Miss = true
	return p
}

// addBases adds candidates not tried yet.
func (c *srvClient) addBases(bs []string) {
	for _, b := range bs {
		if !slices.Contains(c.bases, b) {
			c.bases = append(c.bases, b)
		}
	}
}

// detect tells LM Studio (GET /api/v1/models lists "models") from llama-server (404 there, /health answers).
func (c *srvClient) detect() string {
	if c.backend != "auto" {
		return c.backend
	}
	code, b := c.get("/api/v1/models")
	if code == http.StatusOK {
		if _, ok := pickLM(b, "", ""); ok {
			return "lmstudio"
		}
	}
	if h, _ := c.get("/health"); h == http.StatusOK || h == http.StatusServiceUnavailable {
		return "llama.cpp"
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return "lmstudio"
	}
	return "llama.cpp"
}

// lmRound: /api/v1/models every 2 s (liveness, loaded instance, context length), the server log every round.
// lmModelsEvery: LM Studio writes every API request into its server log, so the model list is asked rarely; in
// between a TCP connect (not logged) says it is still there. A request starting asks at once (it may have loaded one).
const lmModelsEvery = 10 * time.Second

func (c *srvClient) lmRound(now time.Time) probe {
	p := probe{T: now, Health: http.StatusOK}
	started := c.lm.st.Phase == "" && c.lmBusy
	c.lmBusy = c.lm.st.Phase != ""
	if now.Sub(c.lastProps) < lmModelsEvery && !c.lastProps.IsZero() && !started {
		if !c.alive() {
			p.Health = 0
			c.lastProps = time.Time{}
			c.lm.st.Phase = ""
			return p
		}
	} else {
		c.lastProps = now
		code, b := c.get("/api/v1/models")
		p.PropsCode = code
		if code == 0 {
			p.Health = 0
			c.lastProps = time.Time{}
			c.lm.st.Phase = "" // a request does not survive the server going away
			return p
		}
		if code == http.StatusOK {
			p.LMPick, _ = pickLM(b, c.want, c.lm.st.Model)
			if k := p.LMPick.Key; k != "" && k != c.lmModel { // another model: the old one's numbers don't apply
				if c.lmModel != "" && c.lm.st.Phase == "" {
					c.lm.st.clearStats()
					c.lm.st.Decoded, c.lm.st.GenRate = 0, 0
				}
				c.lmModel = k
			}
		}
	}
	p.LM = c.lm.tail(now)
	if p.LM.Phase != "" && !c.lmBusy { // next round asks for the models again
		c.lmBusy = true
		c.lastProps = time.Time{}
	}
	return p
}

// alive is a bare TCP connect to the base's host.
func (c *srvClient) alive() bool {
	u, err := url.Parse(c.base)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), map[string]string{"https": "443"}[u.Scheme]+map[bool]string{true: "80"}[u.Scheme != "https"])
	}
	conn, err := net.DialTimeout("tcp", host, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (c *srvClient) llamaRound(now time.Time) probe {
	p := probe{T: now}
	p.Health, _ = c.get("/health")
	if p.Health == 0 || p.Health == http.StatusServiceUnavailable {
		c.lastModels, c.lastProps, c.slotsActive = time.Time{}, time.Time{}, true // look again once it is up
		return p
	}
	// a router is asked every round: a model can be evicted or fall asleep any time, and /models never loads or
	// wakes one, while ?model= on an unloaded one could load it
	if c.router || now.Sub(c.lastModels) >= 10*time.Second {
		c.lastModels = now
		code, b := c.get("/models")
		if code == http.StatusOK {
			router, target, note, asleep := routerTarget(b, c.want)
			if target != c.target { // another model: nothing measured on the old one applies; one idle look at it
				c.work, c.slotsActive, c.lastProps = -1, true, time.Time{}
			}
			c.router, c.target, c.note = router, target, note
			if router {
				c.sleeping = asleep
			}
		} else if c.router {
			p.Router, p.Target, p.Note = c.router, c.target, c.note
			return p // no fresh list, no ?model= questions
		}
	}
	p.Router, p.Target, p.Note = c.router, c.target, c.note
	if c.router && c.target == "" {
		return p
	}
	if now.Sub(c.lastProps) >= 2*time.Second {
		c.lastProps = now
		var b []byte
		if p.PropsCode, b = c.get(c.q("/props")); p.PropsCode == http.StatusOK {
			_ = json.Unmarshal(b, &p.Props)
			c.sleeping = p.Props.Sleeping || c.router && c.sleeping // a router's /models said so this round
		}
	}
	busy := c.slotsActive
	if c.metricsOK || now.Sub(c.lastMetrics) >= 5*time.Second { // every round; retry slowly when it's off
		c.lastMetrics = now
		var b []byte
		if p.MetricsCode, b = c.get(c.q("/metrics")); p.MetricsCode == http.StatusOK {
			p.Metrics = parseMetrics(strings.NewReader(string(b)))
			mt := p.Metrics
			work := mt["prompt_tokens_total"] + mt["prompt_tokens_cached_total"] + mt["tokens_predicted_total"]
			// a request that came and went between two scrapes still gets one /slots look
			busy = busy || mt["requests_processing"]+mt["requests_deferred"] > 0 || (c.metricsOK && c.work >= 0 && work != c.work)
			c.work = work
		}
		c.metricsOK = p.MetricsCode == http.StatusOK
	}
	if c.sleeping {
		return p
	}
	if busy || !c.metricsOK {
		var b []byte
		if p.SlotsCode, b = c.get(c.q("/slots")); p.SlotsCode == http.StatusOK {
			if json.Unmarshal(b, &p.Slots) != nil {
				p.SlotsCode = -1
			}
		}
		c.slotsActive = false
		for _, sl := range p.Slots {
			c.slotsActive = c.slotsActive || sl.Processing
		}
	}
	return p
}

// routerTarget detects router mode (/models entries with a status) and picks the model to watch: router_model if
// it is loaded, else the first loaded one. Unloaded models are never asked about, so nothing gets loaded.
// asleep: the target's status is "sleeping".
func routerTarget(b []byte, want string) (router bool, target, note string, asleep bool) {
	var ml struct {
		Data []struct {
			ID     string `json:"id"`
			Status *struct {
				Value string `json:"value"`
			} `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &ml) != nil {
		return false, "", "", false
	}
	first := ""
	for _, m := range ml.Data {
		if m.Status == nil {
			return false, "", "", false
		}
		live := m.Status.Value == "loaded" || m.Status.Value == "sleeping"
		if want != "" && m.ID == want {
			if live {
				return true, want, "", m.Status.Value == "sleeping"
			}
			return true, "", want + " is " + m.Status.Value, false
		}
		if live && first == "" {
			first, asleep = m.ID, m.Status.Value == "sleeping"
		}
	}
	if len(ml.Data) == 0 {
		return false, "", "", false
	}
	if want != "" {
		return true, "", want + " not found", false
	}
	if first == "" {
		return true, "", "no model loaded", false
	}
	return true, first, "", asleep
}

// ---------- state update ----------

func (s *serverState) apply(p probe) {
	now := p.T
	if p.Miss {
		return
	}
	if p.Health == 0 && s.Kind != "" { // nothing answered: detection only guessed the kind
		p.Kind = s.Kind
	}
	if p.Kind != s.Kind {
		*s = serverState{ReadRate: s.ReadRate, GenRate: s.GenRate, Done: s.Done}
	}
	s.Kind, s.Base, s.Polled = p.Kind, p.Base, true
	if p.Kind == "lmstudio" && p.Health != 0 {
		s.applyLM(p)
		return
	}
	s.Up = p.Health != 0 && p.Health != http.StatusServiceUnavailable
	s.Loading = p.Health == http.StatusServiceUnavailable
	if !s.Up {
		s.HaveSlots, s.Busy, s.Auth, s.Sleeping = false, 0, false, false
		s.Metrics, s.prev, s.LM.Phase, s.Slot.Processing = nil, nil, "", false
		s.Spec = specHist{}
		s.ReadRate.push(0, 240)
		s.GenRate.push(0, 240)
		s.curRead, s.curGen = 0, 0
		return
	}
	s.Router, s.RouterNote = p.Router, p.Note
	if s.Router && s.Model != p.Target {
		s.Model, s.Props, s.Metrics, s.HaveSlots, s.prev = p.Target, propsJSON{}, nil, false, nil
		s.Spec = specHist{}
	}
	if p.PropsCode == http.StatusOK {
		s.Props = p.Props
		s.Sleeping = p.Props.Sleeping
	}
	if p.MetricsCode == http.StatusOK {
		s.Metrics = p.Metrics
		s.Spec.add(p.Metrics)
	} else if p.MetricsCode != 0 {
		s.Metrics = nil
	}
	codes := []int{p.PropsCode, p.MetricsCode, p.SlotsCode}
	s.Auth = false
	for _, c := range codes {
		if c == http.StatusUnauthorized || c == http.StatusForbidden {
			s.Auth = true
		}
	}
	if p.SlotsCode != 0 {
		s.SlotsOff = p.SlotsCode == http.StatusNotImplemented
		s.HaveSlots = p.SlotsCode == http.StatusOK && len(p.Slots) > 0
	}
	rr, gr := 0.0, 0.0
	reading, writing := false, false
	if p.SlotsCode == http.StatusOK && len(p.Slots) > 0 {
		if s.prev == nil {
			s.prev = map[int]slotPrev{}
		}
		s.NSlots, s.Busy = len(p.Slots), 0
		pick := -1 // keep the shown slot while it works, else the first busy one, else the shown one
		for i, sl := range p.Slots {
			if sl.Processing && sl.ID == s.Slot.ID && pick < 0 {
				pick = i
			}
		}
		for i, sl := range p.Slots {
			if sl.Processing && pick < 0 {
				pick = i
			}
		}
		for i, sl := range p.Slots {
			if sl.ID == s.Slot.ID && pick < 0 {
				pick = i
			}
		}
		if pick < 0 {
			pick = 0
		}
		for _, sl := range p.Slots {
			nt := sl.next()
			if sl.Processing {
				s.Busy++
				if nt.NDecoded == 0 {
					reading = true
				} else {
					writing = true
				}
			}
			start, decAt := time.Time{}, time.Time{}
			if sl.Processing {
				start = now
			}
			pv, ok := s.prev[sl.ID]
			if ok && sl.Processing && sl.Task == pv.task {
				if dt := now.Sub(pv.t).Seconds(); dt > 0 {
					rr += float64(max(0, sl.NProcessed-pv.proc)) / dt
					gr += float64(max(0, nt.NDecoded-pv.dec)) / dt
				}
				if !pv.start.IsZero() {
					start, decAt = pv.start, pv.decAt
				}
			}
			if sl.Processing && nt.NDecoded > 0 && decAt.IsZero() {
				decAt = now
			}
			if ok && pv.busy && (!sl.Processing || sl.Task != pv.task) { // pv's request is over
				e := reqEvent{T: pv.start, Prompt: pv.prompt, Cached: pv.cache, Out: pv.dec, Ctx: pv.prompt + pv.dec, Slot: sl.ID}
				end := pv.t
				if !sl.Processing && sl.Task == pv.task { // the slot still shows the request's final numbers
					e.Out, e.Ctx, end = nt.NDecoded, sl.NPrompt+nt.NDecoded, now
				}
				e.Dur = end.Sub(pv.start)
				if !pv.decAt.IsZero() {
					if d := pv.decAt.Sub(pv.start).Seconds(); d > 0 {
						e.ReadRate = float64(e.Prompt-e.Cached) / d
					}
					if d := end.Sub(pv.decAt).Seconds(); d > 0 {
						e.GenRate = float64(e.Out) / d
					}
				}
				s.finished(e)
			}
			s.prev[sl.ID] = slotPrev{task: sl.Task, proc: sl.NProcessed, dec: nt.NDecoded, prompt: sl.NPrompt, cache: sl.NCache,
				busy: sl.Processing, t: now, start: start, decAt: decAt}
		}
		sl := p.Slots[pick]
		if sl.Processing && (!s.Slot.Processing || sl.Task != s.Slot.Task || sl.ID != s.Slot.ID) {
			s.busySince = s.prev[sl.ID].start // switching to a slot that was already busy keeps its time
		}
		nt := sl.next()
		s.Slot, s.Decoded, s.Remain = sl, nt.NDecoded, nt.NRemain
	} else if p.SlotsCode == 0 && s.HaveSlots && s.Slot.Processing {
		// not asked this round (idle per /metrics): keep the last picture, stop the spinner
		s.Slot.Processing, s.Busy = false, 0
	}
	s.ReadRate.push(rr, 240)
	s.GenRate.push(gr, 240)
	s.curRead = ema(s.curRead, rr, reading)
	s.curGen = ema(s.curGen, gr, writing)
}

func ema(prev, x float64, active bool) float64 {
	if !active {
		return 0
	}
	if prev == 0 {
		return x
	}
	return prev*0.6 + x*0.4
}

func parseMetrics(r io.Reader) map[string]float64 {
	m := map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "#") {
			continue
		}
		if i := strings.Index(l, "{"); i >= 0 { // only the per-position acceptance, kept as "name.N"
			pos, ok := strings.CutPrefix(l[i:], `{position="`)
			n, v, ok2 := strings.Cut(pos, `"}`)
			if f := strings.Fields(v); ok && ok2 && len(f) == 1 && strings.TrimPrefix(l[:i], "llamacpp:") == specPosKey {
				if x, err := strconv.ParseFloat(f[0], 64); err == nil {
					m[specPosKey+"."+n] = x
				}
			}
			continue
		}
		f := strings.Fields(l)
		if len(f) == 2 {
			if v, err := strconv.ParseFloat(f[1], 64); err == nil {
				m[strings.TrimPrefix(f[0], "llamacpp:")] = v
			}
		}
	}
	return m
}

func (st *state) pollServer(c *srvClient) {
	for {
		p := c.round(time.Now())
		st.mu.Lock()
		st.srv.apply(p)
		st.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *serverState) applyLM(p probe) {
	now := p.T
	s.Up, s.Loading, s.Router, s.Sleeping = true, false, false, false
	if p.PropsCode != 0 {
		s.Auth = p.PropsCode == http.StatusUnauthorized || p.PropsCode == http.StatusForbidden
		if p.PropsCode == http.StatusOK {
			s.Model, s.LMLoaded, s.Props.Gen.NCtx, s.Info = p.LMPick.Key, p.LMPick.Loaded, p.LMPick.Ctx, p.LMPick.Info
		}
	}
	lg := p.LM
	if lg.Reqs > s.LM.Reqs { // a request finished (maybe started and ended within one round)
		e := reqEvent{T: lg.Started, Prompt: lg.Prompt, Out: lg.Decoded, Ctx: lg.Ctx, ReadRate: lg.ReadRate, GenRate: lg.GenRate}
		if !lg.Started.IsZero() {
			e.Dur = now.Sub(lg.Started)
		}
		s.finished(e)
	}
	if lg.Phase != "" && s.LM.Phase == "" {
		s.busySince = now
	}
	s.LM = lg
	s.HaveSlots, s.NSlots, s.Busy = true, 1, 0
	if lg.Phase != "" {
		s.Busy = 1
	}
	s.Slot = slotJSON{NCtx: s.Props.Gen.NCtx, Processing: lg.Phase != ""}
	s.Decoded = lg.Decoded
	switch {
	case lg.Phase == "" && lg.Ctx > 0:
		s.Slot.NPrompt = max(0, lg.Ctx-lg.Decoded)
	case lg.Phase == "":
		s.Slot.NPrompt, s.Decoded = lg.Prompt, 0
	}
	s.curRead, s.curGen = 0, 0
	if lg.Phase == "writing" {
		s.curGen = lg.GenRate
	}
	s.ReadRate.push(0, 240)
	s.GenRate.push(s.curGen, 240)
}

// ---------- speculative decoding ----------

const specPosKey = "spec_decode_num_accepted_tokens_per_pos_total"

// specWindow is how many polls that drafted count as "recent": ~60 s while drafting all the time.
const specWindow = 120

type specSample struct{ acc, tok, drafts float64 }

// specHist keeps the spec_decode counters' deltas between polls that drafted.
type specHist struct {
	prev    [3]float64 // accepted, draft tokens, drafts at the last scrape
	seen    bool
	Samples []specSample // oldest first, at most specWindow
}

func specCounters(mt map[string]float64) [3]float64 {
	return [3]float64{mt["spec_decode_num_accepted_tokens_total"], mt["spec_decode_num_draft_tokens_total"], mt["spec_decode_num_drafts_total"]}
}

func (h *specHist) add(mt map[string]float64) {
	if _, ok := mt["spec_decode_num_draft_tokens_total"]; !ok {
		*h = specHist{}
		return
	}
	c := specCounters(mt)
	if c[0] < h.prev[0] || c[1] < h.prev[1] || c[2] < h.prev[2] { // the server restarted: older samples are another run
		h.Samples, h.seen = nil, false
	}
	if h.seen {
		// counters print with 6 significant digits (%g): deltas of large ones are coarse, so keep them consistent
		d := specSample{acc: max(0, c[0]-h.prev[0]), tok: c[1] - h.prev[1], drafts: max(0, c[2]-h.prev[2])}
		if d.tok > 0 {
			d.acc = min(d.acc, d.tok)
			h.Samples = append(h.Samples, d)
			if len(h.Samples) > specWindow {
				h.Samples = h.Samples[len(h.Samples)-specWindow:]
			}
		}
	}
	h.prev, h.seen = c, true
}

// recent is the acceptance rate and accepted tokens per draft over the window (ok false without samples).
func (h *specHist) recent() (rate, perDraft float64, ok bool) {
	var a, t, d float64
	for _, x := range h.Samples {
		a, t, d = a+x.acc, t+x.tok, d+x.drafts
	}
	if t <= 0 {
		return 0, 0, false
	}
	if d > 0 {
		perDraft = a / d
	}
	return a / t, perDraft, true
}

// rates is the acceptance rate of each sample, for the sparkline.
func (h *specHist) rates() []float64 {
	v := make([]float64, len(h.Samples))
	for i, x := range h.Samples {
		v[i] = x.acc / x.tok
	}
	return v
}

// specPos is the share of drafts whose token at each position was accepted, since server start.
func specPos(mt map[string]float64) []float64 {
	d := mt["spec_decode_num_drafts_total"]
	if d <= 0 {
		return nil
	}
	var v []float64
	for i := 0; ; i++ {
		x, ok := mt[specPosKey+"."+strconv.Itoa(i)]
		if !ok {
			break
		}
		v = append(v, min(1, x/d))
	}
	for len(v) > 0 && v[len(v)-1] == 0 {
		v = v[:len(v)-1]
	}
	return v
}

// drafters names the speculative decoding from a slot's speculative.types: "none,ngram-mod" is "ngram-mod",
// "draft-dflash,ngram-mod" is "dflash+ngram". known is false for an empty string; "" with known means none.
func drafters(types string) (name string, known bool) {
	types = strings.TrimSpace(types)
	if types == "" {
		return "", false
	}
	var ts []string
	for _, t := range strings.Split(types, ",") {
		if t = strings.TrimSpace(t); t != "" && t != "none" {
			ts = append(ts, strings.TrimPrefix(t, "draft-"))
		}
	}
	if len(ts) == 1 {
		return ts[0], true
	}
	var short []string
	for _, t := range ts {
		t, _, _ = strings.Cut(t, "-")
		if !slices.Contains(short, t) {
			short = append(short, t)
		}
	}
	return strings.Join(short, "+"), true
}
