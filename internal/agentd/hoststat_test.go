package agentd

import (
	"runtime"
	"testing"
	"time"
)

func TestParseProcStatAndPercent(t *testing.T) {
	const first = "cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 50 0 25 400 25 0 0 0 0 0\ncpu1 50 0 25 400 25 0 0 0 0 0\n"
	const second = "cpu  200 0 100 1400 100 0 0 0 0 0\ncpu0 100 0 50 700 50 0 0 0 0 0\ncpu1 100 0 50 700 50 0 0 0 0 0\n"
	prev, cores, ok := parseProcStat(first)
	if !ok || cores != 2 || prev.total == 0 {
		t.Fatalf("first sample = %+v cores=%d ok=%v", prev, cores, ok)
	}
	next, _, ok := parseProcStat(second)
	if !ok {
		t.Fatal("second sample failed")
	}
	// idle grew by 650 (idle+iowait), total grew by 800 → 18.75% busy
	pct, ok := cpuPercent(prev, next)
	if !ok {
		t.Fatal("cpuPercent rejected a forward sample")
	}
	if pct < 18 || pct > 19 {
		t.Fatalf("cpuPercent = %v, want ~18.75", pct)
	}
}

func TestParseMeminfo(t *testing.T) {
	text := "" +
		"MemTotal:       1000 kB\n" +
		"MemFree:         100 kB\n" +
		"MemAvailable:    400 kB\n" +
		"Buffers:          50 kB\n" +
		"Cached:          100 kB\n" +
		"SwapTotal:       200 kB\n" +
		"SwapFree:         50 kB\n"
	mem, swap := parseMeminfo(text)
	if mem == nil || mem.Total != 1000*1024 || mem.Used != 600*1024 {
		t.Fatalf("memory = %+v", mem)
	}
	if swap == nil || swap.Total != 200*1024 || swap.Used != 150*1024 {
		t.Fatalf("swap = %+v", swap)
	}

	fallback, none := parseMeminfo("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 100 kB\n")
	if none != nil {
		t.Fatalf("unexpected swap %+v", none)
	}
	if fallback == nil || fallback.Used != 750*1024 {
		t.Fatalf("fallback memory = %+v", fallback)
	}
}

func TestParseLoadUptimeOSCPU(t *testing.T) {
	load, ok := parseLoadavg("0.40 0.30 0.20 1/200 1234\n")
	if !ok || load.One != 0.4 || load.Five != 0.3 || load.Fifteen != 0.2 {
		t.Fatalf("load = %+v ok=%v", load, ok)
	}
	if got := parseUptime("12345.67 999.0\n"); got != 12345 {
		t.Fatalf("uptime = %d", got)
	}
	distro := parseOSRelease("NAME=\"Arch Linux\"\nPRETTY_NAME=\"Arch Linux\"\n")
	if distro != "Arch Linux" {
		t.Fatalf("distro = %q", distro)
	}
	if model := parseCPUModel("processor : 0\nmodel name : AMD Ryzen\n"); model != "AMD Ryzen" {
		t.Fatalf("model = %q", model)
	}
}

func TestParseMounts(t *testing.T) {
	text := "" +
		"proc /proc proc rw 0 0\n" +
		"/dev/nvme0n1p2 / ext4 rw 0 0\n" +
		"tmpfs /tmp tmpfs rw 0 0\n" +
		"/dev/sda1 /home\\040disk ext4 rw 0 0\n" +
		"/dev/sda1 /home\\040disk ext4 rw 0 0\n" +
		"overlay /var/lib/docker overlay rw 0 0\n" +
		"overlay / overlay rw 0 0\n"
	got := parseMounts(text)
	if len(got) != 2 || got[0] != "/" || got[1] != "/home disk" {
		t.Fatalf("mounts = %#v", got)
	}
	overlayRoot := parseMounts("overlay / overlay rw 0 0\noverlay /var/lib/docker/overlay2/abc overlay rw 0 0\n")
	if len(overlayRoot) != 1 || overlayRoot[0] != "/" {
		t.Fatalf("overlay mounts = %#v", overlayRoot)
	}
}

func TestReadNetsSkipsLoopbackAndRanksPhysical(t *testing.T) {
	nets := readNets()
	for _, n := range nets {
		if n.Name == "lo" {
			t.Fatalf("loopback included: %+v", n)
		}
	}
	physical := -1
	virtual := -1
	for i, n := range nets {
		if virtualNIC(n.Name) {
			if virtual < 0 {
				virtual = i
			}
			continue
		}
		if len(n.Addrs) > 0 && physical < 0 {
			physical = i
		}
	}
	if physical >= 0 && virtual >= 0 && physical > virtual {
		t.Fatalf("physical nic ranked after virtual: %+v", nets)
	}
}

func TestHostSnapshotLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host proc files")
	}
	start := time.Now()
	snap := (&hostCollector{}).snapshot()
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("first snapshot should take a short CPU sample")
	}
	if snap.OS != "linux" || snap.Arch == "" {
		t.Fatalf("identity = %+v", snap)
	}
	if snap.Memory == nil || snap.Memory.Total == 0 || snap.Memory.Used > snap.Memory.Total {
		t.Fatalf("memory = %+v", snap.Memory)
	}
	if snap.CPU == nil || snap.CPU.Cores < 1 || snap.CPU.Usage == nil {
		t.Fatalf("cpu = %+v", snap.CPU)
	}
	if *snap.CPU.Usage < 0 || *snap.CPU.Usage > 100 {
		t.Fatalf("usage = %v", *snap.CPU.Usage)
	}
	if snap.Load == nil {
		t.Fatal("load missing")
	}
	if len(snap.Disks) == 0 || snap.Disks[0].Mount != "/" || snap.Disks[0].Total == 0 {
		t.Fatalf("disks = %+v", snap.Disks)
	}
}
