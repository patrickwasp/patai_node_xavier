package resources

import (
	"math"
	"testing"
)

func TestStat(t *testing.T) {
	// From the bench unit: user nice system idle iowait irq softirq steal guest guest_nice.
	times, ok := ParseStat([]byte("cpu  40238 83329 182857 19744395 6874 14262 14015 0 5 5\ncpu0 1 2 3 4 5 6 7 8 0 0\n"))
	if !ok || times.Total != 40238+83329+182857+19744395+6874+14262+14015 || times.Busy != 40238+83329+182857+14262+14015 {
		t.Fatalf("%+v", times)
	}
	for _, bad := range []string{"", "intr 1 2 3", "cpu 1 2 x 4 5"} {
		if _, ok := ParseStat([]byte(bad)); ok {
			t.Fatalf("%q", bad)
		}
	}
}

func TestPercent(t *testing.T) {
	if got := Percent(CPUTimes{100, 1000}, CPUTimes{150, 1100}); got == nil || *got != 50 {
		t.Fatalf("%v", got)
	}
	if Percent(CPUTimes{100, 1000}, CPUTimes{100, 1000}) != nil {
		t.Fatal("no time passed")
	}
	if Percent(CPUTimes{100, 1000}, CPUTimes{90, 1100}) != nil {
		t.Fatal("counters went back")
	}
}

func TestMeminfoAndCombine(t *testing.T) {
	memory := ParseMeminfo([]byte("MemTotal:       31734248 kB\nMemFree:        19189676 kB\nMemAvailable:   26893612 kB\n" +
		"SwapTotal:      15866960 kB\nSwapFree:       15800000 kB\nHugePages_Total:       0\n"))
	if memory["MemTotal"] != 31734248*1024 || memory["HugePages_Total"] != 0 {
		t.Fatalf("%v", memory)
	}
	pool := int64(991695 * 4096)
	gpu := int64(5 << 30)
	got := Combine(memory, &pool, &gpu, nil, nil)
	if got.MemoryUsed != (31734248-26893612)*1024-pool || got.MemoryTotal != 31734248*1024 || *got.GPUMemory != gpu {
		t.Fatalf("%+v", got)
	}
	if got.SwapUsed != (15866960-15800000)*1024 {
		t.Fatalf("%+v", got)
	}
	// A pool bigger than what reads as used can't make it less than nothing.
	huge := int64(1 << 40)
	if Combine(memory, &huge, nil, nil, nil).MemoryUsed != 0 {
		t.Fatal("below zero")
	}
}

func TestClients(t *testing.T) {
	list := "CLIENT                        PROCESS      PID        SIZE\nuser                   llama-server   4242    5413020K\n" +
		"user                   nvargus-daemon      892          0K\ntotal                                             5413020K\n"
	if total, ok := ParseClients([]byte(list)); !ok || total != 5413020*1024 {
		t.Fatalf("%d %v", total, ok)
	}
	if _, ok := ParseClients([]byte("CLIENT PROCESS PID SIZE\n")); ok {
		t.Fatal("no total")
	}
}

func TestGPULoad(t *testing.T) {
	if load, ok := ParseGPULoad([]byte("994\n")); !ok || math.Abs(load-99.4) > 1e-9 {
		t.Fatalf("%v %v", load, ok)
	}
	if load, _ := ParseGPULoad([]byte("1200")); load != 100 {
		t.Fatalf("%v", load)
	}
	if _, ok := ParseGPULoad([]byte("x")); ok {
		t.Fatal("not a number")
	}
}

func TestClientsByPIDAndMemoryStat(t *testing.T) {
	list := "CLIENT                        PROCESS      PID        SIZE\nuser                   llama-server   4242    5413020K\n" +
		"user                   some process    77        100K\nuser                   llama-server   4242       10K\n" +
		"total                                             5413130K\n"
	got := ParseClientsByPID([]byte(list))
	if len(got) != 2 || got[4242] != 5413030*1024 || got[77] != 100*1024 {
		t.Fatalf("%v", got)
	}
	stat := ParseMemoryStat([]byte("anon 2048\nfile 4096\nanon_thp 0\n"))
	if stat["anon"] != 2048 || stat["file"] != 4096 || len(stat) != 3 {
		t.Fatalf("%v", stat)
	}
}

func TestCgroup(t *testing.T) {
	for data, want := range map[string]string{
		"0::/system.slice/docker-ab12.scope\n": "/sys/fs/cgroup/system.slice/docker-ab12.scope",
		"0::/\n":                               "/sys/fs/cgroup/",
	} {
		if got, ok := Cgroup([]byte(data)); !ok || got != want {
			t.Errorf("%q: %q %v", data, got, ok)
		}
	}
	// cgroup v1's lines, and a path that would leave /sys/fs/cgroup.
	for _, data := range []string{"12:memory:/docker/ab12\n1:name=systemd:/docker/ab12\n", "0::/../../etc\n", ""} {
		if got, ok := Cgroup([]byte(data)); ok {
			t.Errorf("%q: %q", data, got)
		}
	}
}
