package schedule

import (
	"testing"
	"time"
)

func TestParseAndString(t *testing.T) {
	cases := map[string]string{
		"0 * * * *":        "0 * * * *",
		"*/15 2,4 * * 1-5": "0,15,30,45 2,4 * * 1,2,3,4,5",
		"0 3 * * 0-7":      "0 3 * * *",
		"0 3 * * 7":        "0 3 * * 0",
	}
	for in, want := range cases {
		s, err := Parse(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := s.String(); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "a * * * *", "*/0 * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestNext(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 11, 11, 10, 17, 30, 0, loc)
	check := func(expr string, want time.Time) {
		t.Helper()
		s, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Next(base); !got.Equal(want) {
			t.Errorf("%s: got %v want %v", expr, got, want)
		}
	}
	check("0 * * * *", time.Date(2026, 11, 11, 11, 0, 0, 0, loc))
	check("30 3 * * *", time.Date(2026, 11, 12, 3, 30, 0, 0, loc))
	check("0 0 1 * *", time.Date(2026, 12, 1, 0, 0, 0, 0, loc))
	check("0 12 * * 0", time.Date(2026, 11, 15, 12, 0, 0, 0, loc)) // next Sunday
	check("0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, loc))
	// Day-of-month OR day-of-week.
	check("0 0 1 * 5", time.Date(2026, 11, 13, 0, 0, 0, 0, loc))
	s, _ := Parse("0 0 30 2 *")
	if !s.Next(base).IsZero() {
		t.Error("30 February should never match")
	}
}
