package parleak_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slepp/parleak"
)

// fakeT is a test double for parleak.TB. It records failures and lets a test
// run the registered cleanups on demand, so the failure path can be exercised
// without failing the enclosing test.
type fakeT struct {
	mu       sync.Mutex
	errors   []string
	cleanups []func()
}

func (f *fakeT) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeT) Cleanup(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, fn)
}

func (f *fakeT) Helper() {}

// runCleanups runs the registered cleanups in LIFO order, matching testing.T.
func (f *fakeT) runCleanups() {
	f.mu.Lock()
	cs := f.cleanups
	f.cleanups = nil
	f.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

func (f *fakeT) failures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.errors...)
}

func (f *fakeT) joined() string {
	return strings.Join(f.failures(), "\n")
}

func TestCleanGoroutinePasses(t *testing.T) {
	t.Parallel()

	ft := &fakeT{}
	g := parleak.New(ft)
	g.Go("responder", func(ctx context.Context) {
		<-ctx.Done() // returns as soon as cleanup cancels the context
	})

	ft.runCleanups()

	if got := ft.failures(); len(got) != 0 {
		t.Fatalf("clean goroutine should not fail the test, got: %v", got)
	}
}

func TestLeakyGoroutineReported(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release) // let the leaked goroutine finish once we're done

	ft := &fakeT{}
	g := parleak.New(ft, parleak.WithTimeout(50*time.Millisecond))
	g.Go("stuck", func(ctx context.Context) {
		<-release // ignores ctx.Done entirely: this is the leak
	})

	ft.runCleanups()

	msg := ft.joined()
	if !strings.Contains(msg, "leaked") {
		t.Fatalf("expected a leak report, got: %q", msg)
	}
	if !strings.Contains(msg, `"stuck"`) {
		t.Fatalf("leak report should name the goroutine label, got: %q", msg)
	}
	if !strings.Contains(msg, "ctx.Done()") {
		t.Fatalf("leak report should point at the likely cause, got: %q", msg)
	}
}

func TestLabelAndLaunchSiteReported(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	ft := &fakeT{}
	g := parleak.New(ft, parleak.WithTimeout(50*time.Millisecond))
	g.Go("named-worker", func(ctx context.Context) {
		<-release
	})

	ft.runCleanups()

	msg := ft.joined()
	if !strings.Contains(msg, `"named-worker"`) {
		t.Fatalf("report should contain the label, got: %q", msg)
	}
	// The launch site is the file that called Go; the line is recorded too.
	if !strings.Contains(msg, "parleak_test.go:") {
		t.Fatalf("report should contain the launch site file:line, got: %q", msg)
	}
	if !strings.Contains(msg, "launched at") {
		t.Fatalf("report should label the launch site, got: %q", msg)
	}
}

func TestWaitTimeoutIsBounded(t *testing.T) {
	t.Parallel()

	// A goroutine that returns comfortably inside the timeout is not a leak.
	ftFast := &fakeT{}
	gFast := parleak.New(ftFast, parleak.WithTimeout(500*time.Millisecond))
	gFast.Go("prompt", func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // small tail, well within the timeout
	})
	ftFast.runCleanups()
	if got := ftFast.failures(); len(got) != 0 {
		t.Fatalf("goroutine that returns within the timeout must pass, got: %v", got)
	}

	// A goroutine that outlives the timeout is reported, and the message states
	// how long it was waited for.
	release := make(chan struct{})
	defer close(release)
	ftSlow := &fakeT{}
	gSlow := parleak.New(ftSlow, parleak.WithTimeout(30*time.Millisecond))
	gSlow.Go("overrunner", func(ctx context.Context) { <-release })

	start := time.Now()
	ftSlow.runCleanups()
	waited := time.Since(start)

	if waited > time.Second {
		t.Fatalf("cleanup should give up near the timeout, waited %s", waited)
	}
	if msg := ftSlow.joined(); !strings.Contains(msg, "30ms") {
		t.Fatalf("report should state the wait duration, got: %q", msg)
	}
}

func TestPanicInGoroutineFailsTestNotProcess(t *testing.T) {
	t.Parallel()

	ft := &fakeT{}
	g := parleak.New(ft)
	g.Go("boom", func(ctx context.Context) {
		panic("kaboom")
	})

	// Give the goroutine a moment to run and panic; cleanup also waits for it.
	ft.runCleanups()

	msg := ft.joined()
	if !strings.Contains(msg, "panicked") {
		t.Fatalf("panic should be reported as a failure, got: %q", msg)
	}
	if !strings.Contains(msg, "kaboom") {
		t.Fatalf("panic report should include the panic value, got: %q", msg)
	}
	if !strings.Contains(msg, `"boom"`) {
		t.Fatalf("panic report should name the goroutine, got: %q", msg)
	}
	// Reaching here at all proves the process didn't crash.
}

// TestParallelNoCrossTestInterference is the headline capability: many tests
// run under t.Parallel at once, each owning goroutines that stay alive across
// the whole parallel window. A snapshot/diff detector would see other tests'
// goroutines in its window and misfire. parleak tracks explicitly, so each
// subtest only ever waits for, and reports on, its own goroutines. Every
// subtest here uses a real *testing.T, so a false leak report would fail it.
func TestParallelNoCrossTestInterference(t *testing.T) {
	t.Parallel()

	for i := 0; i < 16; i++ {
		i := i
		t.Run(fmt.Sprintf("worker-%02d", i), func(t *testing.T) {
			t.Parallel()

			g := parleak.New(t)

			// Each subtest owns two goroutines that live until this subtest's
			// own context is cancelled at cleanup. Their lifetimes overlap
			// every other subtest's goroutines on purpose.
			started := make(chan struct{}, 2)
			g.Go("poller", func(ctx context.Context) {
				started <- struct{}{}
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			})
			g.Go("draining-worker", func(ctx context.Context) {
				started <- struct{}{}
				<-ctx.Done()
			})

			// Make sure both are actually running before the subtest returns,
			// so the parallel windows genuinely overlap.
			<-started
			<-started

			// Stagger completion so subtests finish in a jumbled order.
			time.Sleep(time.Duration(i%5) * time.Millisecond)
		})
	}
}

