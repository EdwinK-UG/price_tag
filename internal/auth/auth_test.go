package auth

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected format %s", h)
	}
	if !VerifyPassword(h, "correct horse battery") {
		t.Fatal("good password rejected")
	}
	if VerifyPassword(h, "correct horse batterY") {
		t.Fatal("bad password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salt must differ")
	}
}

func TestPolicyAndTamper(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	if VerifyPassword("garbage", "x") || VerifyPassword("$argon2id$v=19$m=999999999,t=1,p=1$AAAA$AAAA", "x") {
		t.Fatal("tampered hash accepted")
	}
}

func TestTokens(t *testing.T) {
	a, ha, _ := NewToken("pt_")
	b, hb, _ := NewToken("pt_")
	if a == b || string(ha) == string(hb) || !strings.HasPrefix(a, "pt_") {
		t.Fatal("tokens not unique")
	}
	if string(HashToken(a)) != string(ha) {
		t.Fatal("hash mismatch")
	}
	if p := RandomPassword(12); len(p) != 12 {
		t.Fatal("bad length")
	}
}
