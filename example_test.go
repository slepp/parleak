package parleak_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slepp/parleak"
)

// fakeTB stands in for *testing.T. An example function does not receive a
// *testing.T, so this records failures instead of failing, which lets the
// example print what parleak reported. Real tests pass their own *testing.T to
// parleak.New; see the New documentation for that form.
type fakeTB struct {
	errors   []string
	cleanups []func()
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Helper()           {}

// runCleanups runs the registered cleanups in LIFO order, as testing does when
// a test ends.
func (f *fakeTB) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// Example_leak shows what parleak reports when a tracked goroutine ignores
// cancellation and is still running when the test's cleanup checks it.
func Example_leak() {
	release := make(chan struct{})
	defer close(release)

	t := &fakeTB{}
	g := parleak.New(t, parleak.WithTimeout(10*time.Millisecond))
	g.Go("poller", func(ctx context.Context) {
		<-release // never checks ctx.Done, so it outlives the test
	})

	t.runCleanups() // testing runs the registered cleanup when a test ends

	// The report also names the launch site (file:line from runtime.Caller) and
	// the likely cause; only the first line is stable enough to print here.
	fmt.Println(strings.SplitN(t.errors[0], "\n", 2)[0])

	// Output:
	// parleak: goroutine "poller" leaked: still running 10ms after cleanup cancelled the context
}
