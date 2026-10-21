// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

// Machine counters: VRAM, RAM, and the inference process's fallbacks (VRAM spilled to system RAM, paging, CPU
// doing the work) for the bottleneck line.
//
// Windows: one PowerShell call every 5 s (CIM classes, locale independent; VRAM total from the display adapter's
// registry key; per-process GPU memory and engine use from the GPU performance counters, per card: PnP gives each
// display device's LUID, which names its counters, and its registry key). With several GPUs the numbers are those of
// the cards holding the model. Linux: /proc and sysfs
// (amdgpu VRAM, GTT and busy %), else nvidia-smi. Everything else shows no counters. The engine process is the first
// configured name that runs (the one using most VRAM if several do), else whatever holds more than 1 GB of VRAM.
// The parsers are plain functions so tests run on any OS.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type sysState struct {
	OK              bool
	VramGB, VramTot float64 // VramTot 0 = unknown
	RamFree, RamTot float64
	Hold            string

	// the inference process; Pid 0 = not found
	Pid      int
	Proc     string
	Guess    bool    // picked by VRAM use, not by name
	SrvWS    float64 // resident memory, GB
	CPUSec   float64 // total CPU time, s
	ProcVram float64 // GB, -1 = unknown
	Spill    float64 // GB of GPU memory in system RAM (Windows: shared usage; Linux: amdgpu GTT), -1 = unknown
	ProcSwap float64 // GB of the process in swap (Linux), -1 = unknown
	GPUUtil  float64 // %, -1 = unknown
	SwapGB   float64 // page file / swap in use, GB
	PageIn   float64 // pages read back from disk per second (Windows) or the cumulative count (Linux, see pollSys)

	Cores     float64  // CPU cores the process used since the last probe, -1 = unknown
	Listen    []string // addresses the process listens on ("127.0.0.1:8091"): finds a server on another port
	pageInRaw float64  // Linux: pswpin at this probe

	NPU npuState // Windows only; NPU.Name "" = no NPU

	GPUs []gpuCard // Windows: every GPU with memory of its own, biggest first; nil = no per-card counters
}

// gpuCard is one GPU. A model split across cards (llama.cpp -dev ROCm0,ROCm1, --tensor-split) has a part on each.
type gpuCard struct {
	Name         string
	Used, Tot    float64 // dedicated memory, GB
	Proc, Shared float64 // the inference process on this card: dedicated and shared memory, GB
	Util         float64 // %, the process's busiest engine on this card, -1 = unknown
	Model        bool    // the process holds at least modelCardGB here
}

// modelCardGB of the process's VRAM make a card one the model runs on; the runtime keeps a few MB (or a few
// hundred, on the iGPU) on every other GPU.
const modelCardGB = 1

const gib = 1 << 30

var procName = regexp.MustCompile(`^[A-Za-z0-9._ -]+$`)

func probeSys(names []string) sysState {
	switch runtime.GOOS {
	case "windows":
		return probeWindows(names)
	case "linux":
		return probeLinux("/proc", "/sys", names, nvidiaSmi)
	}
	return sysState{}
}

// ---------- windows ----------

