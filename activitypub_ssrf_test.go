package writefreely

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The ActivityPub inbox (POST /api/collections/{alias}/inbox, registered with
// handler.All) is unauthenticated, and reaches resolveIRI with an
// attacker-supplied actor/object IRI. isPublicIRI guards that call, but it
// resolves the host and returns; the transport then resolves again when it
// dials. These tests pin the two gaps that leaves open.

// TestActivityPubClientRefusesLoopbackDial asserts the client validates the
// address it actually connects to, rather than trusting a hostname check that
// ran before resolution. Fails without a DialContext hook on the client.
func TestActivityPubClientRefusesLoopbackDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := activityPubClient().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected dial to %s to be refused, but the request succeeded", srv.URL)
	}
}

// TestActivityPubClientRevalidatesRedirects asserts redirects are re-checked.
// isPublicIRI only ever sees the original URL, so without a CheckRedirect
// policy a public host can bounce the request to an internal one.
func TestActivityPubClientRevalidatesRedirects(t *testing.T) {
	c := activityPubClient()
	if c.CheckRedirect == nil {
		t.Fatal("expected a CheckRedirect policy on the ActivityPub client, got nil")
	}

	req, err := http.NewRequest("GET", "http://127.0.0.1:1/internal", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(req, nil); err == nil {
		t.Fatal("expected redirect to a loopback address to be refused")
	}
}

// TestActivityPubClientCapsRedirectHops keeps the policy from allowing an
// unbounded redirect chain.
func TestActivityPubClientCapsRedirectHops(t *testing.T) {
	c := activityPubClient()
	if c.CheckRedirect == nil {
		t.Fatal("expected a CheckRedirect policy on the ActivityPub client, got nil")
	}

	req, err := http.NewRequest("GET", "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	via := make([]*http.Request, 5)
	for i := range via {
		via[i] = req
	}
	if err := c.CheckRedirect(req, via); err == nil {
		t.Fatal("expected the 6th redirect hop to be refused")
	}
}
