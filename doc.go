// Package parleak detects goroutine leaks in tests that use t.Parallel.
//
// The standard goroutine leak detector, uber-go/goleak, says of itself:
//
//	For tests that use t.Parallel, goleak does not know how to distinguish a
//	leaky goroutine from tests that have not finished running.
//
// That is a real gap: t.Parallel is normal, encouraged Go testing practice, so
// the ecosystem's leak detector can't run in the ecosystem's normal testing
// style. parleak closes that gap for the goroutines a test owns.
//
// # How it works
//
// A test launches its goroutines through the package instead of the bare go
// keyword. Tracking is explicit and scoped to one test, so it composes with
// t.Parallel without any attribution guesswork:
//
//	func TestWorker(t *testing.T) {
//		t.Parallel()
//
//		g := parleak.New(t)
//		g.Go("poller", func(ctx context.Context) {
//			poll(ctx) // must return when ctx is done
//		})
//
//		// exercise the system under test...
//
//		// A t.Cleanup registered by New runs here automatically: it cancels
//		// the group context, waits up to the timeout, and fails the test
//		// naming "poller" and its launch site if it's still running.
//	}
//
// New registers the cleanup itself, so a test can't forget to check. Each
// goroutine carries a caller-supplied label and an automatically captured
// launch site (via runtime.Caller), so a failure points at real code. The wait
// is bounded by a default timeout of one second, overridable with WithTimeout.
//
// # Why explicit tracking, not snapshot-and-diff
//
// A tempting alternative is to snapshot runtime.Stack at test start and at
// cleanup, diff the two, and blame this test for the difference. Under
// t.Parallel that approach is unsound: goroutine IDs aren't a public API,
// creation stacks don't give a full ancestry chain, and other parallel tests'
// goroutines appear and vanish inside the window. The result is a flaky leak
// detector, which is worse than none. parleak tracks goroutines explicitly so
// there is no attribution problem to get wrong.
//
// # Panics
//
// A panic in a goroutine started with the bare go keyword crashes the whole
// test process, and the failure is hard to attribute. A goroutine started
// through Group.Go recovers instead: the panic value and stack are captured and
// reported as an ordinary test failure through the test's own Errorf, so the
// process keeps running and the test that owns the goroutine is the one that
// fails. A goroutine that was already reported as leaked, and panics later
// after the test has finished, can no longer fail that test; its panic is still
// recovered to avoid a process crash and is written to standard error.
//
// # Limitation
//
// parleak only sees goroutines started through a Group. It does not catch a
// goroutine leaked deep inside a third-party library a test happens to call —
// goleak does catch those, when it can run at all. The two tools are
// complementary: use goleak in serial tests for leaks anywhere, and use parleak
// in parallel tests for the goroutines the test itself owns.
package parleak