const psProbe = `$ErrorActionPreference='SilentlyContinue'
$ic=[Globalization.CultureInfo]::InvariantCulture
function o($k,$v) { if ($null -ne $v) { [Console]::Out.WriteLine($k+' '+[string]::Format($ic,'{0}',[double]$v)) } }
o vram_used (Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory | Measure-Object DedicatedUsage -Maximum).Maximum
o vram_total (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}\0*' -Name 'HardwareInformation.qwMemorySize' | Measure-Object 'HardwareInformation.qwMemorySize' -Maximum).Maximum
$os=Get-CimInstance Win32_OperatingSystem
o ram_free_kb $os.FreePhysicalMemory
o ram_total_kb $os.TotalVisibleMemorySize
o swap_mb (Get-CimInstance Win32_PageFileUsage | Measure-Object CurrentUsage -Sum).Sum
o pages_in (Get-CimInstance Win32_PerfFormattedData_PerfOS_Memory).PagesInputPersec
$gm=@{}; $gp=@{}
foreach ($r in Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessMemory) {
  if ($r.Name -match '^pid_(\d+)_(luid_0x[0-9a-f]+_0x[0-9a-f]+)_') { $k=[int]$Matches[1]; $e=$gm[$k]; $gp[[string]$k+' '+$Matches[2]]=@([double]$r.DedicatedUsage,[double]$r.SharedUsage)
    if (-not $e -or $r.DedicatedUsage -gt $e[0]) { $gm[$k]=@([double]$r.DedicatedUsage,[double]$r.SharedUsage) } }
}
$lm=%s
function eng($p) { [string]$p.Path -match '[\\/]\.lmstudio[\\/]' }
$best=$null; $bd=-1; $guess=0
foreach ($n in @(%s)) {
  foreach ($p in Get-Process -Name $n) {
    if ($p.ProcessName -eq 'llama-server' -and ((eng $p) -ne ($lm -eq 1))) { continue }
    $d=0; if ($gm[$p.Id]) { $d=$gm[$p.Id][0] }; if ($d -gt $bd) { $best=$p; $bd=$d } }
  if ($best) { break }
}
if (-not $best) {
  $top=$gm.GetEnumerator() | Sort-Object { $_.Value[0] } -Descending | Select-Object -First 1
  if ($top -and $top.Value[0] -gt 1GB) { $best=Get-Process -Id $top.Key; $guess=1 }
}
if ($best) {
  o pid $best.Id
  o guess $guess
  o ws $best.WorkingSet64
  o cpu_s $best.CPU
  $g=$gm[$best.Id]; if ($g) { o proc_vram $g[0]; o proc_shared $g[1] }
  $pe=@(Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUEngine -Filter ("Name LIKE 'pid[_]"+$best.Id+"[_]%%'"))
  o gpu_util ($pe | Measure-Object UtilizationPercentage -Maximum).Maximum
  $nm=$best.ProcessName; if (eng $best) { $nm='LM Studio' }
  foreach ($c in Get-NetTCPConnection -State Listen -OwningProcess $best.Id) { [Console]::Out.WriteLine('listen '+$c.LocalAddress+' '+$c.LocalPort) }
  [Console]::Out.WriteLine('name '+$nm)
}
$am=@(Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory)
foreach ($d in Get-CimInstance Win32_PnPEntity -Filter "PNPClass='Display'") {
  $dp=@{}; foreach ($x in (Invoke-CimMethod -InputObject $d -MethodName GetDeviceProperties -Arguments @{devicePropertyKeys=@('DEVPKEY_Device_Driver','{60b193cb-5276-4d0f-96fc-f173abad3ec6} 2')}).deviceProperties) { $dp[$x.KeyName]=$x.Data }
  $l=$dp['{60b193cb-5276-4d0f-96fc-f173abad3ec6} 2']; $t=(Get-ItemProperty ('HKLM:\SYSTEM\CurrentControlSet\Control\Class\'+$dp['DEVPKEY_Device_Driver']) -Name 'HardwareInformation.qwMemorySize').'HardwareInformation.qwMemorySize'
  if ($null -eq $l -or -not $t) { continue }
  $u=[uint64]$l; $id='luid_0x{0:X8}_0x{1:X8}' -f [uint32]($u -shr 32),[uint32]($u -band 4294967295)
  $pm=@(0,0); $pu=$null
  if ($best) { if ($gp[[string]$best.Id+' '+$id]) { $pm=$gp[[string]$best.Id+' '+$id] }; $pu=($pe | Where-Object { $_.Name -like "*_$($id)_*" } | Measure-Object UtilizationPercentage -Maximum).Maximum }
  $au=($am | Where-Object { $_.Name -like "$($id)_*" } | Measure-Object DedicatedUsage -Sum).Sum
  [Console]::Out.WriteLine('gpu '+[string]::Format($ic,'{0} {1} {2} {3} {4}',[double]$t,[double]$au,$pm[0],$pm[1],$(if ($null -eq $pu) { -1 } else { [double]$pu }))+' '+$d.Name)
}
$npu=Get-CimInstance Win32_PnPEntity -Filter "PNPClass='ComputeAccelerator'" | Select-Object -First 1
if ($npu) {
  [Console]::Out.WriteLine('npu_name '+$npu.Name)
  $eng=@(Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUEngine)
  $ty=@{}
  foreach ($e in $eng) { if ($e.Name -match '_luid_(0x\w+_0x\w+)_phys_\d+_eng_\d+_engtype_(.*)$') { if (-not $ty[$Matches[1]]) { $ty[$Matches[1]]=@{} }; $ty[$Matches[1]][$Matches[2]]=1 } }
  $nl=$ty.Keys | Where-Object { $k=@($ty[$_].Keys); $k.Count -eq 1 -and $k[0] -eq 'Compute' } | Select-Object -First 1
  if ($nl) {
    $u=0; $np=@{}
    foreach ($e in $eng) { if ($e.Name -like "*_luid_$($nl)_*") { $u+=$e.UtilizationPercentage; if ($e.Name -match '^pid_(\d+)_') { $np[[int]$Matches[1]]+=$e.UtilizationPercentage } } }
    o npu_util ([Math]::Min(100,$u))
    $m=@(Get-CimInstance Win32_PerfFormattedData_GPUPerformanceCounters_GPUAdapterMemory | Where-Object { $_.Name -like "luid_$($nl)_*" })
    o npu_mem ((($m | Measure-Object SharedUsage -Sum).Sum) + (($m | Measure-Object DedicatedUsage -Sum).Sum))
    foreach ($k in $np.Keys) { if ($k -gt 4) { $p=Get-Process -Id $k; if ($p) { [Console]::Out.WriteLine('npu_proc '+$k+' '+[string]::Format($ic,'{0}',[double]$np[$k])+' '+$p.ProcessName) } } }
  }
}`

