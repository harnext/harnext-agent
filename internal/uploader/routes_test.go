package uploader

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/harnext/harnext-agent/internal/aggregate"
)

// routeGate sends sessions whose id starts with "a" through route A and "b" through route B.
func routeGate(r aggregate.Row) Decision {
	route := "route-" + strings.ToUpper(r.SessionID[:1])
	return Decision{Send: true, AccountHash: testHash, RouteID: route}
}

func tokens(route string) string { return "hxt_token-" + route }

func newRouted(t *testing.T, url string) *Uploader {
	t.Helper()
	u, err := New(Options{
		BaseURL: url, Token: tokens, AgentVersion: "0.1.0", Ledger: newLedger(), AllowInsecureLocalhost: true, Gate: routeGate,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSyncUploadsEachRouteWithItsOwnToken(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	ts := s.start()
	defer ts.Close()
	u := newRouted(t, ts.URL)

	res, err := u.Sync(context.Background(), []aggregate.Row{row("a1"), row("b1"), row("a2"), row("b2")}, 0)
	if err != nil || res.Accepted != 4 || res.Batches != 2 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	counts := map[string]int{}
	for i, auth := range s.auths {
		counts[auth] += len(s.bodies[i]["sessions"].([]any))
	}
	if counts["Bearer hxt_token-route-A"] != 2 || counts["Bearer hxt_token-route-B"] != 2 {
		t.Fatalf("sessions per token: %v", counts)
	}
}

func TestRevokedAccountDoesNotBlockTheOthers(t *testing.T) {
	s := &server{t: t, handler: func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }}
	var denied bool
	s.handler = func(w http.ResponseWriter, b map[string]any) {
		// The server records the request first; refuse route A's token only.
		if strings.HasSuffix(s.auths[len(s.auths)-1], "route-A") {
			denied = true
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		okHandler(w, b)
	}
	srv := s.start()
	defer srv.Close()
	u := newRouted(t, srv.URL)

	res, err := u.Sync(context.Background(), []aggregate.Row{row("a1"), row("a2"), row("b1")}, 0)
	var e *Error
	if !denied || err == nil || !asError(err, &e) || e.Code != CodeUnauthorized {
		t.Fatalf("want unauthorized after the others were sent: res=%+v err=%v", res, err)
	}
	if res.Accepted != 1 {
		t.Fatalf("the other account's session must be sent: %+v", res)
	}
	// The refused sessions are not recorded: the next Sync tries them again.
	denied = false
	s.handler = func(w http.ResponseWriter, b map[string]any) { okHandler(w, b) }
	res, err = u.Sync(context.Background(), []aggregate.Row{row("a1"), row("a2"), row("b1")}, 0)
	if err != nil || res.Accepted != 2 || res.Unchanged != 1 {
		t.Fatalf("retry: res=%+v err=%v", res, err)
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
