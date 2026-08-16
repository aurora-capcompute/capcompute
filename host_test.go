package capcompute

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aurora-capcompute/capcompute/sys"
)

// The guest must not learn the taint taxonomy. A stamped result reaches the
// journal with its provenance intact — that is how a crash-restarted host
// rebuilds a run's taint — but the bytes handed to the guest carry no label at
// all: they are the map an adversarial guest would use to find a path out that
// the flow policy has not marked.
func TestGuestRenderingCarriesNoLabels(t *testing.T) {
	stamped := sys.Result(json.RawMessage(`{"page":"..."}`)).WithLabels("untrusted_web", "secret")

	// The durable rendering keeps them: replay depends on it.
	durable, err := json.Marshal(stamped)
	if err != nil {
		t.Fatalf("marshal durable: %v", err)
	}
	if !bytes.Contains(durable, []byte("untrusted_web")) {
		t.Fatalf("durable rendering dropped provenance: %s", durable)
	}

	// The guest rendering does not.
	toGuest, err := json.Marshal(guestResult{
		Status:  stamped.Status(),
		Code:    stamped.Errno(),
		Result:  stamped.Result(),
		Message: stamped.Message(),
	})
	if err != nil {
		t.Fatalf("marshal guest: %v", err)
	}
	for _, leaked := range []string{"untrusted_web", "secret", "labels"} {
		if bytes.Contains(toGuest, []byte(leaked)) {
			t.Fatalf("SECURITY: guest rendering leaks %q: %s", leaked, toGuest)
		}
	}
	if !bytes.Contains(toGuest, []byte(`"page"`)) {
		t.Fatalf("guest rendering lost the result body: %s", toGuest)
	}
}