// probeTimeout bounds one probe (PowerShell takes about 2 s); a hung WMI or driver then shows no machine data
// instead of freezing the last numbers on screen.
const probeTimeout = 10 * time.Second

// lmEngine in a name list stands for LM Studio's engine: LM Studio 0.4 runs a llama-server from
// ~/.lmstudio/extensions/backends, which holds the model. It is never taken for the user's own llama-server.
const lmEngine = "<lmstudio-engine>"

func psScript(quoted []string, lm bool) string {
	return fmt.Sprintf(psProbe, map[bool]string{true: "1", false: "0"}[lm], strings.Join(quoted, ","))
}

func probeWindows(names []string) sysState {
	var q []string
	lm := false
	for _, n := range names {
		if n == lmEngine {
			lm, n = true, "llama-server"
		}
		n = strings.TrimSuffix(n, ".exe")
		if procName.MatchString(n) {
			q = append(q, "'"+n+"'")
		}
	}
	if len(q) == 0 {
		q = []string{"'llama-server'"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", psScript(q, lm)).Output()
	if err != nil {
		return sysState{}
	}
	ss := parseWinProbe(string(out))
	if ss.NPU.Name != "" {
		ss.NPU.Smi = probeXrtSmi()
	}
	return ss
}

// parseWinProbe reads the "key value" lines of psProbe. A machine without a GPU just reports zeros.
func parseWinProbe(out string) sysState {
	v := map[string]float64{}
	name := ""
	var listen []string
	var cards []gpuCard
	np := npuState{Util: -1, MemGB: -1}
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		k, val, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		if k == "name" {
			name = strings.TrimSpace(val)
			continue
		}
		if k == "npu_name" {
			np.Name = strings.TrimSpace(val)
			continue
		}
		if k == "npu_proc" { // "<pid> <util %> <name>"; a malformed line is skipped, it never blanks the panel
			f := strings.SplitN(strings.TrimSpace(val), " ", 3)
			if len(f) == 3 {
				pid, e1 := strconv.Atoi(f[0])
				u, e2 := strconv.ParseFloat(f[1], 64)
				if e1 == nil && e2 == nil {
					np.Procs = append(np.Procs, npuProc{pid, f[2], u})
				}
			}
			continue
		}
		if k == "gpu" { // "<total> <used> <process dedicated> <process shared> <process util> <name>", bytes and %; a malformed line is skipped
			f := strings.SplitN(strings.TrimSpace(val), " ", 6)
			var x [5]float64
			bad := len(f) < 5
			for i := 0; i < len(x) && !bad; i++ {
				var err error
				x[i], err = strconv.ParseFloat(f[i], 64)
				bad = err != nil
			}
			if bad {
				continue
			}
			c := gpuCard{Used: x[1] / gib, Tot: x[0] / gib, Proc: x[2] / gib, Shared: x[3] / gib, Util: x[4]}
			if len(f) == 6 {
				c.Name = strings.TrimSpace(f[5])
			}
			cards = append(cards, c)
			continue
		}
		if k == "listen" {
			if a, p, ok := strings.Cut(strings.TrimSpace(val), " "); ok {
				listen = append(listen, net.JoinHostPort(a, p))
			}
			continue
		}
		x, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return sysState{}
		}
		v[k] = x
	}
	if v["ram_total_kb"] <= 0 {
		return sysState{}
	}
	ss := sysState{OK: true, VramGB: v["vram_used"] / gib, VramTot: v["vram_total"] / gib,
		RamFree: v["ram_free_kb"] / (1 << 20), RamTot: v["ram_total_kb"] / (1 << 20),
		SwapGB: v["swap_mb"] / 1024, PageIn: v["pages_in"],
		ProcVram: -1, Spill: -1, ProcSwap: -1, GPUUtil: -1, Cores: -1}
	if np.Name != "" {
		if u, ok := v["npu_util"]; ok {
			np.Util = u
		}
		if m, ok := v["npu_mem"]; ok {
			np.MemGB = m / gib
		}
		sort.SliceStable(np.Procs, func(i, j int) bool { return np.Procs[i].Util > np.Procs[j].Util })
		ss.NPU = np
	}
	if p, ok := v["pid"]; ok && p > 0 {
		ss.Pid, ss.Proc, ss.Guess, ss.Listen = int(p), name, v["guess"] == 1, listen
		ss.SrvWS, ss.CPUSec = v["ws"]/gib, v["cpu_s"]
		if u, ok := v["gpu_util"]; ok { // no GPU engine counters (no GPU, or a CPU-only server): unknown, not 0%
			ss.GPUUtil = u
		}
		if _, ok := v["proc_vram"]; ok {
			ss.ProcVram, ss.Spill = v["proc_vram"]/gib, v["proc_shared"]/gib
		}
	}
	ss.useCards(cards)
	return ss
}

