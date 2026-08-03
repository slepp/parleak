# parleak

`parleak` catches goroutine leaks in tests that use `t.Parallel()`. A test
starts its goroutines through a `Group`; when the test ends, parleak fails it if
any are still running, naming each goroutine and its launch line.

parleak only sees goroutines started through `g.Go`. It does not inspect the
whole runtime, so it cannot catch a goroutine leaked inside a library you call;
that is [`goleak`](https://github.com/uber-go/goleak)'s job. The two are
complementary (see [goleak](#goleak)).

## The problem

`t.Parallel()` is normal Go testing practice, but `uber-go/goleak` cannot run
under it. From goleak's own README
([Note](https://github.com/uber-go/goleak/blob/master/README.md#note)):

> For tests that use [t.Parallel](https://pkg.go.dev/testing#T.Parallel), `goleak` does
> not know how to distinguish a leaky goroutine from tests that have not finished running.

parleak covers the goroutines a parallel test owns by tracking only the ones a
test explicitly starts, so there is no attribution to guess.

## Install

```sh
go get github.com/slepp/parleak
```

Standard library only, no dependencies.

## Example

```go
func TestWorker(t *testing.T) {
	t.Parallel()

	g := parleak.New(t)             // registers t.Cleanup
	g.Go("poller", func(ctx context.Context) {
		poll(ctx)                   // must return when ctx is done
	})

	// Cleanup cancels the group context, waits up to a second, and fails the
	// test if "poller" is still running:
	//
	//   parleak: goroutine "poller" leaked: still running 1s after cleanup
	//   cancelled the context
	//       launched at worker_test.go:42
}
```

`New(t)` registers the cleanup check, so a test that calls `New` cannot skip it.
Each goroutine carries its label and a launch site from `runtime.Caller`, the
default output.

Two options:

- `WithTimeout(d)` changes how long cleanup waits before reporting a leak
  (default one second).
- `WithStackDump()` appends a full goroutine dump to the report. It is off by
  default: the dump is process-wide, so under `t.Parallel()` most of it belongs
  to other tests, and parleak won't filter it to the leaked goroutine.

`g.Context()` returns the group's context, the same one `g.Go` passes each
goroutine. Use it to build the system under test before launching goroutines, so
cancellation reaches shared state: `newServer(g.Context())`.

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

## Why explicit tracking

Snapshotting `runtime.Stack` and diffing test start against cleanup is unsound
under `t.Parallel()`: goroutine IDs aren't a public API, creation stacks lack a
full ancestry chain, and other parallel tests' goroutines drift through the
window. Explicit tracking sidesteps attribution: the test states which
goroutines are its own.

## Panics

A goroutine started with a bare `go` that panics crashes the test process.
Started with `g.Go` it recovers: the panic value and stack are reported as an
ordinary failure of the owning test, and the process keeps running. A goroutine
that panics after being reported as leaked, once its test has finished, can no
longer fail it; the panic is still recovered so the process survives, and it is
written to stderr.

## goleak

`goleak` and parleak cover different leaks:

| | serial tests | `t.Parallel()` tests |
|---|---|---|
| leaks anywhere, including inside libraries you call | `goleak` | |
| leaks in goroutines your test starts | `goleak` or `parleak` | `parleak` |

`goleak` sees every goroutine, including ones leaked inside a dependency, and is
the right tool whenever it can run. Use `parleak` for parallel tests.

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

The cleanup timeout is a total bound rather than per goroutine: fifty leaked
goroutines still finish in about a second. `TB` lets parleak's own tests drive
the failure path with a double.

## Not in scope

- Catching leaks in goroutines the test didn't start; that needs whole-runtime
  inspection, which is `goleak`'s job.
- A `TestMain`/package-level mode. Tracking is per-test so it composes with
  `t.Parallel()`.
- Goroutine-ID or stack diffing (see [Why explicit tracking](#why-explicit-tracking)).
- A parent-context option; the group derives from `context.Background()` and can
  gain one later without breaking the surface.

## License

Dual-licensed under [Apache-2.0](LICENSE-APACHE) or [MIT](LICENSE-MIT) at your
option. Contributions are accepted under the same dual license.
