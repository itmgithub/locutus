// Package schedule implements cron-like schedules. A schedule is stored as a
// standard 5-field cron expression (minute hour day-of-month month
// day-of-week), but is edited in the web interface as sets of checkboxes.
package schedule

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Field ranges, in the order of the cron expression.
var fieldRanges = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// Spec is a parsed schedule. A nil field means "every" (*).
type Spec struct {
	Minutes  []int
	Hours    []int
	Days     []int // day of month
	Months   []int
	Weekdays []int // 0 = Sunday
}

// Parse reads a cron expression. Supported are *, numbers, lists (1,2),
// ranges (1-5) and steps (*/15, 0-30/10). Weekday 7 is accepted as Sunday.
func Parse(expr string) (*Spec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, errors.New("schedule must have 5 fields")
	}
	var out [5][]int
	for i, f := range fields {
		lo, hi := fieldRanges[i][0], fieldRanges[i][1]
		if i == 4 {
			hi = 7
		}
		vals, err := parseField(f, lo, hi)
		if err != nil {
			return nil, fmt.Errorf("field %d: %w", i+1, err)
		}
		if i == 4 && vals != nil {
			vals = normalizeWeekdays(vals)
		}
		out[i] = vals
	}
	return &Spec{Minutes: out[0], Hours: out[1], Days: out[2], Months: out[3], Weekdays: out[4]}, nil
}

func normalizeWeekdays(vals []int) []int {
	set := map[int]bool{}
	for _, v := range vals {
		set[v%7] = true
	}
	var out []int
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	if len(out) == 7 {
		return nil
	}
	return out
}

func parseField(f string, lo, hi int) ([]int, error) {
	if f == "*" {
		return nil, nil
	}
	set := map[int]bool{}
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid step %q", stepStr)
			}
			step = n
		}
		start, end := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if start, err = strconv.Atoi(a); err != nil {
				return nil, fmt.Errorf("invalid value %q", a)
			}
			end = start
			if isRange {
				if end, err = strconv.Atoi(b); err != nil {
					return nil, fmt.Errorf("invalid value %q", b)
				}
			} else if hasStep {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return nil, fmt.Errorf("value out of range %d-%d", lo, hi)
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	var vals []int
	for v := range set {
		vals = append(vals, v)
	}
	sort.Ints(vals)
	if len(vals) == hi-lo+1 {
		return nil, nil // all values: same as *
	}
	return vals, nil
}

// String returns the cron expression of the spec.
func (s *Spec) String() string {
	parts := make([]string, 5)
	for i, vals := range [][]int{s.Minutes, s.Hours, s.Days, s.Months, s.Weekdays} {
		if vals == nil {
			parts[i] = "*"
			continue
		}
		strs := make([]string, len(vals))
		for j, v := range vals {
			strs[j] = strconv.Itoa(v)
		}
		parts[i] = strings.Join(strs, ",")
	}
	return strings.Join(parts, " ")
}

func contains(vals []int, v int) bool {
	if vals == nil {
		return true
	}
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

// dayMatches applies the cron rule: when both day-of-month and day-of-week
// are restricted, a day matches if either of them matches.
func (s *Spec) dayMatches(t time.Time) bool {
	dom, dow := contains(s.Days, t.Day()), contains(s.Weekdays, int(t.Weekday()))
	if s.Days != nil && s.Weekdays != nil {
		return dom || dow
	}
	return dom && dow
}

// timeNow is replaceable in tests.
var timeNow = time.Now

// Next returns the first time after t that matches the schedule, or the zero
// time if the schedule never matches (e.g. 30 February).
func (s *Spec) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		switch {
		case !contains(s.Months, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
		case !contains(s.Hours, t.Hour()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
		case !contains(s.Minutes, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// NextN returns the next n run times after t.
func (s *Spec) NextN(t time.Time, n int) []time.Time {
	var out []time.Time
	for i := 0; i < n; i++ {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// NextRun parses expr and returns its next run after t (zero on error).
func NextRun(expr string, t time.Time) time.Time {
	s, err := Parse(expr)
	if err != nil {
		return time.Time{}
	}
	return s.Next(t)
}