// useCards replaces the single-GPU numbers (the biggest VRAM total and the busiest adapter, which need not be the
// same card) with those of the cards the process holds the model on: VRAM, the process's VRAM and spill summed,
// GPU load summed (layer split runs the cards in turn). Without such a card (no server, or it sleeps unloaded)
// VRAM is the biggest card's.
func (ss *sysState) useCards(cards []gpuCard) {
	if len(cards) == 0 {
		return
	}
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].Tot > cards[j].Tot })
	var used, tot, proc, shared, util float64
	n := 0
	for i := range cards {
		c := &cards[i]
		c.Model = ss.Pid > 0 && c.Proc >= modelCardGB
		if c.Model {
			n++
			used, tot, proc, shared = used+c.Used, tot+c.Tot, proc+c.Proc, shared+c.Shared
			util += max(0, c.Util)
		}
	}
	ss.GPUs = cards
	if n == 0 {
		ss.VramGB, ss.VramTot = cards[0].Used, cards[0].Tot
		return
	}
	ss.VramGB, ss.VramTot, ss.ProcVram, ss.Spill = used, tot, proc, shared
	if ss.GPUUtil >= 0 {
		ss.GPUUtil = min(100, util)
	}
}

// modelCards counts the cards the model runs on.
func (ss sysState) modelCards() int {
	n := 0
	for _, c := range ss.GPUs {
		if c.Model {
			n++
		}
	}
	return n
}

// ---------- linux ----------

