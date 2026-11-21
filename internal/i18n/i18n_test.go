package i18n

import (
	"net/http/httptest"
	"sort"
	"testing"
)

// TestCatalogsComplete makes sure every language has exactly the same keys.
func TestCatalogsComplete(t *testing.T) {
	tr, err := New("en")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.catalogs) < 2 {
		t.Fatalf("only %d languages", len(tr.catalogs))
	}
	ref := tr.catalogs["en"]
	for lang, c := range tr.catalogs {
		var missing, extra []string
		for k := range ref {
			if _, ok := c[k]; !ok {
				missing = append(missing, k)
			}
		}
		for k := range c {
			if _, ok := ref[k]; !ok {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s: missing %v, not in en: %v", lang, missing, extra)
		}
		if c["lang.name"] == "" {
			t.Errorf("%s has no lang.name", lang)
		}
	}
}

func TestLanguage(t *testing.T) {
	tr, err := New("en")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if got := tr.Language(r); got != "en" {
		t.Errorf("default = %q", got)
	}
	r.Header.Set("Accept-Language", "nl-NL,nl;q=0.9,en;q=0.8")
	if got := tr.Language(r); got != "nl" {
		t.Errorf("browser = %q", got)
	}
	r.Header.Set("Cookie", Cookie+"=en")
	if got := tr.Language(r); got != "en" {
		t.Errorf("cookie = %q", got)
	}
}
