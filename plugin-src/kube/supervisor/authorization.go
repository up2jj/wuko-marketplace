// Package supervisor implements the worker's local, fail-closed watchdog.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/up2jj/wuko-marketplace/plugin-src/kube/execution"
)

type Authorization struct {
	Owner    string
	Sequence uint64
	Revoked  bool
}
type Poll func(context.Context) (Authorization, error)
type Watchdog struct {
	Owner                      string
	TTL, Interval, MaxDuration time.Duration
	Poll                       Poll
}

// Watch starts immediately. Ready closes only after an increasing sequence has
// been observed; successful repeated reads cannot authorize a delayed Pod.
func (w Watchdog) Watch(ctx context.Context, ready chan<- struct{}) error {
	type sample struct {
		auth Authorization
		err  error
	}
	results := make(chan sample, 1)
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(results)
		ticker := time.NewTicker(w.Interval)
		defer ticker.Stop()
		for {
			auth, err := w.Poll(pollCtx)
			select {
			case results <- sample{auth, err}:
			case <-pollCtx.Done():
				return
			}
			select {
			case <-ticker.C:
			case <-pollCtx.Done():
				return
			}
		}
	}()
	expiry := time.NewTimer(w.TTL)
	defer expiry.Stop()
	maximum := time.NewTimer(w.MaxDuration)
	defer maximum.Stop()
	var baseline uint64
	seen, started := false, false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-maximum.C:
			return fmt.Errorf("maximum duration exceeded")
		case <-expiry.C:
			return fmt.Errorf("authorization expired")
		case result, ok := <-results:
			if !ok {
				return fmt.Errorf("authorization polling ended")
			}
			if result.err != nil {
				continue
			}
			a := result.auth
			if a.Revoked || a.Owner != w.Owner {
				return fmt.Errorf("authorization revoked or owner changed")
			}
			if !seen {
				baseline = a.Sequence
				seen = true
				continue
			}
			if a.Sequence > baseline {
				baseline = a.Sequence
				expiry.Reset(w.TTL)
				if !started {
					started = true
					close(ready)
				}
			}
		}
	}
}
func PollCluster(s *execution.Service, c execution.Control) Poll {
	return func(ctx context.Context) (Authorization, error) {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		l, err := s.Lease(bounded, c.Options.ExecutionID)
		if err != nil {
			return Authorization{}, err
		}
		a := Authorization{Revoked: l.Annotations[execution.Revoked] == "true"}
		if l.Spec.HolderIdentity != nil {
			a.Owner = *l.Spec.HolderIdentity
		}
		a.Sequence, err = strconv.ParseUint(l.Annotations[execution.Sequence], 10, 64)
		if err != nil {
			return a, err
		}
		// Only a definite reservation mismatch revokes; transient read failures
		// are left to the TTL like Lease read failures.
		if err = s.CheckGuard(bounded, c); errors.Is(err, execution.ErrGuardChanged) {
			return Authorization{Revoked: true}, nil
		} else if err != nil {
			return a, err
		}
		return a, nil
	}
}
