// Package resources reads how busy the unit is right now (docs/console.md, Status): CPU and GPU load, and
// memory. The CPU and GPU share 32 GB, so GPU memory is part of memory in use, not beside it. nvmap keeps freed
// GPU memory in a pool that Linux counts as used and the next CUDA allocation takes first, so in use leaves it
// out (docs/apps.md, Measured). nvmap's counts need root, so patai-rootd reads them.
package resources

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Stat     = "/proc/stat"
	Meminfo  = "/proc/meminfo"
	GPULoad  = "/sys/devices/gpu.0/load"
	Clients  = "/sys/kernel/debug/nvmap/iovmm/clients"
	PoolSize = "/sys/kernel/debug/nvmap/pagepool/page_pool_available_pages"
	pageSize = 4096
)

// Reading is a moment's load and memory, in bytes. A figure the unit couldn't read is nil.
type Reading struct {
	CPUPercent  *float64 `json:"cpu_percent"`
	GPUPercent  *float64 `json:"gpu_percent"`
	MemoryTotal int64    `json:"memory_total"`
	MemoryUsed  int64    `json:"memory_used"`
	GPUMemory   *int64   `json:"gpu_memory"`
	SwapTotal   int64    `json:"swap_total"`
	SwapUsed    int64    `json:"swap_used"`
}

// CPUTimes are the jiffies all cores spent busy, and in all.
type CPUTimes struct{ Busy, Total uint64 }

// ParseStat reads the first line of /proc/stat. Idle and waiting for I/O count as not busy.
func ParseStat(data []byte) (CPUTimes, bool) {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 5 || fields[0] != "cpu" {
		return CPUTimes{}, false
	}
	var times CPUTimes
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return CPUTimes{}, false
		}
		// guest and guest_nice are already in user and nice.
		if i >= 8 {
			break
		}
		times.Total += value
		if i != 3 && i != 4 {
			times.Busy += value
		}
	}
	return times, true
}

// Percent is how busy the cores were between two readings, or nil if no time passed.
func Percent(before, after CPUTimes) *float64 {
	if after.Total <= before.Total || after.Busy < before.Busy {
		return nil
	}
	share := float64(after.Busy-before.Busy) / float64(after.Total-before.Total) * 100
	return &share
}

// ParseMeminfo gives /proc/meminfo's figures in bytes.
func ParseMeminfo(data []byte) map[string]int64 {
	found := map[string]int64{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			value *= 1024
		}
		found[key] = value
	}
	return found
}

// ParseClients gives the total of nvmap's list of GPU memory by process, in bytes.
func ParseClients(data []byte) (int64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "total" && strings.HasSuffix(fields[1], "K") {
			kib, err := strconv.ParseInt(strings.TrimSuffix(fields[1], "K"), 10, 64)
			return kib * 1024, err == nil
		}
	}
	return 0, false
}

// ParseClientsByPID gives nvmap's GPU memory for each process, in bytes. A line ends with its PID and size, and a
// process's name can have spaces in it.
func ParseClientsByPID(data []byte) map[int]int64 {
	found := map[int]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "total" || !strings.HasSuffix(fields[len(fields)-1], "K") {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-2])
		kib, err2 := strconv.ParseInt(strings.TrimSuffix(fields[len(fields)-1], "K"), 10, 64)
		if err == nil && err2 == nil {
			found[pid] += kib * 1024
		}
	}
	return found
}

// Cgroup is the cgroup v2 directory a process is in, from its /proc/<pid>/cgroup, whose one line on the unified
// hierarchy is "0::<path>".
func Cgroup(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::/"); ok && !strings.Contains(path, "..") {
			return "/sys/fs/cgroup/" + path, true
		}
	}
	return "", false
}

// ParseMemoryStat gives a cgroup's memory.stat figures, in bytes.
func ParseMemoryStat(data []byte) map[string]int64 {
	found := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if number, err := strconv.ParseInt(value, 10, 64); ok && err == nil {
			found[key] = number
		}
	}
	return found
}

// ParseGPULoad reads the GPU's load, which the driver gives in tenths of a percent.
func ParseGPULoad(data []byte) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return min(value/10, 100), true
}

// Combine puts a reading together from what was read. pool is nvmap's pool in bytes, or nil if unknown.
func Combine(memory map[string]int64, pool *int64, gpuMemory *int64, cpu, gpu *float64) Reading {
	reading := Reading{CPUPercent: cpu, GPUPercent: gpu, GPUMemory: gpuMemory}
	reading.MemoryTotal = memory["MemTotal"]
	reading.MemoryUsed = memory["MemTotal"] - memory["MemAvailable"]
	if pool != nil {
		reading.MemoryUsed -= *pool
	}
	reading.MemoryUsed = max(reading.MemoryUsed, 0)
	reading.SwapTotal = memory["SwapTotal"]
	reading.SwapUsed = max(memory["SwapTotal"]-memory["SwapFree"], 0)
	return reading
}

// Reader keeps the last CPU reading, so each answer covers the time since the one before. With none recent,
// it looks twice, a moment apart.
type Reader struct {
	mu   sync.Mutex
	last CPUTimes
	at   time.Time
}

const (
	stale   = time.Minute
	glimpse = 300 * time.Millisecond
)

func (r *Reader) cpu() *float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := os.ReadFile(Stat)
	now, ok := ParseStat(data)
	if err != nil || !ok {
		return nil
	}
	before := r.last
	if r.at.IsZero() || time.Since(r.at) > stale {
		before = now
		time.Sleep(glimpse)
		data, err = os.ReadFile(Stat)
		if now, ok = ParseStat(data); err != nil || !ok {
			return nil
		}
	}
	r.last, r.at = now, time.Now()
	return Percent(before, now)
}

// Read reads the unit now. It needs root for GPU memory and the pool, and leaves them out without it.
func (r *Reader) Read() Reading {
	cpu := r.cpu()
	var gpu *float64
	if data, err := os.ReadFile(GPULoad); err == nil {
		if load, ok := ParseGPULoad(data); ok {
			gpu = &load
		}
	}
	data, _ := os.ReadFile(Meminfo)
	var pool, gpuMemory *int64
	if data, err := os.ReadFile(PoolSize); err == nil {
		if pages, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
			size := pages * pageSize
			pool = &size
		}
	}
	if data, err := os.ReadFile(Clients); err == nil {
		if total, ok := ParseClients(data); ok {
			gpuMemory = &total
		}
	}
	return Combine(ParseMeminfo(data), pool, gpuMemory, cpu, gpu)
}
