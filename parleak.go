package parleak

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// defaultTimeout is how long a Group waits for its goroutines to return after
// cancelling the group context, unless WithTimeout overrides it.
const defaultTimeout = time.Second

// TB is the subset of *testing.T (and *testing.B) that a Group needs. Depending
// on this interface rather than the concrete type lets a Group be driven by a
// test double, so the failure path can be asserted on without failing a real
// test. *testing.T and *testing.B satisfy it.
type TB interface {
	Errorf(format string, args ...any)
	Cleanup(func())
	Helper()
}

// Option configures a Group at construction time.
type Option func(*config)

type config struct {
	timeout   time.Duration
	stackDump bool
}

// WithTimeout sets how long a Group waits, after cancelling its context, for a
// tracked goroutine to return before reporting it as leaked. The default is one
// second. A non-positive duration means don't wait at all: a goroutine that
// hasn't already returned is reported immediately.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithStackDump appends a process-wide goroutine dump to the leak reports, once
// per check. The dump is the output of [runtime.Stack], stripped of parleak's
// own reporting frame and indented; it contains goroutines from other tests
// running in parallel and is not trimmed to the leaked ones. Every goroutine
// parleak started ends its block with "created by
// github.com/slepp/parleak.(*Group).Go", so search for that string to find them
// all; within a block the top frame is where the goroutine is blocked, and the
// function passed to Go sits directly above the parleak.(*Group).Go.func1
// wrapper frame. The label does not appear in the dump.
func WithStackDump() Option {
	return func(c *config) { c.stackDump = true }
}

// Group tracks the goroutines a single test starts through Go and, when the
// test's cleanup runs, reports any still running after the group's context is
// cancelled and a timeout elapses. Create a Group with New.
//
// A Group is safe for concurrent use, and Go may be called from any goroutine
// while the test runs. A goroutine started after the cleanup check has begun is
// not included in leak detection.
type Group struct {
	t      TB
	cfg    config
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	tracked []*tracked
	panics  []panicRecord
	sealed  bool // set when check snapshots tracked; no later goroutine is tracked
	closed  bool // set when check has recorded its verdict; later panics go to stderr
}

type tracked struct {
	label string
	file  string
	line  int
	done  chan struct{}
}

type panicRecord struct {
	g     *tracked
	value any
	stack []byte
}

// New returns a Group bound to t and registers a t.Cleanup that runs the leak
// check when the test ends. With a real *testing.T the cleanup runs
// automatically, so the check is not a separate call to remember. Options such
// as WithTimeout configure the check.
//
//	func TestWorker(t *testing.T) {
//		t.Parallel()
//
//		g := parleak.New(t)
//		g.Go("poller", func(ctx context.Context) {
//			poll(ctx) // must return when ctx is done
//		})
//
//		// exercise the system under test; the registered cleanup then cancels
//		// the context and reports "poller" if it is still running.
//	}
func New(t TB, opts ...Option) *Group {
	cfg := config{timeout: defaultTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &Group{t: t, cfg: cfg, ctx: ctx, cancel: cancel}
	t.Cleanup(g.check)
	return g
}

// Context returns the Group's context. It's cancelled when the test's cleanup
// runs. Pass it to the system under test so cancellation reaches code that a
// tracked goroutine depends on.
func (g *Group) Context() context.Context { return g.ctx }

// Go starts fn in a new goroutine tracked by the Group. The label names the
// goroutine in a failure message, and Go records its own call site as the
// launch site.
//
// fn receives the Group's context and is expected to return when that context
// is done. If it does not, the cleanup registered by New reports it as a leak.
//
// A panic in fn is recovered rather than crashing the process. If it happens
// before the group's leak check records its verdict, it fails the owning test.
// A later panic can no longer fail the test and is written to standard error.
func (g *Group) Go(label string, fn func(ctx context.Context)) {
	_, file, line, _ := runtime.Caller(1)
	tr := &tracked{label: label, file: file, line: line, done: make(chan struct{})}

	g.mu.Lock()
	if !g.sealed {
		g.tracked = append(g.tracked, tr)
	}
	g.mu.Unlock()

	go func() {
		defer close(tr.done)
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			stack := captureStack(false)
			g.mu.Lock()
			g.panics = append(g.panics, panicRecord{g: tr, value: r, stack: stack})
			reportedLate := g.closed
			g.mu.Unlock()
			if reportedLate {
				// The check already recorded its verdict, so this panic can no
				// longer fail the test. It is recovered and noted on stderr.
				fmt.Fprintf(os.Stderr, "parleak: goroutine %q panicked after its group's leak check finished: %v\n%s\n",
					tr.label, r, stack)
			}
		}()
		fn(g.ctx)
	}()
}

