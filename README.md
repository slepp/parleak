# parleak

`parleak` catches goroutine leaks in tests that use `t.Parallel()`. It tracks
the goroutines a test starts and, when the test ends, fails it if any are still
running — naming the goroutine and the line it was launched from.

**Scope, up front:** parleak only sees goroutines you start through it. It does
not watch the whole runtime, so it won't catch a goroutine leaked inside a
library you call — that's [`goleak`](https://github.com/uber-go/goleak)'s job.
The two are [complementary](#complementary-to-goleak-not-a-replacement), and the
[limitation](#the-honest-limitation) is spelled out below.

## The problem

`t.Parallel()` is normal, encouraged Go testing practice. The standard leak
detector, [`uber-go/goleak`](https://github.com/uber-go/goleak), can't run under
it. From goleak's own README
([Note](https://github.com/uber-go/goleak/blob/master/README.md#note)):

> For tests that use [t.Parallel](https://pkg.go.dev/testing#T.Parallel), `goleak` does
> not know how to distinguish a leaky goroutine from tests that have not finished running.

goleak's example even labels the spot: `t.Parallel() // <- goleak gets confused here!`

So the ecosystem's leak detector doesn't work in the ecosystem's normal testing
style. That's the gap `parleak` fills.

A goroutine that ignores its context is the bug this catches:

```go
func TestWorker(t *testing.T) {
	t.Parallel()

	stop := make(chan struct{})
	go func() {
		<-stop // never closed; this goroutine outlives the test
		poll()
	}()

	// test passes, the goroutine leaks, and nothing tells you
}
```

## Install

```sh
go get github.com/slepp/parleak
```

Standard library only, no dependencies.

## 60-second example

Launch goroutines through the group. Everything else is automatic.

```go
func TestWorker(t *testing.T) {
	t.Parallel()

	g := parleak.New(t)             // registers t.Cleanup itself
	g.Go("poller", func(ctx context.Context) {
		poll(ctx)                   // must return when ctx is done
	})

	// exercise the system under test...

	// Cleanup runs here automatically: it cancels the group context, waits up
	// to a second, and if "poller" is still running it fails the test with:
	//
	//   parleak: goroutine "poller" leaked: still running 1s after cleanup
	//   cancelled the context
	//       launched at worker_test.go:42
	//       likely cause: the goroutine didn't return when ctx.Done() was closed
}
```

`New(t)` registers the check, so a test can't forget it. Each goroutine gets a
label and an automatically captured launch site (`runtime.Caller`), so the
failure points at real code — that label plus launch line is what pins the leak,
and it's the whole default output.

Two options tune it:

- `parleak.New(t, parleak.WithTimeout(2*time.Second))` changes how long cleanup
  waits before calling a goroutine leaked (default one second).
- `parleak.New(t, parleak.WithStackDump())` appends a full goroutine dump to a
  leak report, for when you need to see what a goroutine is blocked on. It's
  off by default because the dump is process-wide — under `t.Parallel()` most of
  it is *other* tests' goroutines — and parleak deliberately won't filter it
  down to the leaked goroutine (that needs goroutine-ID matching, the unsound
  approach [it rejects](#why-explicit-tracking)).

`g.Context()` returns the group's context — the same one `g.Go` hands each
goroutine. It's rarely needed; reach for it when you must build the system under
test *before* launching goroutines, so the same cancellation reaches shared
state the goroutines depend on:

```go
g := parleak.New(t)
srv := newServer(g.Context()) // shares the group's cancellation
g.Go("reader", srv.readLoop)
```

## Refactoring your code for parleak

parleak only sees goroutines started through `g.Go` — "launch through the group"
is a requirement, not a suggestion. A goroutine a library spawns internally with
a bare `go` is invisible to it. To make one visible, expose its body as a plain
work function and let the test start it:

```go
// BEFORE — the goroutine is hidden inside Start, so parleak never sees it.
func (w *Watcher) Start(ctx context.Context) <-chan string {
	out := make(chan string)
	go w.Run(ctx, out) // bare go — invisible to parleak
	return out
}

// AFTER — expose the goroutine body as a plain work function.
func (w *Watcher) Run(ctx context.Context, out chan<- string) {
	// ... same loop; returns when ctx is done ...
}

// In the test, the test starts it and owns its lifecycle:
out := make(chan string)
g.Go("watcher", func(ctx context.Context) { w.Run(ctx, out) })
```

(The closure is only needed to bind the extra `out` argument. A work function
that already has the shape `func(context.Context)` — say `w.Poll` — goes
straight in: `g.Go("poller", w.Poll)`.)

This is good design on its own merits: it separates *what to do* (the library's
`Run` method) from *how to run it* (the test's `g.Go` call), and the test
explicitly owns the goroutine's lifecycle. **Production code never imports
parleak** — the refactor only exposes a work function, so no test dependency
bleeds into the library.

The cost is real, though: a mature library that spawns goroutines internally has
to be restructured to expose those work functions. Each change is small, but on
an existing codebase there can be many. Weigh it like the goleak trade-off below
— for goroutines a test owns and can start itself, the visibility is worth it;
for anything spawned deep inside code you don't control, reach for goleak.

## Why explicit tracking

The obvious approach — snapshot `runtime.Stack` at the start and at cleanup,
diff them, blame this test — is a trap under `t.Parallel()`. Goroutine IDs
aren't a public API, creation stacks don't give a full ancestry chain, and other
parallel tests' goroutines drift in and out of the window. That makes a flaky
leak detector, which is worse than none.

`parleak` sidesteps all of it: the test says which goroutines are its own, so
there's no attribution to get wrong. That's what makes it safe under
`t.Parallel()`.

## Panics

A goroutine started with the bare `go` keyword that panics takes the whole test
process down, and the crash is hard to pin on a test. A goroutine started with
`g.Go` recovers instead: the panic value and stack are reported as an ordinary
failure of the test that owns it, and the process keeps running. (A goroutine
already reported as leaked that panics later, after its test has finished, can't
fail that test anymore; its panic is still recovered so the process survives,
and it's written to stderr.)

## Complementary to goleak, not a replacement

Use both. They cover different leaks:

| | serial tests | `t.Parallel()` tests |
|---|---|---|
| leaks anywhere, including inside libraries you call | `goleak` | — |
| leaks in goroutines your test starts | `goleak` or `parleak` | `parleak` |

`goleak` is the right tool whenever it can run, and it catches more: it sees
every goroutine, including ones leaked deep inside a dependency. Reach for
`parleak` for the case `goleak` names as its own limitation — parallel tests —
and for the goroutines those tests own.

## The honest limitation

`parleak` only sees goroutines started through a `Group`. It will not catch a
goroutine leaked inside a third-party library a test happens to call — it never
saw that `go` statement. `goleak` does catch those. If a test's own goroutines
all go through `g.Go`, `parleak` covers them; anything else is out of its view
by design.

## Not in scope

- **Catching leaks you didn't start through the group.** That needs whole-runtime
  inspection, which is `goleak`'s job and is unsound under `t.Parallel()`.
- **A `TestMain`/package-level mode.** Tracking is per-test on purpose; that's
  what composes with `t.Parallel()`.
- **Goroutine-ID or stack diffing.** Rejected deliberately — see
  [Why explicit tracking](#why-explicit-tracking).
- **A parent-context option.** The group derives from `context.Background()`. If
  this proves necessary it can be added without breaking the surface.

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted
for inclusion in the work by you, as defined in the Apache-2.0 license, shall be
dual licensed as above, without any additional terms or conditions.
