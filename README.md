# parleak

parleak reports goroutine leaks for a single test, including tests that use
`t.Parallel()`. A test starts its goroutines through a `Group`. When the test's
cleanup runs, parleak cancels the group's context, waits up to a timeout (one
second by default) for the tracked goroutines to return, and reports any still
running, naming each goroutine and its launch site.

The verdict depends on that timeout. A goroutine that returns within the window
passes even if it ignored cancellation, and a slow but correct shutdown that
exceeds the window is reported. Set the window with `WithTimeout`.

parleak sees only goroutines started through `g.Go`. A goroutine leaked inside a
library you call is invisible to it; see [goleak](#goleak) below.

## Install

```sh
go get github.com/slepp/parleak
```

Standard library only, no dependencies.

## Example

```go
func TestWorker(t *testing.T) {
	t.Parallel()

	g := parleak.New(t)
	g.Go("poller", func(ctx context.Context) {
		poll(ctx) // must return when ctx is done
	})

	// exercise the system under test
}
```

With a real `*testing.T`, the cleanup registered by `New` runs automatically
when the test ends, so the check is not a separate call to remember. If `poller`
is still running after the timeout, the test fails with:

```
parleak: goroutine "poller" leaked: still running 1s after cleanup cancelled the context
	launched at /home/you/project/worker_test.go:42
	likely cause: the goroutine didn't return when ctx.Done() was closed
```

The launch site is the full path reported by `runtime.Caller`, not a base name.

## Options

- `WithTimeout(d)` sets how long cleanup waits, after cancelling the context,
  before reporting a goroutine as leaked (default one second). A non-positive
  duration reports anything not already returned immediately.
- `WithStackDump()` appends the process-wide `runtime.Stack` dump to a failing
  check, once per check. The dump also contains goroutines from other tests
  running in parallel and is not trimmed to the leaked ones; locate a leaked
  goroutine's stack in it by the label and launch site from the per-leak report.
  Use it when the launch site alone is not enough and you need to see where the
  goroutine is blocked. Off by default.

`g.Context()` returns the group's context, the one `g.Go` passes each goroutine.
Use it to build the system under test so cancellation reaches shared state:
`newServer(g.Context())`.

## Exposing goroutines to parleak

To track a goroutine, start it through `g.Go`. A goroutine a library spawns
internally with a bare `go` is invisible, so expose its body as a plain
`func(context.Context)` work function and let the test start it:

```go
// Expose the body as a work function instead of a bare `go` inside Start.
func (w *Watcher) Run(ctx context.Context, out chan<- string) {
	// ... loop; returns when ctx is done ...
}

out := make(chan string)
g.Go("watcher", func(ctx context.Context) { w.Run(ctx, out) })
```

Production code never imports parleak; the refactor only exposes a work
function. A function already shaped `func(context.Context)` needs no closure:
`g.Go("poller", w.Poll)`.

## goleak

[`goleak`](https://github.com/uber-go/goleak) takes a process-wide stack
snapshot and filters it against a set of known-safe goroutines. It catches leaks
anywhere, including inside a dependency, and for parallel suites it recommends
`goleak.VerifyTestMain`, which runs the check once after a package's tests
finish.

The two tools check different things. goleak's per-test `VerifyNone` cannot
attribute a goroutine to one test when tests run under `t.Parallel()`, because a
process-wide snapshot cannot tell which parallel test a goroutine belongs to;
goleak's own README notes this. parleak records the goroutines each test starts
through `g.Go`, so it attributes every tracked goroutine to the test that
started it and works per test under `t.Parallel()`. In exchange it sees only
what goes through `g.Go`; a goroutine a dependency starts on its own is invisible
to it, just as goleak cannot attribute such a goroutine to a particular test.

Use goleak (via `VerifyTestMain`, or `VerifyNone` in serial tests) for leaks
anywhere in a package. Use parleak for per-test checks of the goroutines a
parallel test owns.

## Panics

A goroutine started with a bare `go` that panics crashes the test process. One
started with `g.Go` recovers: the panic value and stack are reported as a failure
of the owning test, and the process keeps running. If a goroutine panics after
the group's check has already recorded its verdict (for example, one already
reported as leaked that panics later), it can no longer fail the test; the panic
is still recovered so the process survives, and it is written to stderr.

## API

```go
func New(t TB, opts ...Option) *Group
func (g *Group) Go(label string, fn func(ctx context.Context))
func (g *Group) Context() context.Context

func WithTimeout(d time.Duration) Option   // default 1s
func WithStackDump() Option                // off by default

type TB interface {                        // *testing.T and *testing.B satisfy it
	Errorf(format string, args ...any)
	Cleanup(func())
	Helper()
}
```

All calls to `g.Go` must happen before the test's cleanup runs. `g.Go` is safe
for concurrent use while the test runs; a goroutine started after the cleanup
check has begun is not tracked. The cleanup timeout is a single bound on the
whole wait, not per goroutine, so fifty leaked goroutines still finish the check
in about one timeout. `TB` lets parleak's own tests drive the failure path with
a double.

## Design notes

parleak tracks goroutines explicitly rather than diffing `runtime.Stack`
snapshots at test start and end: goroutine IDs are not a public API, creation
stacks lack a full ancestry chain, and other parallel tests' goroutines drift
through any snapshot window. Explicit tracking lets the test state which
goroutines are its own.

It has no `TestMain` or package-level mode; tracking is per test so it composes
with `t.Parallel()`. The group derives its context from `context.Background()`;
a parent-context option could be added later without changing the surface.

## License

Dual-licensed under [Apache-2.0](LICENSE-APACHE) or [MIT](LICENSE-MIT) at your
option. Contributions are accepted under the same dual license.