// check is registered as a t.Cleanup by New. It cancels the context, waits for
// the tracked goroutines to return, and reports panics and leaks.
func (g *Group) check() {
	g.t.Helper()
	g.cancel()

	// Seal and snapshot under one lock, so a Go racing with cleanup either lands
	// before the seal and is tracked, or after and is not.
	g.mu.Lock()
	g.sealed = true
	list := make([]*tracked, len(g.tracked))
	copy(list, g.tracked)
	g.mu.Unlock()

	leaked := g.wait(list)

	g.mu.Lock()
	g.closed = true
	panics := g.panics
	g.panics = nil
	g.mu.Unlock()

	for _, p := range panics {
		g.t.Errorf("%s", formatPanic(p))
	}

	if len(leaked) == 0 {
		return
	}

	for _, tr := range leaked {
		g.t.Errorf("%s", formatLeak(tr, g.cfg.timeout))
	}

	if g.cfg.stackDump {
		dump := stripReporterFrame(captureStack(true))
		g.t.Errorf("%s", formatDump(len(leaked), dump))
	}
}

// wait waits up to the configured timeout for the goroutines in list to return
// and returns those still running. The timeout bounds the whole wait, not each
// goroutine.
func (g *Group) wait(list []*tracked) []*tracked {
	if len(list) == 0 {
		return nil
	}

	timer := time.NewTimer(g.cfg.timeout)
	defer timer.Stop()

	for i, tr := range list {
		select {
		case <-tr.done:
			// returned in time
		case <-timer.C:
			// Deadline hit. This goroutine and any later ones that aren't
			// already done are leaks.
			var leaked []*tracked
			for _, rest := range list[i:] {
				select {
				case <-rest.done:
				default:
					leaked = append(leaked, rest)
				}
			}
			return leaked
		}
	}
	return nil
}

func formatLeak(tr *tracked, timeout time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "parleak: goroutine %q leaked: still running %s after cleanup cancelled the context\n",
		tr.label, timeout)
	fmt.Fprintf(&b, "\tlaunched at %s:%d\n", tr.file, tr.line)
	fmt.Fprintf(&b, "\tlikely cause: the goroutine didn't return when ctx.Done() was closed")
	return b.String()
}

// formatDump renders the process-wide goroutine dump, once per check.
func formatDump(n int, dump []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "parleak: %d goroutine(s) leaked; dumping every goroutine in the process, "+
		"including goroutines from other tests:\n", n)
	b.WriteString(indent(string(dump)))
	return b.String()
}

func formatPanic(p panicRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "parleak: goroutine %q panicked: %v\n", p.g.label, p.value)
	fmt.Fprintf(&b, "\tlaunched at %s:%d\n", p.g.file, p.g.line)
	if len(p.stack) > 0 {
		b.WriteString("\tstack:\n")
		b.WriteString(indent(string(p.stack)))
	}
	return b.String()
}

// captureStack returns a formatted stack trace. When all is true it dumps every
// goroutine; otherwise just the calling one.
func captureStack(all bool) []byte {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, all)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// stripReporterFrame drops the leading stack block, which belongs to parleak's
// own goroutine calling runtime.Stack. runtime.Stack lists the calling
// goroutine first.
func stripReporterFrame(dump []byte) []byte {
	const sep = "\n\n"
	s := string(dump)
	i := strings.Index(s, sep)
	if i < 0 {
		return dump
	}
	if first := s[:i]; strings.Contains(first, "parleak.(*Group).check") ||
		strings.Contains(first, "parleak.captureStack") {
		return []byte(s[i+len(sep):])
	}
	return dump
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = "\t\t" + ln
	}
	return strings.Join(lines, "\n") + "\n"
}
