//go:build waza_diagnostic_late_profile

package controlledcomparison

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"
)

// Disposable diagnostic instrumentation, never part of the product build.
// Allocation snapshots are cumulative sampled alloc_space, not RSS or deltas.
func TestMain(m *testing.M) {
	dir := os.Getenv("WAZA_DIAGNOSTIC_PROFILE_DIR")
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "diagnostic profiling: %v\n", err)
		os.Exit(2)
	}
	if !filepath.IsAbs(dir) {
		fail(fmt.Errorf("WAZA_DIAGNOSTIC_PROFILE_DIR must be absolute"))
	}
	create := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			fail(err)
		}
		return f
	}
	closeFile := func(f *os.File) {
		if err := f.Close(); err != nil {
			fail(err)
		}
	}
	timeline := create("profile-timeline.jsonl")
	encoder := json.NewEncoder(timeline)
	started := time.Now()
	event := func(name string) {
		now := time.Now()
		if err := encoder.Encode(struct {
			Event          string `json:"event"`
			UTC            string `json:"utc"`
			ElapsedNS      int64  `json:"elapsed_ns"`
			MemProfileRate int    `json:"mem_profile_rate"`
		}{name, now.UTC().Format(time.RFC3339Nano), now.Sub(started).Nanoseconds(), runtime.MemProfileRate}); err != nil {
			fail(err)
		}
		if err := timeline.Sync(); err != nil {
			fail(err)
		}
	}
	var cpu *os.File
	window := ""
	startCPU := func(name string) {
		cpu = create("cpu-" + name + ".pprof")
		event("cpu_" + name + "_start_requested")
		if err := pprof.StartCPUProfile(cpu); err != nil {
			fail(err)
		}
		window = name
		event("cpu_" + name + "_start_returned")
	}
	stopCPU := func() {
		event("cpu_" + window + "_stop_requested")
		pprof.StopCPUProfile()
		closeFile(cpu)
		event("cpu_" + window + "_flushed")
		cpu = nil
	}
	snapshot := func(name string) {
		event("allocs_" + name + "_start")
		f := create("allocs-" + name + ".pprof")
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			fail(err)
		}
		closeFile(f)
		event("allocs_" + name + "_flushed")
	}
	event("testmain_enter")
	startCPU("early")
	var mu sync.Mutex
	finished := false
	early := time.AfterFunc(time.Until(started.Add(20*time.Minute)), func() {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		stopCPU()
		snapshot("20m")
		startCPU("late")
	})
	late := time.AfterFunc(time.Until(started.Add(29*time.Minute+45*time.Second)), func() {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		stopCPU()
		snapshot("29m45s")
		finished = true
		event("scheduled_capture_complete_tests_continue")
	})
	event("tests_start")
	code := m.Run()
	early.Stop()
	late.Stop()
	mu.Lock()
	event("tests_returned")
	if !finished {
		stopCPU()
		snapshot("normal-exit")
	}
	finished = true
	event(fmt.Sprintf("testmain_exit_%d", code))
	closeFile(timeline)
	mu.Unlock()
	os.Exit(code)
}