func probeLinux(procRoot, sysRoot string, names []string, smi func() string) sysState {
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return sysState{}
	}
	mi := parseKV(string(b))
	if mi["MemTotal"] <= 0 {
		return sysState{}
	}
	ss := sysState{OK: true, RamTot: mi["MemTotal"] / (1 << 20), RamFree: mi["MemAvailable"] / (1 << 20),
		SwapGB: (mi["SwapTotal"] - mi["SwapFree"]) / (1 << 20), ProcVram: -1, Spill: -1, ProcSwap: -1, GPUUtil: -1, Cores: -1}
	if vs, err := os.ReadFile(filepath.Join(procRoot, "vmstat")); err == nil {
		for _, l := range strings.Split(string(vs), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "pswpin" {
				ss.PageIn, _ = strconv.ParseFloat(f[1], 64)
			}
		}
	}
	if pid, name := findProc(procRoot, names); pid > 0 {
		d := filepath.Join(procRoot, strconv.Itoa(pid))
		ss.Pid, ss.Proc, ss.Listen = pid, name, listenAddrs(procRoot, pid)
		if st, err := os.ReadFile(filepath.Join(d, "status")); err == nil {
			kv := parseKV(string(st))
			ss.SrvWS = kv["VmRSS"] / (1 << 20)
			if sw, ok := kv["VmSwap"]; ok {
				ss.ProcSwap = sw / (1 << 20)
			}
		}
		if st, err := os.ReadFile(filepath.Join(d, "stat")); err == nil {
			ss.CPUSec = statCPU(string(st))
		}
	}
	// both kinds are asked: a Ryzen iGPU (amdgpu, 512 MB) must not hide an NVIDIA card. The bigger one wins.
	if g, ok := amdGPU(sysRoot); ok {
		ss.VramGB, ss.VramTot, ss.GPUUtil = g.used/gib, g.tot/gib, g.busy
		if g.gtt >= 0 {
			ss.Spill = g.gtt / gib
		}
	}
	if smi != nil {
		if used, tot, util, ok := parseNvidiaSmi(smi()); ok && tot/1024 > ss.VramTot {
			ss.VramGB, ss.VramTot, ss.GPUUtil, ss.Spill = used/1024, tot/1024, util, -1
		}
	}
	return ss
}

// listenAddrs are the TCP sockets pid listens on: /proc/net/tcp{,6} rows in state 0A (LISTEN) whose inode is one of
// the process's fds.
func listenAddrs(procRoot string, pid int) []string {
	inodes := map[string]bool{}
	fds, _ := filepath.Glob(filepath.Join(procRoot, strconv.Itoa(pid), "fd", "*"))
	for _, fd := range fds {
		if l, err := os.Readlink(fd); err == nil && strings.HasPrefix(l, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]")] = true
		}
	}
	var out []string
	for _, f := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(procRoot, "net", f))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n")[1:] {
			x := strings.Fields(l)
			if len(x) < 10 || x[3] != "0A" || !inodes[x[9]] {
				continue
			}
			if a := hexAddr(x[1]); a != "" {
				out = append(out, a)
			}
		}
	}
	return out
}

// hexAddr turns /proc/net/tcp's "0100007F:1F9B" (little-endian IPv4, or 32 hex digits of IPv6) into "127.0.0.1:8091".
func hexAddr(s string) string {
	h, p, ok := strings.Cut(s, ":")
	port, err := strconv.ParseUint(p, 16, 16)
	if !ok || err != nil {
		return ""
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 && len(b) != 16 {
		return ""
	}
	for i := 0; i+4 <= len(b); i += 4 { // each 32-bit word is little-endian
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.JoinHostPort(net.IP(b).String(), strconv.Itoa(int(port)))
}

// parseKV reads "Key:   123 kB" lines (/proc/meminfo, /proc/<pid>/status) into kB values.
func parseKV(s string) map[string]float64 {
	m := map[string]float64{}
	for _, l := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		if f := strings.Fields(v); len(f) > 0 {
			if x, err := strconv.ParseFloat(f[0], 64); err == nil {
				m[strings.TrimSpace(k)] = x
			}
		}
	}
	return m
}

// findProc returns the first configured name that runs (comm is cut at 15 bytes); several matches: most resident.
func findProc(procRoot string, names []string) (int, string) {
	dirs, _ := filepath.Glob(filepath.Join(procRoot, "[0-9]*"))
	sort.Strings(dirs)
	lm := len(names) > 0 && names[0] == lmEngine
	for _, n := range names {
		if n == lmEngine {
			n = "llama-server"
		}
		want := n
		if len(want) > 15 {
			want = want[:15]
		}
		best, bestRSS := 0, -1.0
		for _, d := range dirs {
			c, err := os.ReadFile(filepath.Join(d, "comm"))
			if err != nil || strings.TrimSpace(string(c)) != want {
				continue
			}
			exe, _ := os.Readlink(filepath.Join(d, "exe"))
			if want == "llama-server" && strings.Contains(filepath.ToSlash(exe), "/.lmstudio/") != lm {
				continue // LM Studio's engine is not the user's llama-server, and the other way round
			}
			st, _ := os.ReadFile(filepath.Join(d, "status"))
			if rss := parseKV(string(st))["VmRSS"]; rss > bestRSS {
				best, _ = strconv.Atoi(filepath.Base(d))
				bestRSS = rss
			}
		}
		if best > 0 {
			if lm && n == "llama-server" {
				n = "LM Studio"
			}
			return best, n
		}
	}
	return 0, ""
}

// procRSS returns the resident set (kB) of the process found for name.
func procRSS(procRoot, name string) float64 {
	pid, _ := findProc(procRoot, []string{name})
	if pid == 0 {
		return 0
	}
	st, _ := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	return parseKV(string(st))["VmRSS"]
}

// statCPU returns utime + stime in seconds from /proc/<pid>/stat (USER_HZ is 100 on Linux).
func statCPU(s string) float64 {
	i := strings.LastIndexByte(s, ')') // comm may contain spaces
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 13 { // fields 3.. after comm; utime and stime are 14 and 15
		return 0
	}
	u, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (u + st) / 100
}

type amdCard struct{ used, tot, gtt, busy float64 }

// amdGPU picks the card with the most VRAM from the amdgpu sysfs files (bytes, busy in %).
func amdGPU(sysRoot string) (amdCard, bool) {
	var best amdCard
	ok := false
	files, _ := filepath.Glob(filepath.Join(sysRoot, "class", "drm", "card*", "device", "mem_info_vram_total"))
	for _, f := range files {
		d := filepath.Dir(f)
		t := readNum(f)
		u := readNum(filepath.Join(d, "mem_info_vram_used"))
		if t > best.tot && u >= 0 {
			best, ok = amdCard{used: u, tot: t, gtt: readNum(filepath.Join(d, "mem_info_gtt_used")), busy: readNum(filepath.Join(d, "gpu_busy_percent"))}, true
		}
	}
	return best, ok
}

func readNum(p string) float64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return -1
	}
	x, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return -1
	}
	return x
}

