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
	write(t, filepath.Join(proc, "4242", "status"), "Name:\tllama-server\nVmRSS:\t 9437184 kB\nRssAnon:\t 2080768 kB\nRssFile:\t 7340032 kB\nRssShmem:\t   16384 kB\nVmSwap:\t  524288 kB\n")
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
	// the counters name adapters luid_0x<high 8 hex>_0x<low 8 hex>; DEVPKEY_Gpu_Luid is one UInt64
	for _, want := range []string{"foreach ($n in @('llama-server','LM Studio'))", `"Name LIKE 'pid[_]"+$best.Id+"[_]%'"`, "if ($null -ne $v)",
		"'luid_0x{0:X8}_0x{1:X8}' -f [uint32]($u -shr 32),[uint32]($u -band 4294967295)", `-like "$($id)_*"`} {
		if !strings.Contains(s, want) {
			t.Errorf("probe script lacks %s", want)
		}
	}
	if strings.Contains(s, "%!") || strings.Contains(s, "%%") {
		t.Error("format verbs left in the probe script")
	}
}

// Two 7900s holding a split model (-dev ROCm0,ROCm1) and the Ryzen iGPU: the process's share of each card comes from
// its DRM fdinfo, one count per client however many fds repeat it.
func TestProbeLinuxSplitModel(t *testing.T) {
	proc, sys := fakeLinux(t, false)
	card := func(n, pdev, tot, used, gtt, busy string) {
		dev := filepath.Join(sys, "class", "drm", n, "device")
		write(t, filepath.Join(dev, "mem_info_vram_total"), tot)
		write(t, filepath.Join(dev, "mem_info_vram_used"), used)
		write(t, filepath.Join(dev, "mem_info_gtt_used"), gtt)
		write(t, filepath.Join(dev, "gpu_busy_percent"), busy)
		write(t, filepath.Join(dev, "uevent"), "DRIVER=amdgpu\nPCI_ID=1002:744C\nPCI_SLOT_NAME="+pdev+"\n")
	}
	card("card1", "0000:03:00.0", "25753026560\n", "20053274624\n", "900000000\n", "40\n")
	card("card2", "0000:06:00.0", "21458059264\n", "15406784512\n", "100000000\n", "35\n")
	card("card3", "0000:76:00.0", "536870912\n", "63123456\n", "300000000\n", "2\n")
	fdinfo := func(fd, pdev, id, vram, gtt string) {
		write(t, filepath.Join(proc, "4242", "fdinfo", fd), "pos:\t0\nflags:\t02100002\ndrm-driver:\tamdgpu\ndrm-client-id:\t"+id+
			"\ndrm-pdev:\t"+pdev+"\ndrm-memory-vram:\t"+vram+"\ndrm-memory-gtt: \t"+gtt+"\ndrm-memory-cpu: \t0 KiB\n")
	}
	write(t, filepath.Join(proc, "4242", "fdinfo", "0"), "pos:\t0\nflags:\t0100002\nmnt_id:\t25\n")
	fdinfo("5", "0000:03:00.0", "415", "18558284 KiB", "26708 KiB")
	fdinfo("6", "0000:03:00.0", "415", "18558284 KiB", "26708 KiB") // a dup of fd 5
	fdinfo("7", "0000:06:00.0", "416", "14939528 KiB", "524288 KiB")
	fdinfo("8", "0000:76:00.0", "417", "88 KiB", "26720 KiB")
	ss := probeLinux(proc, sys, []string{"llama-server"}, func() string { return "" })
	if ss.modelCards() != 2 || len(ss.GPUs) != 3 || ss.GPUs[0].Name != "card1" || !ss.GPUs[1].Model || ss.GPUs[2].Model {
		t.Fatalf("cards: %+v", ss.GPUs)
	}
	if !near(ss.VramGB, 33.02) || !near(ss.VramTot, 43.97) || !near(ss.ProcVram, 31.95) || ss.GPUUtil != 75 {
		t.Errorf("%+v", ss)
	}
	if !near(ss.Spill, 0.52) || !near(ss.GPUs[1].Shared, 0.5) || !near(ss.GPUs[0].Proc, 17.7) {
		t.Errorf("spill %.2f, cards %+v", ss.Spill, ss.GPUs)
	}
	// no server: the desktop's GTT is not spill
	if ss := probeLinux(proc, sys, []string{"ollama-runner"}, func() string { return "" }); ss.Spill != -1 || !near(ss.VramTot, 23.98) || ss.GPUUtil != 40 {
		t.Errorf("no process: %+v", ss)
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

// Two discrete cards (RX 7900 XTX 24 GB, RX 7900 XT 20 GB) and the iGPU, from one machine's counters. The legacy
// lines are maxima over adapters; the gpu lines are what counts. iGPU and idle card always carry a little of the
// process's memory.
func dualProbe(xtx, xt string) string {
	return "vram_used 23886606336\nvram_total 25753026560\nram_free_kb 18681280\nram_total_kb 32380768\nswap_mb 1153\npages_in 0\n" +
		"pid 25640\nguess 0\nws 2114416640\ncpu_s 38.15\nproc_vram 22135398400\nproc_shared 325439488\ngpu_util 60\nname llama-server\n" +
		"gpu 536870912 408150016 149258240 388784128 0 AMD Radeon(TM) Graphics\n" +
		"gpu 25753026560 " + xtx + " AMD Radeon RX 7900 XTX\n" +
		"gpu 21458059264 " + xt + " AMD Radeon RX 7900 XT\n"
}

func TestParseWinProbeCards(t *testing.T) {
	// -dev ROCm0: all on the XTX, as with one GPU
	ss := parseWinProbe(dualProbe("23886606336 22135398400 325439488 60", "6430720 3211264 316837888 0"))
	if len(ss.GPUs) != 3 || ss.GPUs[0].Name != "AMD Radeon RX 7900 XTX" || !ss.GPUs[0].Model || ss.GPUs[1].Model || ss.GPUs[2].Model || ss.modelCards() != 1 {
		t.Fatalf("cards: %+v", ss.GPUs)
	}
	if !near(ss.VramGB, 22.25) || !near(ss.VramTot, 23.98) || !near(ss.ProcVram, 20.62) || !near(ss.Spill, 0.303) || ss.GPUUtil != 60 {
		t.Errorf("XTX only: %+v", ss)
	}
	// -dev ROCm1: the old maxima read 18 GB of the XTX's 24 and never saw the XT fill up
	ss = parseWinProbe(dualProbe("1900000000 0 316837888 0", "20401094656 19327352832 1073741824 95"))
	if !near(ss.VramGB, 19) || !near(ss.VramTot, 19.98) || !near(ss.Spill, 1) || ss.GPUUtil != 95 {
		t.Errorf("XT only: %+v", ss)
	}
	if v, _ := bottleneck(true, ss, ss.VramTot, 16); !strings.Contains(v, "VRAM full") {
		t.Errorf("XT only verdict %q", v)
	}
	// split: sums, load summed (the cards take turns)
	ss = parseWinProbe(dualProbe("15032385536 13958643712 343932928 35", "12884901888 11811160064 322122547 30"))
	if ss.modelCards() != 2 || !near(ss.VramGB, 26) || !near(ss.VramTot, 43.97) || !near(ss.ProcVram, 24) || !near(ss.Spill, 0.62) || ss.GPUUtil != 65 {
		t.Errorf("split: %+v", ss)
	}
	if v, _ := bottleneck(true, ss, ss.VramTot, 16); !strings.Contains(v, "GPU-bound") {
		t.Errorf("split verdict %q", v)
	}
	// split, the XT full and spilling while the XTX has room: 39 of 44 GB is not 90%, the card is
	ss = parseWinProbe(dualProbe("19327352832 18253611008 343932928 50", "21152923648 20401094656 1610612736 50"))
	if v, _ := bottleneck(true, ss, ss.VramTot, 16); !strings.Contains(v, "VRAM full") {
		t.Errorf("one card overflowing: %q (%+v)", v, ss)
	}
	// split, both full but only the normal shared memory per card: 0.5 + 0.5 GB is over the one-card threshold
	ss = parseWinProbe(dualProbe("24696061952 23622320128 536870912 50", "20937965568 19864223744 536870912 50"))
	if v, _ := bottleneck(true, ss, ss.VramTot, 16); strings.Contains(v, "VRAM full") || overflow(ss, ss.VramTot) {
		t.Errorf("split at rest flagged: %q", v)
	}
	// no server process: VRAM is the biggest card's, not the busiest adapter's
	ss = parseWinProbe("ram_total_kb 8000000\nvram_used 9000000000\nvram_total 25753026560\n" +
		"gpu 21458059264 9000000000 0 0 -1 AMD Radeon RX 7900 XT\ngpu 25753026560 2000000000 0 0 -1 AMD Radeon RX 7900 XTX\n")
	if !near(ss.VramGB, 1.86) || !near(ss.VramTot, 23.98) || ss.modelCards() != 0 || ss.GPUUtil != -1 {
		t.Errorf("no process: %+v", ss)
	}
	// a malformed gpu line is skipped, it never blanks the panel
	ss = parseWinProbe(winProbeOut + "gpu 1 x 2 3 4 bad\ngpu 5\n")
	if !ss.OK || len(ss.GPUs) != 0 || !near(ss.VramGB, 23.756) {
		t.Errorf("malformed: %+v", ss)
	}
}

func TestCardName(t *testing.T) {
	for in, want := range map[string]string{"AMD Radeon RX 7900 XTX": "RX 7900 XTX", "NVIDIA GeForce RTX 4090": "RTX 4090", "NVIDIA RTX A6000": "RTX A6000", "": "GPU"} {
		if got := cardName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
