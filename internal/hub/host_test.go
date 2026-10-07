package hub

import (
	"math"
	"testing"
	"time"
)

func TestNormalizeHostClipsAndDropsEmpty(t *testing.T) {
	if normalizeHost(nil) != nil {
		t.Fatal("nil host should stay nil")
	}
	if normalizeHost(&HostSnapshot{}) != nil {
		t.Fatal("empty host should stay nil")
	}
	usage := 140.0
	bad := math.NaN()
	nets := make([]HostNet, 20)
	for i := range nets {
		nets[i] = HostNet{Name: "eth0", Addrs: []string{"10.0.0.1/24", "\x1b[31mred"}}
	}
	got := normalizeHost(&HostSnapshot{
		OS:     "linux\n",
		Distro: "Arch\x1b[0m",
		CPU:    &HostCPU{Cores: 9000, Usage: &usage, Model: "cpu"},
		Memory: &HostMemory{Total: 100, Used: 250},
		Load:   &HostLoad{One: bad, Five: -1, Fifteen: 0.5},
		Nets:   nets,
	})
	if got == nil || got.OS != "linux" || got.Distro != "Arch" {
		t.Fatalf("identity = %+v", got)
	}
	if got.CPU == nil || got.CPU.Cores != 4096 || got.CPU.Usage == nil || *got.CPU.Usage != 100 {
		t.Fatalf("cpu = %+v", got.CPU)
	}
	if got.Memory == nil || got.Memory.Used != 100 {
		t.Fatalf("memory = %+v", got.Memory)
	}
	if got.Load == nil || got.Load.One != 0 || got.Load.Five != 0 || got.Load.Fifteen != 0.5 {
		t.Fatalf("load = %+v", got.Load)
	}
	if len(got.Nets) != 12 || len(got.Nets[0].Addrs) != 1 || got.Nets[0].Addrs[0] != "10.0.0.1/24" {
		t.Fatalf("nets = %+v", got.Nets)
	}
}

func TestRegisterKeepsReportedHostAndDrift(t *testing.T) {
	r := NewRegistry()
	usage := 12.5
	if err := r.Register(AgentInfo{
		AgentID:  "a",
		Hostname: "box",
		Mode:     AgentModeConnect,
		Host:     &HostSnapshot{OS: "linux", CPU: &HostCPU{Cores: 4, Usage: &usage}},
		Drift:    &AgentDrift{Pull: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentInfo{AgentID: "a", Hostname: "box2", Mode: AgentModeConnect, Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	info, ok := r.Get("a")
	if !ok || info.Hostname != "box2" || info.Version != "v2" {
		t.Fatalf("upsert identity = %+v", info)
	}
	if info.Drift == nil || info.Drift.Pull != 2 || info.Host == nil || info.Host.OS != "linux" || info.Host.CPU == nil || info.Host.CPU.Usage == nil || *info.Host.CPU.Usage != 12.5 {
		t.Fatalf("upsert dropped report: %+v drift=%+v", info.Host, info.Drift)
	}
	info.Host.OS = "mutated"
	again, _ := r.Get("a")
	if again.Host == nil || again.Host.OS != "linux" {
		t.Fatalf("Get host is aliased: %+v", again.Host)
	}

	replacement := &HostSnapshot{OS: "fresh", Memory: &HostMemory{Total: 10, Used: 1}}
	if err := r.Register(AgentInfo{AgentID: "a", Hostname: "box2", Mode: AgentModeConnect, Host: replacement}); err != nil {
		t.Fatal(err)
	}
	info, _ = r.Get("a")
	if info.Host == nil || info.Host.OS != "fresh" || info.Host.Memory == nil || info.Drift == nil || info.Drift.Pull != 2 || info.Version != "v2" {
		t.Fatalf("replacement = %+v drift=%+v version=%q", info.Host, info.Drift, info.Version)
	}
	if time.Since(info.LastSeen) > time.Minute {
		t.Fatalf("last seen = %s", info.LastSeen)
	}
}

func TestUpdateHost(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(AgentInfo{AgentID: "a", Mode: AgentModeConnect}); err != nil {
		t.Fatal(err)
	}
	r.UpdateHost("missing", HostSnapshot{OS: "nope"})
	r.UpdateHost("a", HostSnapshot{})
	info, _ := r.Get("a")
	if info.Host != nil {
		t.Fatalf("empty update stored %+v", info.Host)
	}
	r.UpdateHost("a", HostSnapshot{OS: "linux", Nets: []HostNet{{Name: "enp3s0", Addrs: []string{"192.168.1.20/24"}, Up: true}}})
	info, _ = r.Get("a")
	if info.Host == nil || len(info.Host.Nets) != 1 || info.Host.Nets[0].Addrs[0] != "192.168.1.20/24" {
		t.Fatalf("host = %+v", info.Host)
	}
}