func TestContextIsCancelledOnCleanup(t *testing.T) {
	t.Parallel()

	ft := &fakeT{}
	g := parleak.New(ft)
	ctx := g.Context()
	if ctx.Err() != nil {
		t.Fatalf("context should be live before cleanup, got %v", ctx.Err())
	}
	ft.runCleanups()
	if ctx.Err() == nil {
		t.Fatalf("context should be cancelled after cleanup")
	}
}

// TestGoAfterCleanupIsNotTracked pins the lifecycle boundary: check seals the
// group and snapshots the tracked goroutines together, so a Go that lands after
// the check has begun is not tracked and cannot produce a spurious late report.
func TestGoAfterCleanupIsNotTracked(t *testing.T) {
	t.Parallel()

	ft := &fakeT{}
	g := parleak.New(ft, parleak.WithTimeout(20*time.Millisecond))

	ft.runCleanups() // seals the group; nothing was tracked yet

	release := make(chan struct{})
	defer close(release)
	g.Go("late", func(ctx context.Context) { <-release }) // ignores ctx on purpose

	// The goroutine is running and will not return, but because it started after
	// the check it is untracked, so no leak is reported.
	time.Sleep(40 * time.Millisecond)
	if got := ft.failures(); len(got) != 0 {
		t.Fatalf("a goroutine started after cleanup must not be tracked or reported, got: %v", got)
	}
}

func TestZeroTimeoutReportsImmediately(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)

	ft := &fakeT{}
	g := parleak.New(ft, parleak.WithTimeout(0))
	g.Go("nowaiter", func(ctx context.Context) { <-release })

	start := time.Now()
	ft.runCleanups()
	if waited := time.Since(start); waited > 200*time.Millisecond {
		t.Fatalf("zero timeout should not wait, waited %s", waited)
	}
	if msg := ft.joined(); !strings.Contains(msg, "leaked") {
		t.Fatalf("expected an immediate leak report, got: %q", msg)
	}
}

// TestDefaultOutputIsBoundedAndDumpIsOptIn guards two related properties found
// the hard way in end-to-end stress runs: a process-wide goroutine dump per
// failing test produced ~1.18MB / 19k lines, most of it other parallel tests'
// goroutines. By default parleak emits only the short per-leak report; the dump
// is opt-in via WithStackDump and, when on, appears exactly once per check.
func TestDefaultOutputIsBoundedAndDumpIsOptIn(t *testing.T) {
	t.Parallel()

	labels := []string{"leak-a", "leak-b", "leak-c"}

	// Default: sharp per-leak reports only, no process-wide dump.
	release1 := make(chan struct{})
	defer close(release1)
	ftDefault := &fakeT{}
	gDefault := parleak.New(ftDefault, parleak.WithTimeout(30*time.Millisecond))
	for _, name := range labels {
		gDefault.Go(name, func(ctx context.Context) { <-release1 })
	}
	ftDefault.runCleanups()

	def := ftDefault.failures()
	if len(def) != len(labels) {
		t.Fatalf("default output should be one report per leak, got %d messages: %v", len(def), def)
	}
	defJoined := strings.Join(def, "\n")
	if strings.Contains(defJoined, "dumping every goroutine") {
		t.Fatalf("default output must not include a process-wide dump:\n%s", defJoined)
	}
	for _, name := range labels {
		if !strings.Contains(defJoined, `"`+name+`"`) {
			t.Fatalf("expected a report naming %q, got: %v", name, def)
		}
	}
	// The 1.18MB / 19k-line regression must not come back: a few leaks stay tiny.
	const ceiling = 4 << 10
	if n := len(defJoined); n > ceiling {
		t.Fatalf("default output for %d leaks was %d bytes, over the %d-byte ceiling", len(labels), n, ceiling)
	}

	// Opt in with WithStackDump: the same sharp reports, plus exactly one dump.
	release2 := make(chan struct{})
	defer close(release2)
	ftDump := &fakeT{}
	gDump := parleak.New(ftDump, parleak.WithTimeout(30*time.Millisecond), parleak.WithStackDump())
	for _, name := range labels {
		gDump.Go(name, func(ctx context.Context) { <-release2 })
	}
	ftDump.runCleanups()

	var leakMsgs, dumpMsgs int
	for _, m := range ftDump.failures() {
		if strings.Contains(m, "leaked: still running") {
			leakMsgs++
			if strings.Contains(m, "dumping every goroutine") {
				t.Fatalf("per-leak report must stay dump-free:\n%s", m)
			}
		}
		if strings.Contains(m, "dumping every goroutine") {
			dumpMsgs++
		}
	}
	if leakMsgs != len(labels) {
		t.Fatalf("want %d leak reports with the option on, got %d", len(labels), leakMsgs)
	}
	if dumpMsgs != 1 {
		t.Fatalf("dump must be emitted exactly once per check, got %d times", dumpMsgs)
	}
}
