// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 jdominick05

package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// numbers from a 24 GB GPU running llama-server at 229k context
const winProbeOut = "vram_used 25508036608\r\nvram_total 25753026560\r\nram_free_kb 15625000\r\nram_total_kb 32505856\r\n" +
	"swap_mb 1007\r\npages_in 0\r\npid 18184\r\nguess 0\r\nws 1213005824\r\ncpu_s 6055.78\r\n" +
	"proc_vram 23060332544\r\nproc_shared 362921984\r\ngpu_util 93\r\nname llama-server\r\n"

func TestParseWinProbe(t *testing.T) {
	ss := parseWinProbe(winProbeOut)
	if !ss.OK || !near(ss.VramGB, 23.756) || !near(ss.VramTot, 23.984) || !near(ss.RamTot, 31) || !near(ss.SrvWS, 1.13) {
		t.Errorf("%+v", ss)
	}
	if ss.Pid != 18184 || ss.Proc != "llama-server" || ss.Guess || !near(ss.ProcVram, 21.48) || !near(ss.Spill, 0.338) ||
		ss.GPUUtil != 93 || !near(ss.SwapGB, 0.983) || ss.ProcSwap != -1 || ss.Cores != -1 {
		t.Errorf("process: %+v", ss)
	}
	// a CI runner: no GPU, no server process
	ss = parseWinProbe("vram_used 0\nvram_total 0\nram_free_kb 1000000\nram_total_kb 8000000\nswap_mb 0\npages_in 0\n")
	if !ss.OK || ss.VramTot != 0 || ss.Pid != 0 || ss.Spill != -1 || ss.GPUUtil != -1 {
		t.Errorf("no gpu: %+v", ss)
	}
	// a CPU-only server on a machine without GPU counters: o() skips the null, so GPU load is unknown, not 0%
	ss = parseWinProbe("ram_total_kb 8000000\npid 9\nguess 0\nws 100\ncpu_s 5\nname llama-server\n")
	if ss.Pid != 9 || ss.GPUUtil != -1 {
		t.Errorf("cpu-only: %+v", ss)
	}
	ss.Cores = 6
	if v, _ := bottleneck(true, ss, 0, 16); !strings.Contains(v, "GPU load unknown") {
		t.Errorf("cpu-only verdict %q", v)
	}
	// LM Studio found by name, process name with a space
	ss = parseWinProbe("ram_total_kb 8000000\npid 77\nguess 1\nname LM Studio\n")
	if ss.Proc != "LM Studio" || !ss.Guess || ss.ProcVram != -1 {
		t.Errorf("name: %+v", ss)
	}
	for _, bad := range []string{"", "ram_total_kb 0", "ram_total_kb 1,5", "vram_used x\nram_total_kb 5"} {
		if parseWinProbe(bad).OK {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeLinux builds a /proc and /sys tree.
func fakeLinux(t *testing.T, amd bool) (proc, sys string) {
	d := t.TempDir()
	proc, sys = filepath.Join(d, "proc"), filepath.Join(d, "sys")
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:       65536000 kB\nMemFree:         1000000 kB\nMemAvailable:   32768000 kB\n"+
		"SwapTotal:       8388608 kB\nSwapFree:        7340032 kB\n")
	write(t, filepath.Join(proc, "vmstat"), "nr_free_pages 1\npswpin 1200\npswpout 5000\n")
	write(t, filepath.Join(proc, "1", "comm"), "systemd\n")
	write(t, filepath.Join(proc, "1", "status"), "Name:\tsystemd\nVmRSS:\t   12000 kB\n")
	write(t, filepath.Join(proc, "4242", "comm"), "llama-server\n")
	write(t, filepath.Join(proc, "4242", "status"), "Name:\tllama-server\nVmRSS:\t 2097152 kB\nVmSwap:\t  524288 kB\n")
	write(t, filepath.Join(proc, "4242", "stat"), "4242 (llama-server) S 1 4242 4242 0 -1 4194560 100 0 0 0 1500 250 0 0 20 0 9 0 100 0 0\n")
	write(t, filepath.Join(proc, "self", "comm"), "llm-top\n")
	if amd {
		// iGPU with 512 MB and a 24 GB card: the big one counts
		card := func(n, tot, used, gtt, busy string) {
			dev := filepath.Join(sys, "class", "drm", n, "device")
			write(t, filepath.Join(dev, "mem_info_vram_total"), tot)
			write(t, filepath.Join(dev, "mem_info_vram_used"), used)
			write(t, filepath.Join(dev, "mem_info_gtt_used"), gtt)
			write(t, filepath.Join(dev, "gpu_busy_percent"), busy)
		}
		card("card0", "536870912\n", "100000000\n", "50000000\n", "3\n")
		card("card1", "25753026560\n", "21474836480\n", "1073741824\n", "97\n")
	}
	return proc, sys
}

func TestProbeLinuxAMD(t *testing.T) {
	proc, sys := fakeLinux(t, true)
	ss := probeLinux(proc, sys, []string{"llama-server"}, func() string { return "" })
	if !ss.OK || !near(ss.RamTot, 62.5) || !near(ss.RamFree, 31.25) || !near(ss.SrvWS, 2) || !near(ss.VramGB, 20) || !near(ss.VramTot, 23.98) {
		t.Errorf("%+v", ss)
	}
	if ss.Pid != 4242 || !near(ss.Spill, 1) || ss.GPUUtil != 97 || !near(ss.ProcSwap, 0.5) || !near(ss.SwapGB, 1) || !near(ss.CPUSec, 17.5) || ss.PageIn != 1200 {
		t.Errorf("fallbacks: %+v", ss)
	}
}

// A Ryzen desktop: amdgpu iGPU (512 MB) next to an NVIDIA card. The NVIDIA card is the one that matters, and the
// iGPU's GTT (desktop compositor) is not spill.
func TestProbeLinuxIGPUAndNvidia(t *testing.T) {
	proc, sys := fakeLinux(t, false)
	dev := filepath.Join(sys, "class", "drm", "card0", "device")
	write(t, filepath.Join(dev, "mem_info_vram_total"), "536870912\n")
	write(t, filepath.Join(dev, "mem_info_vram_used"), "500000000\n")
	write(t, filepath.Join(dev, "mem_info_gtt_used"), "2147483648\n")
	write(t, filepath.Join(dev, "gpu_busy_percent"), "4\n")
	ss := probeLinux(proc, sys, []string{"llama-server"}, func() string { return "20480, 24576, 91\n" })
	if !near(ss.VramTot, 24) || !near(ss.VramGB, 20) || ss.GPUUtil != 91 || ss.Spill != -1 {
		t.Errorf("%+v", ss)
	}
}

// The GPU engine filter must match this pid only: in WQL LIKE "_" is a wildcard, so 'pid_1818_%' matched pid 18184.
func TestPsScript(t *testing.T) {
	s := psScript([]string{"'llama-server'", "'LM Studio'"}, true)
	for _, want := range []string{"foreach ($n in @('llama-server','LM Studio'))", `"Name LIKE 'pid[_]"+$best.Id+"[_]%'"`, "if ($null -ne $v)"} {
		if !strings.Contains(s, want) {
			t.Errorf("probe script lacks %s", want)
		}
	}
	if strings.Contains(s, "%!") || strings.Contains(s, "%%") {
		t.Error("format verbs left in the probe script")
	}
}

func TestProbeLinuxNvidia(t *testing.T) {
	proc, sys := fakeLinux(t, false)
	ss := probeLinux(proc, sys, []string{"llama-server"}, func() string { return "1024, 8192, 5\n20480, 24576, 88\n" })
	if !ss.OK || !near(ss.VramGB, 20) || !near(ss.VramTot, 24) || ss.GPUUtil != 88 || ss.Spill != -1 {
		t.Errorf("%+v", ss)
	}
}

func TestProbeLinuxNoGPU(t *testing.T) {
	proc, sys := fakeLinux(t, false)
	ss := probeLinux(proc, sys, []string{"ollama-runner"}, func() string { return "" })
	if !ss.OK || ss.VramTot != 0 || ss.VramGB != 0 || ss.SrvWS != 0 || ss.Pid != 0 || ss.GPUUtil != -1 {
		t.Errorf("%+v", ss)
	}
	if probeLinux(filepath.Join(t.TempDir(), "none"), sys, []string{"x"}, nil).OK {
		t.Error("OK without /proc/meminfo")
	}
}

func TestFindProc(t *testing.T) {
	proc, _ := fakeLinux(t, false)
	write(t, filepath.Join(proc, "77", "comm"), "llama-server-cu\n") // comm keeps 15 bytes
	write(t, filepath.Join(proc, "77", "status"), "VmRSS:\t1024 kB\n")
	if got := procRSS(proc, "llama-server-cuda"); got != 1024 {
		t.Errorf("rss %v", got)
	}
	// LM Studio runs several processes with one name: the biggest is the engine
	for pid, rss := range map[string]string{"500": "300000", "501": "9000000", "502": "80000"} {
		write(t, filepath.Join(proc, pid, "comm"), "lm-studio\n")
		write(t, filepath.Join(proc, pid, "status"), "VmRSS:\t"+rss+" kB\n")
	}
	if pid, name := findProc(proc, procNames("lmstudio", "llama-server")); pid != 501 || name != "lm-studio" {
		t.Errorf("lm studio: %d %q", pid, name)
	}
	if pid, _ := findProc(proc, procNames("llama.cpp", "llama-server")); pid != 4242 {
		t.Errorf("llama: %d", pid)
	}
}

func TestStatCPU(t *testing.T) {
	if got := statCPU("12 (my (odd) name) R 1 1 1 0 -1 0 0 0 0 0 250 50 0 0"); !near(got, 3) {
		t.Errorf("%v", got)
	}
	if statCPU("garbage") != 0 {
		t.Error("garbage")
	}
}

func TestRates(t *testing.T) {
	prev := sysState{OK: true, Pid: 9, CPUSec: 100}
	prev = rates(prev, sysState{}, 0, true)
	prev.pageInRaw = 1000
	ss := rates(sysState{OK: true, Pid: 9, CPUSec: 110, PageIn: 6000}, prev, 5, true)
	if !near(ss.Cores, 2) || !near(ss.PageIn, 1000) {
		t.Errorf("%+v", ss)
	}
	if ss := rates(sysState{OK: true, Pid: 10, CPUSec: 5}, prev, 5, false); ss.Cores != -1 {
		t.Error("cores across a pid change")
	}
	if ss := rates(sysState{OK: true, Pid: 9, CPUSec: 120, PageIn: 42}, ss, 5, false); ss.PageIn != 42 {
		t.Error("windows page-ins are already a rate")
	}
}

func TestParseNvidiaSmi(t *testing.T) {
	if _, _, _, ok := parseNvidiaSmi(""); ok {
		t.Error("empty")
	}
	if _, _, _, ok := parseNvidiaSmi("NVIDIA-SMI has failed"); ok {
		t.Error("error text")
	}
	if u, tot, util, ok := parseNvidiaSmi("[N/A], [N/A], [N/A]\n500, 16384, [N/A]"); !ok || u != 500 || tot != 16384 || util != -1 {
		t.Errorf("%v %v %v %v", u, tot, util, ok)
	}
}

func TestReadHold(t *testing.T) {
	d := t.TempDir()
	if readHold(filepath.Join(d, "none")) != "" || readHold("") != "" {
		t.Error("hold without file")
	}
	write(t, filepath.Join(d, "h"), "benchmark run\nmore")
	if got := readHold(filepath.Join(d, "h")); got != "benchmark run" {
		t.Errorf("%q", got)
	}
	write(t, filepath.Join(d, "e"), "\n")
	if got := readHold(filepath.Join(d, "e")); got != "GPU job" {
		t.Errorf("%q", got)
	}
}

func TestProcNameGuard(t *testing.T) {
	for _, ok := range []string{"llama-server", "llama-server.exe", "koboldcpp", "LM Studio"} {
		if !procName.MatchString(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"x'; Remove-Item C:\\", "a$b", "`calc`", "it's"} {
		if procName.MatchString(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// LM Studio 0.4 runs its engine as a llama-server under ~/.lmstudio: it is LM Studio's engine when watching LM
// Studio, and never the user's own llama-server.
func TestLMStudioEngineProcess(t *testing.T) {
	proc, _ := fakeLinux(t, false) // has the user's llama-server as 4242
	eng := filepath.Join(proc, "900")
	write(t, filepath.Join(eng, "comm"), "llama-server\n")
	write(t, filepath.Join(eng, "status"), "VmRSS:\t 9000000 kB\n")
	if err := os.Symlink("/home/u/.lmstudio/extensions/backends/llama.cpp-linux/llama-server", filepath.Join(eng, "exe")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if pid, name := findProc(proc, procNames("lmstudio", "llama-server")); pid != 900 || name != "LM Studio" {
		t.Errorf("lmstudio: pid %d %q", pid, name)
	}
	if pid, name := findProc(proc, procNames("llama.cpp", "llama-server")); pid != 4242 || name != "llama-server" {
		t.Errorf("llama.cpp: pid %d %q", pid, name)
	}
	s := psScript([]string{"'llama-server'"}, true)
	for _, want := range []string{"$lm=1", `-match '[\\/]\.lmstudio[\\/]'`, "$nm='LM Studio'"} {
		if !strings.Contains(s, want) {
			t.Errorf("probe script lacks %s", want)
		}
	}
}

func TestProbeLiveTmp(t *testing.T) {
	if os.Getenv("LLMTOP_LIVE") == "" {
		t.Skip("live probe: set LLMTOP_LIVE=1")
	}
	for _, k := range []string{"lmstudio", "llama.cpp"} {
		ss := probeSys(procNames(k, "llama-server"))
		t.Logf("%s: pid %d proc %q guess %v procVram %.2f gpu %.0f", k, ss.Pid, ss.Proc, ss.Guess, ss.ProcVram, ss.GPUUtil)
	}
}

// The probe reports where the server process listens, so a llama-server on another port is found without config.
func TestListenAddrs(t *testing.T) {
	ss := parseWinProbe(winProbeOut + "listen 127.0.0.1 8091\r\nlisten :: 8092\r\n")
	if !ss.OK || len(ss.Listen) != 2 || ss.Listen[0] != "127.0.0.1:8091" || ss.Listen[1] != "[::]:8092" {
		t.Errorf("windows listen: %+v", ss.Listen)
	}
	for in, want := range map[string]string{"0100007F:1F9B": "127.0.0.1:8091", "00000000:1F90": "0.0.0.0:8080",
		"00000000000000000000000001000000:1F9B": "[::1]:8091", "zz:1": "", "0100007F": ""} {
		if got := hexAddr(in); got != want {
			t.Errorf("hexAddr(%q) = %q, want %q", in, got, want)
		}
	}
	proc, _ := fakeLinux(t, false)
	fd := filepath.Join(proc, "4242", "fd")
	os.MkdirAll(fd, 0o755)
	if err := os.Symlink("socket:[55501]", filepath.Join(fd, "3")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	write(t, filepath.Join(proc, "net", "tcp"), "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 0100007F:1F9B 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 55501 1 0\n"+
		"   1: 0100007F:2328 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 99999 1 0\n")
	if got := listenAddrs(proc, 4242); len(got) != 1 || got[0] != "127.0.0.1:8091" {
		t.Errorf("linux listen: %v", got)
	}
}
