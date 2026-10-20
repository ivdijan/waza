//go:build waza_diagnostic_profile

package controlledcomparison

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync"
	"testing"
	"time"
)

// Diagnostic-only instrumentation: sampling perturbs execution. Flush at 20m
// without stopping tests so the unchanged 30m panic cannot lose the profiles.
// CPU samples cover TestMain entry through min(test completion, 20m);
// allocs is a sampled cumulative allocation/retained-heap snapshot at that point.
func TestMain(m *testing.M) {
	dir := os.Getenv("WAZA_DIAGNOSTIC_PROFILE_DIR")
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "diagnostic profiling: %v\n", err)
		os.Exit(2)
	}
	if !filepath.IsAbs(dir) {
		fail(fmt.Errorf("WAZA_DIAGNOSTIC_PROFILE_DIR must be absolute"))
	}
	cpu, err := os.OpenFile(filepath.Join(dir, "cpu.pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fail(err)
	}
	started := time.Now()
	if err := pprof.StartCPUProfile(cpu); err != nil {
		fail(err)
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			pprof.StopCPUProfile()
			if err := cpu.Close(); err != nil {
				fail(err)
			}
			heap, err := os.OpenFile(filepath.Join(dir, "allocs.pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				fail(err)
			}
			writeErr := pprof.Lookup("allocs").WriteTo(heap, 0)
			closeErr := heap.Close()
			if writeErr != nil {
				fail(writeErr)
			}
			if closeErr != nil {
				fail(closeErr)
			}
			window := fmt.Sprintf("start_utc=%s\nend_utc=%s\nelapsed=%s\nmaximum_window=20m\n"+
				"CPU sampling and allocation snapshot perturb execution; tests continue after capture.\n",
				started.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), time.Since(started))
			if err := os.WriteFile(filepath.Join(dir, "profile-window.txt"), []byte(window), 0600); err != nil {
				fail(err)
			}
			fmt.Fprintln(os.Stderr, "diagnostic profiles flushed; tests are not stopped or skipped")
		})
	}
	timer := time.AfterFunc(20*time.Minute, finish)
	code := m.Run()
	timer.Stop()
	finish()
	os.Exit(code)
}
