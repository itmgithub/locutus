package schedule

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Preset kinds offered by the schedule editor. Everything that does not fit
// one of the simple kinds is edited as a raw cron expression ("custom").
const (
	KindMinutes = "minutes" // every N minutes
	KindHourly  = "hourly"  // every N hours at minute M
	KindDaily   = "daily"   // every day at HH:MM
	KindWeekly  = "weekly"  // on selected weekdays at HH:MM
	KindMonthly = "monthly" // on selected days of the month at HH:MM
	KindCustom  = "custom"  // free cron expression
)

// Kinds lists the preset kinds in the order they are offered.
var Kinds = []string{KindMinutes, KindHourly, KindDaily, KindWeekly, KindMonthly, KindCustom}

// MinuteIntervals and HourIntervals divide an hour and a day evenly, so the
// schedule repeats at the same moments every hour or day.
var (
	MinuteIntervals = []int{1, 2, 5, 10, 15, 20, 30}
	HourIntervals   = []int{1, 2, 3, 4, 6, 8, 12}
)

// Preset is the user friendly form of a schedule.
type Preset struct {
	Kind      string
	Every     int   // minutes: interval in minutes; hourly: interval in hours
	Minute    int   // hourly, daily, weekly, monthly
	Hour      int   // daily, weekly, monthly
	Weekdays  []int // weekly (0 = Sunday)
	MonthDays []int // monthly
	Cron      string
}

// Time returns the time of day as HH:MM.
func (p Preset) Time() string { return fmt.Sprintf("%02d:%02d", p.Hour, p.Minute) }

func in(vals []int, v int) bool {
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

func joinInts(vals []int) string {
	s := make([]string, len(vals))
	for i, v := range vals {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ",")
}

func sortedUnique(vals []int) []int {
	set := map[int]bool{}
	var out []int
	for _, v := range vals {
		if !set[v] {
			set[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// Expr returns the cron expression of the preset after validating it.
func (p Preset) Expr() (string, error) {
	timeOK := p.Minute >= 0 && p.Minute <= 59 && p.Hour >= 0 && p.Hour <= 23
	var expr string
	switch p.Kind {
	case KindMinutes:
		if !in(MinuteIntervals, p.Every) {
			return "", errors.New("invalid minute interval")
		}
		expr = fmt.Sprintf("*/%d * * * *", p.Every)
		if p.Every == 1 {
			expr = "* * * * *"
		}
	case KindHourly:
		if !in(HourIntervals, p.Every) || p.Minute < 0 || p.Minute > 59 {
			return "", errors.New("invalid hourly schedule")
		}
		hours := "*"
		if p.Every > 1 {
			hours = fmt.Sprintf("*/%d", p.Every)
		}
		expr = fmt.Sprintf("%d %s * * *", p.Minute, hours)
	case KindDaily:
		if !timeOK {
			return "", errors.New("invalid time")
		}
		expr = fmt.Sprintf("%d %d * * *", p.Minute, p.Hour)
	case KindWeekly:
		days := sortedUnique(p.Weekdays)
		if !timeOK || len(days) == 0 || days[0] < 0 || days[len(days)-1] > 6 {
			return "", errors.New("invalid weekly schedule")
		}
		expr = fmt.Sprintf("%d %d * * %s", p.Minute, p.Hour, joinInts(days))
	case KindMonthly:
		days := sortedUnique(p.MonthDays)
		if !timeOK || len(days) == 0 || days[0] < 1 || days[len(days)-1] > 31 {
			return "", errors.New("invalid monthly schedule")
		}
		expr = fmt.Sprintf("%d %d %s * *", p.Minute, p.Hour, joinInts(days))
	case KindCustom:
		expr = strings.Join(strings.Fields(p.Cron), " ")
	default:
		return "", errors.New("unknown schedule kind")
	}
	spec, err := Parse(expr)
	if err != nil {
		return "", err
	}
	if spec.Next(timeNow()).IsZero() {
		return "", errors.New("schedule never runs")
	}
	return expr, nil
}

// stepOf reports whether vals is lo, lo+n, lo+2n, ... up to hi for an n in
// options, and returns n. A nil slice ("every") counts as step 1.
func stepOf(vals []int, lo, hi int, options []int) (int, bool) {
	if vals == nil {
		return 1, in(options, 1)
	}
	if len(vals) < 2 || vals[0] != lo {
		return 0, false
	}
	n := vals[1] - vals[0]
	if !in(options, n) {
		return 0, false
	}
	for i, v := range vals {
		if v != lo+i*n {
			return 0, false
		}
	}
	return n, vals[len(vals)-1]+n > hi
}

// Detect returns the simplest preset that describes expr; expressions that
// fit no preset are returned as KindCustom.
func Detect(expr string) Preset {
	custom := Preset{Kind: KindCustom, Cron: expr, Every: 1}
	s, err := Parse(expr)
	if err != nil || s.Months != nil {
		return custom
	}
	single := func(v []int) bool { return len(v) == 1 }
	switch {
	case s.Hours == nil && s.Days == nil && s.Weekdays == nil:
		if n, ok := stepOf(s.Minutes, 0, 59, MinuteIntervals); ok {
			return Preset{Kind: KindMinutes, Every: n, Cron: expr}
		}
		if single(s.Minutes) {
			return Preset{Kind: KindHourly, Every: 1, Minute: s.Minutes[0], Cron: expr}
		}
	case single(s.Minutes) && s.Days == nil && s.Weekdays == nil && !single(s.Hours):
		if n, ok := stepOf(s.Hours, 0, 23, HourIntervals); ok {
			return Preset{Kind: KindHourly, Every: n, Minute: s.Minutes[0], Cron: expr}
		}
	case single(s.Minutes) && single(s.Hours):
		p := Preset{Minute: s.Minutes[0], Hour: s.Hours[0], Every: 1, Cron: expr}
		switch {
		case s.Days == nil && s.Weekdays == nil:
			p.Kind = KindDaily
		case s.Days == nil:
			p.Kind, p.Weekdays = KindWeekly, s.Weekdays
		case s.Weekdays == nil:
			p.Kind, p.MonthDays = KindMonthly, s.Days
		default:
			return custom
		}
		return p
	}
	return custom
}
