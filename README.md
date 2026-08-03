# parleak

parleak tracks the goroutines a test starts through `g.Go`, and after the test
cancels the context it reports any that are still running. It works under
`t.Parallel()`.

## Install

```sh
go get github.com/slepp/parleak
```

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

`New` registers a `t.Cleanup`, so the check runs when the test ends. It cancels
the group's context, waits up to a timeout (one second by default, set with
`WithTimeout`), and fails the test for each goroutine still running. The first
line of a report reads:

```
parleak: goroutine "poller" leaked: still running 1s after cleanup cancelled the context
```

parleak indents the rest: the launch site, which is the file string returned by
`runtime.Caller`, and the likely cause; `t.Errorf` adds only its own file:line
prefix. `WithStackDump` adds the process-wide `runtime.Stack` output to the leak
reports, stripped of parleak's reporting frame and indented. Every goroutine
parleak started ends its block with `created by
github.com/slepp/parleak.(*Group).Go`, so search the dump for that string; the
top frame is where the goroutine is blocked, and the function passed to `Go`
sits directly above the `parleak.(*Group).Go.func1` wrapper frame.

Pass `g.Context()` to the system under test so cancellation reaches shared
state.

A Group reports only goroutines started through `g.Go`. To track one a library
spawns with a bare `go`, expose its body as a `func(context.Context)` and start
it from the test; production code never imports parleak. A goroutine a
dependency leaks on its own is invisible to parleak.
[goleak](https://github.com/uber-go/goleak) inspects a process-wide snapshot and
can catch those, though under `t.Parallel()` it cannot attribute a goroutine to
one test.

See the [package documentation](https://pkg.go.dev/github.com/slepp/parleak) for
the full API.

## License

Dual-licensed under [Apache-2.0](LICENSE-APACHE) or [MIT](LICENSE-MIT) at your
option. Contributions are accepted under the same dual license.
