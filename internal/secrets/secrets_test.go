package secrets

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret.key")
	box, err := Open("", file)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.Encrypt("geheim")
	if err != nil || enc == "geheim" {
		t.Fatalf("encrypt: %q %v", enc, err)
	}
	// Reopening uses the same key file.
	box2, err := Open("", file)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := box2.Decrypt(enc); err != nil || plain != "geheim" {
		t.Fatalf("decrypt: %q %v", plain, err)
	}
	if v, _ := box.Encrypt(""); v != "" {
		t.Error("empty value should stay empty")
	}
}
