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

// WithStackDump appends a full goroutine dump to a leak report. It's off by
// default: the per-leak report already pins the leak with a label, launch site,
// and likely cause, and the dump is large and mostly noise. The dump contains
// every goroutine in the process, and under t.Parallel most of those belong to
// other tests. parleak deliberately doesn't trim it down to the leaked
// goroutine — singling one out would need goroutine-ID matching, the unsound
// approach this package rejects. Turn it on only when you need to see what a
// leaked goroutine is blocked on.
func WithStackDump() Option {
	return func(c *config) { c.stackDump = true }
}

// Group tracks the goroutines started for a single test and, on cleanup,
// reports any that outlive it. A Group is created with New and is safe for
// concurrent use by multiple goroutines.
type Group struct {
	t      TB
	cfg    config
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	tracked []*tracked
	panics  []panicRecord
	closed  bool
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

// New returns a Group bound to t and registers a t.Cleanup that checks for
// leaks when the test finishes. Because the check is registered here, a test
// can't forget to run it. Options such as WithTimeout tune the check.
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
// goroutine in any failure message, and the call site of Go is captured
// automatically as its launch site.
//
// fn receives the Group's context and is expected to return when that context
// is done. If it doesn't, the cleanup registered by New reports it as a leak.
// A panic in fn is recovered and reported as a test failure rather than
// crashing the process; see the package comment for the exact behavior.
func (g *Group) Go(label string, fn func(ctx context.Context)) {
	_, file, line, _ := runtime.Caller(1)
	tr := &tracked{label: label, file: file, line: line, done: make(chan struct{})}

	g.mu.Lock()
	g.tracked = append(g.tracked, tr)
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
				// The test already finished (this goroutine was reported as
				// leaked). It can no longer be failed, but the panic is
				// recovered so the process survives; note it on stderr.
				fmt.Fprintf(os.Stderr, "parleak: goroutine %q panicked after its test finished: %v\n%s\n",
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

	leaked := g.wait()

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

	// Report each leaked goroutine as its own short, sharp failure. The label
	// and launch site are what actually pin the bug, so this is the default and
	// only output.
	for _, tr := range leaked {
		g.t.Errorf("%s", formatLeak(tr, g.cfg.timeout))
	}

	// A full goroutine dump is opt-in (WithStackDump). It's process-wide and,
	// under t.Parallel, mostly other tests' goroutines, so it's off by default.
	// When on, emit it exactly once per check regardless of how many leaked.
	if g.cfg.stackDump {
		dump := stripReporterFrame(captureStack(true))
		g.t.Errorf("%s", formatDump(len(leaked), dump))
	}
}

// wait waits up to the configured timeout for the tracked goroutines to finish
// and returns those still running. It waits at most one timeout in total, not
// one per goroutine.
func (g *Group) wait() []*tracked {
	g.mu.Lock()
	list := make([]*tracked, len(g.tracked))
	copy(list, g.tracked)
	g.mu.Unlock()

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

// formatDump renders the process-wide goroutine dump that accompanies a leak
// report. It's emitted once per check, not once per leaked goroutine.
func formatDump(n int, dump []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "parleak: %d goroutine(s) leaked; dumping every goroutine in the process below "+
		"(not just the leaked ones — the launch sites above pin those; parleak can't single a "+
		"goroutine out of the dump without unsound goroutine-ID matching):\n", n)
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

// stripReporterFrame drops the leading stack block belonging to parleak's own
// cleanup goroutine — the one that called runtime.Stack — so the dump opens on
// application goroutines rather than library internals. runtime.Stack always
// lists the calling goroutine first. This isn't leak attribution: it only
// removes the reporter's own frame, which is always present and never useful.
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
