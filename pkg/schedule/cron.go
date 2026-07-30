package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed 5-field expression: minute hour day-of-month month
// day-of-week. Corral parses it rather than only handing it to Kubernetes,
// because a schedule on a local backend has no CronJob controller to evaluate
// it — the tick has to decide for itself what is due.
//
// The supported grammar is the portable subset every backend agrees on:
// `*`, a number, a `a-b` range, a `a-b/n` or `*/n` step, and comma-separated
// lists of those. Non-standard extensions (@daily, L, W, #) are refused rather
// than silently mishandled — a schedule that means something different on two
// backends is worse than one that won't be created.
type Cron struct {
	expr    string
	minute  fieldSet
	hour    fieldSet
	dom     fieldSet
	month   fieldSet
	dow     fieldSet
	domStar bool // day-of-month was "*"
	dowStar bool // day-of-week was "*"
}

// fieldSet is the set of matching values for one field.
type fieldSet map[int]bool

func (f fieldSet) has(v int) bool { return f[v] }

// ParseCron parses a 5-field cron expression.
func ParseCron(expr string) (Cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Cron{}, fmt.Errorf("%q is not a 5-field cron expression (e.g. \"0 9 * * 1-5\")", expr)
	}
	specs := []struct {
		name     string
		min, max int
	}{
		{"minute", 0, 59},
		{"hour", 0, 23},
		{"day of month", 1, 31},
		{"month", 1, 12},
		{"day of week", 0, 6},
	}
	sets := make([]fieldSet, 5)
	for i, spec := range specs {
		set, err := parseField(fields[i], spec.min, spec.max)
		if err != nil {
			return Cron{}, fmt.Errorf("%s field %q: %w", spec.name, fields[i], err)
		}
		sets[i] = set
	}
	return Cron{
		expr:   strings.Join(fields, " "),
		minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		domStar: fields[2] == "*",
		dowStar: fields[4] == "*",
	}, nil
}

func (c Cron) String() string { return c.expr }

// Matches reports whether t falls on this schedule, to the minute.
//
// Day-of-month and day-of-week are OR'd when both are restricted, which is
// what cron(8) and Kubernetes both do: "0 0 1 * 1" fires on the 1st *and* on
// Mondays, not only on Mondays that fall on the 1st. Getting this backwards
// would silently skip most firings.
func (c Cron) Matches(t time.Time) bool {
	if !c.minute.has(t.Minute()) || !c.hour.has(t.Hour()) || !c.month.has(int(t.Month())) {
		return false
	}
	dom, dow := c.dom.has(t.Day()), c.dow.has(int(t.Weekday()))
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	default:
		return dom || dow
	}
}

func parseField(field string, min, max int) (fieldSet, error) {
	set := fieldSet{}
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return nil, fmt.Errorf("empty list element")
		}
		body, step := part, 1
		if base, stepStr, found := strings.Cut(part, "/"); found {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("step must be a positive number, got %q", stepStr)
			}
			body, step = base, n
		}

		lo, hi := min, max
		switch {
		case body == "*":
			// full range
		case strings.Contains(body, "-"):
			loStr, hiStr, _ := strings.Cut(body, "-")
			var err error
			if lo, err = boundedAtoi(loStr, min, max); err != nil {
				return nil, err
			}
			if hi, err = boundedAtoi(hiStr, min, max); err != nil {
				return nil, err
			}
			if lo > hi {
				return nil, fmt.Errorf("range %q runs backwards", body)
			}
		default:
			v, err := boundedAtoi(body, min, max)
			if err != nil {
				return nil, err
			}
			lo, hi = v, v
			if step > 1 {
				// "5/2" is a non-portable extension; refuse rather than guess.
				return nil, fmt.Errorf("a step needs a range or \"*\", got %q", part)
			}
		}
		for v := lo; v <= hi; v += step {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("matches nothing")
	}
	return set, nil
}

func boundedAtoi(s string, min, max int) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%d is outside %d-%d", v, min, max)
	}
	return v, nil
}
