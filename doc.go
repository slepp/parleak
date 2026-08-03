// Package parleak reports goroutine leaks for a single test, including tests
// that run under t.Parallel.
//
// A test starts its goroutines through a [Group] instead of the go keyword. Each
// goroutine carries a caller-supplied label and the launch site recorded by
// [Group.Go]. [New] registers a t.Cleanup that cancels the group's context,
// waits for the tracked goroutines to return, and reports any still running as
// leaks. The wait is bounded by a timeout, one second by default and set with
// [WithTimeout], so the verdict depends on it: a goroutine that returns within
// the window passes even if it ignored cancellation, and a slow but correct
// shutdown that exceeds the window is reported.
//
// Tracking is explicit and scoped to one test, so parleak attributes each
// goroutine to the test that started it, which is what lets it run under
// t.Parallel. It sees only goroutines started through a [Group]; a goroutine
// leaked inside a library the test calls is invisible to it. go.uber.org/goleak
// takes a process-wide stack snapshot and can catch those, and for parallel
// suites it recommends goleak.VerifyTestMain, which checks once per package.
// A process-wide snapshot cannot attribute a goroutine to one test among
// several running under t.Parallel, which is the case parleak covers.
//
// # Panics
//
// A goroutine started with the go keyword crashes the process if it panics. One
// started through [Group.Go] recovers instead and reports the panic as a failure
// of the owning test. If the panic arrives after the group's check has already
// recorded its verdict (for example, a goroutine already reported as leaked that
// panics later), it can no longer fail the test; it is still recovered and is
// written to standard error.
package parleak
