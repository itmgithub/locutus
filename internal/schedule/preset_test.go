package schedule

import (
	"reflect"
	"testing"
)

func TestPresetExprAndDetect(t *testing.T) {
	cases := []struct {
		p    Preset
		expr string
	}{
		{Preset{Kind: KindMinutes, Every: 15}, "*/15 * * * *"},
		{Preset{Kind: KindMinutes, Every: 1}, "* * * * *"},
		{Preset{Kind: KindHourly, Every: 1, Minute: 12}, "12 * * * *"},
		{Preset{Kind: KindHourly, Every: 6, Minute: 12}, "12 */6 * * *"},
		{Preset{Kind: KindDaily, Hour: 4, Minute: 33}, "33 4 * * *"},
		{Preset{Kind: KindWeekly, Hour: 22, Minute: 0, Weekdays: []int{5, 1, 3}}, "0 22 * * 1,3,5"},
		{Preset{Kind: KindMonthly, Hour: 2, Minute: 5, MonthDays: []int{1, 15}}, "5 2 1,15 * *"},
		{Preset{Kind: KindCustom, Cron: "  0  9-17 * *   1-5 "}, "0 9-17 * * 1-5"},
	}
	for _, c := range cases {
		got, err := c.p.Expr()
		if err != nil || got != c.expr {
			t.Errorf("%+v: got %q, %v; want %q", c.p, got, err, c.expr)
			continue
		}
		d := Detect(got)
		if d.Kind != c.p.Kind {
			t.Errorf("Detect(%q) kind = %s, want %s", got, d.Kind, c.p.Kind)
		}
		if again, _ := d.Expr(); c.p.Kind != KindCustom && again != got {
			t.Errorf("round trip %q -> %q", got, again)
		}
	}
	// Expanded forms (as stored by older versions) are recognised too.
	if d := Detect("0,15,30,45 * * * *"); d.Kind != KindMinutes || d.Every != 15 {
		t.Errorf("expanded minutes: %+v", d)
	}
	if d := Detect("7 0,8,16 * * *"); d.Kind != KindHourly || d.Every != 8 || d.Minute != 7 {
		t.Errorf("expanded hours: %+v", d)
	}
	if d := Detect("0,30 * * * 1,2"); d.Kind != KindCustom {
		t.Errorf("custom expected: %+v", d)
	}
	if d := Detect("0 4 * * 1-5"); d.Kind != KindWeekly || !reflect.DeepEqual(d.Weekdays, []int{1, 2, 3, 4, 5}) {
		t.Errorf("weekday range: %+v", d)
	}
	for _, bad := range []Preset{
		{Kind: KindMinutes, Every: 7},
		{Kind: KindHourly, Every: 5, Minute: 1},
		{Kind: KindDaily, Hour: 24},
		{Kind: KindWeekly, Hour: 1},
		{Kind: KindMonthly, Hour: 1, MonthDays: []int{32}},
		{Kind: KindCustom, Cron: "0 0 30 2 *"},
		{Kind: "x"},
	} {
		if _, err := bad.Expr(); err == nil {
			t.Errorf("%+v: expected error", bad)
		}
	}
}
