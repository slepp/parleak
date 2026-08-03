// Package parleak detects goroutine leaks in tests that use t.Parallel.
//
// The standard leak detector, uber-go/goleak, cannot run under t.Parallel. Its
// README says:
//
//	For tests that use t.Parallel, goleak does not know how to distinguish a
//	leaky goroutine from tests that have not finished running.
//
// parleak covers the goroutines a parallel test owns. A test starts its
// goroutines through a Group instead of the bare go keyword, and a t.Cleanup
// registered by New fails the test if any are still running when it ends:
//
//	func TestWorker(t *testing.T) {
//		t.Parallel()
//
//		g := parleak.New(t)
//		g.Go("poller", func(ctx context.Context) {
//			poll(ctx) // must return when ctx is done
//		})
//
//		// exercise the system under test; cleanup cancels the group context,
//		// waits up to the timeout, and names "poller" and its launch site if
//		// it is still running.
//	}
//
// Each goroutine carries a caller-supplied label and a launch site captured
// with runtime.Caller. The wait is bounded by a default timeout of one second,
// overridable with WithTimeout. Tracking is explicit and scoped to one test, so
// there is no attribution guesswork and it composes with t.Parallel.
//
// # Panics
//
// A panic in a goroutine started with the bare go keyword crashes the test
// process. A goroutine started through Group.Go recovers instead: the panic
// value and stack are reported as an ordinary test failure through the owning
// test's Errorf, so the process keeps running. A goroutine already reported as
// leaked that panics later, after the test has finished, can no longer fail
// that test; its panic is still recovered to avoid a crash and is written to
// standard error.
//
// # Limitation
//
// parleak only sees goroutines started through a Group. It does not catch a
// goroutine leaked inside a third-party library a test calls; goleak covers
// those when it can run. Use goleak in serial tests for leaks anywhere, and
// parleak in parallel tests for the goroutines the test itself owns.
package parleak
