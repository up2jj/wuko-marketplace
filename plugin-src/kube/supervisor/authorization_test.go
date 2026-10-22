package supervisor

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestWatchdogRequiresFreshHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ready := make(chan struct{})
		w := Watchdog{Owner: "owner", TTL: 15 * time.Second, Interval: time.Second, MaxDuration: time.Hour, Poll: func(context.Context) (Authorization, error) { return Authorization{Owner: "owner", Sequence: 7}, nil }}
		start := time.Now()
		err := w.Watch(t.Context(), ready)
		if err == nil || time.Since(start) != 15*time.Second {
			t.Fatal("stale reads extended authorization", err)
		}
		select {
		case <-ready:
			t.Fatal("started without fresh heartbeat")
		default:
		}
	})
}
func TestWatchdogPartitionAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				n := uint64(0)
				ready := make(chan struct{})
				w := Watchdog{Owner: "owner", TTL: 15 * time.Second, Interval: time.Second, MaxDuration: time.Hour, Poll: func(context.Context) (Authorization, error) {
					n++
					if n > 2 {
						if revoke {
							return Authorization{Owner: "owner", Revoked: true}, nil
						}
						return Authorization{}, fmt.Errorf("API unavailable")
					}
					return Authorization{Owner: "owner", Sequence: n}, nil
				}}
				start := time.Now()
				err := w.Watch(t.Context(), ready)
				if err == nil {
					t.Fatal("did not stop")
				}
				select {
				case <-ready:
				default:
					t.Fatal("fresh heartbeat did not start")
				}
				want := 16 * time.Second
				if revoke {
					want = 2 * time.Second
				}
				if time.Since(start) != want {
					t.Fatalf("stopped after %s want %s", time.Since(start), want)
				}
			})
		})
	}
}
func TestWatchdogBlockedAPIStillExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := Watchdog{Owner: "owner", TTL: 15 * time.Second, Interval: time.Second, MaxDuration: time.Hour, Poll: func(ctx context.Context) (Authorization, error) { <-ctx.Done(); return Authorization{}, ctx.Err() }}
		start := time.Now()
		if w.Watch(t.Context(), make(chan struct{})) == nil || time.Since(start) != 15*time.Second {
			t.Fatal("blocked API disabled watchdog")
		}
	})
}
func TestWatchdogLocalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := uint64(0)
		w := Watchdog{Owner: "owner", TTL: 15 * time.Second, Interval: time.Second, MaxDuration: 5 * time.Second, Poll: func(context.Context) (Authorization, error) {
			n++
			return Authorization{Owner: "owner", Sequence: n}, nil
		}}
		start := time.Now()
		if w.Watch(t.Context(), make(chan struct{})) == nil || time.Since(start) != 5*time.Second {
			t.Fatal("maximum duration ignored")
		}
	})
}
