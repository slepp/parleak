// Package parleak reports the goroutines a single test leaks, including tests
// that run under t.Parallel.
//
// A test starts its goroutines through a [Group] instead of the go keyword.
// [New] binds a Group to a test and registers a t.Cleanup that cancels the
// group's context, waits up to a timeout for the tracked goroutines to return,
// and reports any still running. A Group reports only the goroutines started
// through it, so the check runs per test even under t.Parallel.
package parleak
