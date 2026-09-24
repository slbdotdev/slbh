package harness

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slbdotdev/slbh/internal/config"
	"github.com/slbdotdev/slbh/internal/provider"
)

// TestAgentTurnRefusesAnUnroutedModelWithoutARequest runs a real Seat turn on
// the runtime's own provider factory, not a test double, and proves the
// fail-closed routing contract end to end: a model the policy in force does
// not describe ends the turn with the policy's refusal, and nothing is sent.
// The provider package proves ResolveRoute refuses; this proves the agent loop
// takes that path and reports it rather than falling through to a request.
func TestAgentTurnRefusesAnUnroutedModelWithoutARequest(t *testing.T) {
	// No credential, so even a broken refusal could not reach a real service.
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("ZAI_API_KEY", "")
	for _, tc := range []struct {
		name   string
		policy provider.Policy
		reason string
	}{
		{"no policy", provider.Policy{}, "no routing policy is in force"},
		{"unlisted route", provider.Policy{Version: provider.PolicyVersion, Routes: map[string]provider.RoutePolicy{
			"vendor/listed": {Endpoint: provider.OpenRouterEndpoint, Wire: provider.WireOpenAIChat},
		}}, "has no entry in the routing policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				http.Error(w, "unexpected request", http.StatusTeapot)
			}))
			defer server.Close()

			r, err := New(config.Config{
				Home:             t.TempDir(),
				SeatModel:        "vendor/unlisted",
				Endpoint:         server.URL,
				EndpointExplicit: true,
				Policy:           tc.policy,
			}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			seat := r.seat()
			if err := seat.Send("hello"); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(5 * time.Second)
			for failed := false; !failed; {
				select {
				case event := <-testEvents(r):
					if event.AgentID != seat.ID {
						continue
					}
					switch event.Kind {
					case "inference_request", "turn_done":
						t.Fatalf("an unrouted model produced %s", event.Kind)
					case "error":
						if !strings.Contains(event.Text, tc.reason) {
							t.Fatalf("error = %q, want the policy refusal %q", event.Text, tc.reason)
						}
						failed = true
					}
				case <-deadline:
					t.Fatal("the turn neither failed nor finished")
				}
			}
			if n := hits.Load(); n != 0 {
				t.Fatalf("the endpoint received %d requests for an unrouted model", n)
			}
		})
	}
}
