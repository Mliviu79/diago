// Package serving runs a Diago for the examples: its listeners and its request run.
package serving

import (
	"context"
	"errors"
	"sync"

	"github.com/emiago/diago"
)

// Run registers f and runs the listeners and the request run of dg until ctx
// ends or either of them returns, and joins both before it returns.
func Run(ctx context.Context, dg *diago.Diago, f diago.ServeDialogFunc) error {
	dg.HandleFunc(f)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		errs <- dg.ServeRequests(ctx, nil)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		errs <- dg.Serve(ctx)
	}()
	wg.Wait()
	return errors.Join(<-errs, <-errs)
}
