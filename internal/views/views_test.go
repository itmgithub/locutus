package views

import (
	"testing"

	"itmon.nl/locutus/internal/i18n"
)

// TestTemplatesParse catches template syntax errors before the container starts.
func TestTemplatesParse(t *testing.T) {
	tr, err := i18n.New("en")
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(tr, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.pages) < 10 {
		t.Errorf("only %d pages parsed", len(r.pages))
	}
}