func nvidiaSmi() string {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.used,memory.total,utilization.gpu", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// parseNvidiaSmi reads "used, total[, util]" lines (MiB, %) and returns the GPU with the most memory.
func parseNvidiaSmi(out string) (used, tot, util float64, ok bool) {
	util = -1
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 2 {
			continue
		}
		uf, e1 := strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
		tf, e2 := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		if e1 != nil || e2 != nil || tf <= tot {
			continue
		}
		used, tot, util, ok = uf, tf, -1, true
		if len(f) > 2 {
			if x, err := strconv.ParseFloat(strings.TrimSpace(f[2]), 64); err == nil {
				util = x
			}
		}
	}
	return used, tot, util, ok
}

// ---------- poller ----------

// procNames is what the probe looks for, by server kind.
func procNames(kind, configured string) []string {
	switch kind {
	case "lmstudio":
		return []string{lmEngine, "LM Studio", "lm-studio", "llmster"}
	case "llama.cpp":
		return []string{configured}
	}
	return []string{configured, "LM Studio", "lm-studio", "llmster"}
}

func (st *state) pollSys(configured, hold string) {
	var prev sysState
	var prevT time.Time
	for {
		st.mu.Lock()
		kind := st.srv.Kind
		st.mu.Unlock()
		ss := probeSys(procNames(kind, configured))
		now := time.Now()
		ss = rates(ss, prev, now.Sub(prevT).Seconds(), runtime.GOOS == "linux")
		prev, prevT = ss, now
		ss.Hold = readHold(hold)
		st.mu.Lock()
		st.sys, st.sysTried = ss, true
		st.mu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

// rates turns counters into rates: CPU seconds into cores, and on Linux cumulative pswpin into pages per second.
// prev keeps the raw counters in its fields, so the returned state carries them for the next call.
func rates(ss, prev sysState, dt float64, cumulative bool) sysState {
	ss.Cores = -1
	ss.NPU.CmdRate = npuRate(ss.NPU.Smi, prev.NPU.Smi, dt)
	if ss.Pid != 0 && ss.Pid == prev.Pid && dt > 0 && ss.CPUSec >= prev.CPUSec {
		ss.Cores = (ss.CPUSec - prev.CPUSec) / dt
	}
	if cumulative {
		raw := ss.PageIn
		ss.PageIn = 0
		if prev.OK && dt > 0 && raw >= prev.pageInRaw {
			ss.PageIn = (raw - prev.pageInRaw) / dt
		}
		ss.pageInRaw = raw
	}
	return ss
}

// readHold returns the hold file's first line ("GPU job" if empty), or "" when there is no hold.
func readHold(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if h := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]); h != "" {
		return h
	}
	return "GPU job"
}
