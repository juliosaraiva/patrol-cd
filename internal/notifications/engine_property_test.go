package notifications

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

// flakyService fails while fail is set; it is driven by a single goroutine.
type flakyService struct {
	fail bool
	sent int
}

func (f *flakyService) Send(context.Context, services.Notification, services.Destination) error {
	if f.fail {
		return errors.New("remote unavailable")
	}
	f.sent++
	return nil
}

// TestProcessDeliverySemanticsModel checks Process against a reference model over random
// sequences of application states and transient delivery failures:
//
//   - without oncePer, a notification is sent whenever the condition is true and it was
//     not delivered since the condition last became true (false -> true re-arms);
//   - with oncePer, a notification is sent once per oncePer value, whatever the condition
//     does in between;
//   - a failed delivery is not recorded, so it is attempted again on the next evaluation;
//   - Process is deterministic: re-processing the same input with the returned state sends
//     nothing new while the condition stays true.
func TestProcessDeliverySemanticsModel(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		"service.slack":     "token: xoxb-test",
		"template.m":        "message: '{{.app.metadata.name}}'",
		"trigger.plain":     "- when: app.metadata.labels.on == 'true'\n  send: [m]",
		"trigger.per-value": "- when: app.metadata.labels.on == 'true'\n  oncePer: app.metadata.labels.rev\n  send: [m]",
	}
	rapid.Check(t, func(rt *rapid.T) {
		trigger := rapid.SampledFrom([]string{"plain", "per-value"}).Draw(rt, "trigger")
		svc := &flakyService{}
		cfg, err := ParseConfig(cm, nil)
		if err != nil {
			rt.Fatal(err)
		}
		eng, err := NewEngine(cfg, WithClock(fixedClock), WithServices(map[string]services.Service{"slack": svc}))
		if err != nil {
			rt.Fatal(err)
		}

		var (
			state     DeliveryState
			armed     = true              // plain: the next true evaluation sends
			delivered = map[string]bool{} // per-value: oncePer values already delivered
			wantSent  int
		)
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			on := rapid.Bool().Draw(rt, "on")
			rev := rapid.SampledFrom([]string{"r1", "r2", "r3"}).Draw(rt, "rev")
			svc.fail = rapid.IntRange(0, 4).Draw(rt, "failRoll") == 0
			app := appFixture(
				withAnnotation(testPrefix+"/subscribe."+trigger+".slack", "chan"),
				func(app map[string]any) {
					setPath(app, map[string]any{"on": boolString(on), "rev": rev}, "metadata", "labels")
				},
			)

			// Reference model.
			shouldAttempt := false
			switch trigger {
			case "plain":
				if !on {
					armed = true
				} else if armed {
					shouldAttempt = true
				}
			case "per-value":
				shouldAttempt = on && !delivered[rev]
			}
			success := shouldAttempt && !svc.fail
			if success {
				wantSent++
				armed = false
				delivered[rev] = true
			}

			before := svc.sent
			newState, res, err := eng.Process(context.Background(), app, state)
			if err != nil {
				rt.Fatalf("step %d: Process: %v", i, err)
			}
			attempted := 0
			for _, r := range res {
				switch r.Status {
				case StatusSent, StatusFailed:
					attempted++
				}
			}
			if shouldAttempt != (attempted == 1) || attempted > 1 {
				rt.Fatalf("step %d (on=%v rev=%s fail=%v): attempted=%d, model expects attempt=%v; results %s",
					i, on, rev, svc.fail, attempted, shouldAttempt, statuses(res))
			}
			if got := svc.sent - before; (got == 1) != success {
				rt.Fatalf("step %d: sent %d, model success=%v", i, got, success)
			}
			state = newState

			// Idempotency: same input, healthy service, state from this step.
			if success {
				svc.fail = false
				again, res2, err := eng.Process(context.Background(), app, state)
				if err != nil {
					rt.Fatal(err)
				}
				for _, r := range res2 {
					if r.Status != StatusSkippedDuplicate {
						rt.Fatalf("step %d: re-processing produced %s", i, statuses(res2))
					}
				}
				if len(again) != len(state) {
					rt.Fatalf("step %d: re-processing changed the state %v -> %v", i, state, again)
				}
			}
		}
		if svc.sent != wantSent {
			rt.Fatalf("sent %d notifications, model expects %d", svc.sent, wantSent)
		}
	})
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
