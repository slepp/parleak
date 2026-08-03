package parleak_test

import (
	"context"
	"fmt"

	"github.com/slepp/parleak"
)

// exampleTB stands in for *testing.T so these examples are self-contained and
// runnable under `go test`. Real code passes its *testing.T to parleak.New.
type exampleTB struct {
	errors   []string
	cleanups []func()
}

func (e *exampleTB) Errorf(format string, args ...any) {
	e.errors = append(e.errors, fmt.Sprintf(format, args...))
}
func (e *exampleTB) Cleanup(fn func()) { e.cleanups = append(e.cleanups, fn) }
func (e *exampleTB) Helper()           {}

// cleanup runs what testing would run when the test ends.
func (e *exampleTB) cleanup() {
	for i := len(e.cleanups) - 1; i >= 0; i-- {
		e.cleanups[i]()
	}
}

// Example tracks a background poller. The poller returns when the group context
// is cancelled, so cleanup finds no leak.
func Example() {
	t := &exampleTB{} // a real test passes its *testing.T

	g := parleak.New(t)

	readings := make(chan int)
	g.Go("poller", func(ctx context.Context) {
		n := 0
		for {
			select {
			case <-ctx.Done():
				return
			case readings <- n:
				n++
			}
		}
	})

	fmt.Println(<-readings, <-readings, <-readings)

	t.cleanup() // testing runs the leak check here automatically
	fmt.Println("clean:", len(t.errors) == 0)

	// Output:
	// 0 1 2
	// clean: true
}

// ExampleGroup_workerPool tracks a pool of workers draining a job channel. Each
// worker returns when the jobs run out or the context is cancelled.
func ExampleGroup_workerPool() {
	t := &exampleTB{}

	g := parleak.New(t)

	jobs := make(chan int)
	results := make(chan int, 3)

	for w := 0; w < 3; w++ {
		g.Go(fmt.Sprintf("worker-%d", w), func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-jobs:
					if !ok {
						return
					}
					results <- j * j
				}
			}
		})
	}

	for _, j := range []int{2, 3, 4} {
		jobs <- j
	}
	close(jobs)

	sum := 0
	for i := 0; i < 3; i++ {
		sum += <-results
	}
	fmt.Println("sum of squares:", sum)

	t.cleanup()
	fmt.Println("leaks:", len(t.errors))

	// Output:
	// sum of squares: 29
	// leaks: 0
}
