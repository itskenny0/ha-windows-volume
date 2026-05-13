package haclient

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestPkcePair(t *testing.T) {
	v, c, err := pkcePair()
	if err != nil {
		t.Fatalf("pkcePair: %v", err)
	}
	if len(v) < 32 {
		t.Fatalf("verifier too short: %d", len(v))
	}
	// Challenge must be SHA-256(verifier) base64url-no-pad.
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if c != want {
		t.Fatalf("challenge mismatch: got %s want %s", c, want)
	}
}

func TestTokensValid(t *testing.T) {
	if (&Tokens{}).Valid() {
		t.Fatal("empty tokens should not be valid")
	}
}
