package ticket

import (
	"strings"
	"testing"
)

func TestNewIDUnique(t *testing.T) {
	a, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated IDs are equal")
	}
	if len(a) != 32 {
		t.Fatalf("expected 32 hex chars, got %d", len(a))
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	s := NewSigner("secret")
	for _, payload := range []string{"abc123", "admin:1750000000", "with:colons:inside"} {
		signed := s.Sign(payload)
		got, ok := s.Verify(signed)
		if !ok {
			t.Fatalf("Verify(%q) failed", signed)
		}
		if got != payload {
			t.Fatalf("payload roundtrip: got %q, want %q", got, payload)
		}
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	s := NewSigner("secret")
	signed := s.Sign("abc123")

	tampered := strings.Replace(signed, "abc123", "abc124", 1)
	if _, ok := s.Verify(tampered); ok {
		t.Fatal("tampered payload accepted")
	}

	badSig := signed[:len(signed)-2] + "xx"
	if _, ok := s.Verify(badSig); ok {
		t.Fatal("tampered signature accepted")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	signed := NewSigner("secret-a").Sign("abc123")
	if _, ok := NewSigner("secret-b").Verify(signed); ok {
		t.Fatal("value signed with another secret accepted")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	s := NewSigner("secret")
	for _, v := range []string{"", "nocolons", "v2:abc:sig", "v1:", ":::"} {
		if _, ok := s.Verify(v); ok {
			t.Fatalf("malformed value %q accepted", v)
		}
	}
}
