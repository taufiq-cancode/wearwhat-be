package domain

import (
	"fmt"
	"sync/atomic"
	"time"
)

var location atomic.Pointer[time.Location]

// SetLocation sets the zone used for dates shown to people.
func SetLocation(l *time.Location) { location.Store(l) }

func loc() *time.Location {
	if l := location.Load(); l != nil {
		return l
	}
	return time.UTC
}

// Location is the zone dates are shown in.
func Location() *time.Location { return loc() }

// Ago is the compact age used in lists: "now", "12m", "3h", "2d", then a date.
func Ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return ShortDate(t)
}

// ShortDate is "Oct 21".
func ShortDate(t time.Time) string { return t.In(loc()).Format("Jan 2") }

// LongDate is "Oct 2, 2026".
func LongDate(t time.Time) string { return t.In(loc()).Format("Jan 2, 2006") }

// DayTime is "Thu 15:02", used on tracking timelines.
func DayTime(t time.Time) string { return t.In(loc()).Format("Mon 15:04") }

// Weekday is "Thursday".
func Weekday(t time.Time) string { return t.In(loc()).Format("Monday") }

// ChatTime labels a message: "Now", "09:14" today, "Yesterday", "Tue" this week, else a date.
func ChatTime(t, now time.Time) string {
	l := loc()
	t, now = t.In(l), now.In(l)
	if now.Sub(t) < time.Minute {
		return "Now"
	}
	dayT := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, l)
	dayN := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, l)
	switch days := int(dayN.Sub(dayT).Hours() / 24); {
	case days <= 0:
		return t.Format("15:04")
	case days == 1:
		return "Yesterday"
	case days < 7:
		return t.Format("Mon")
	}
	return t.Format("Jan 2")
}

// NotificationGroup buckets a notification for its section heading.
func NotificationGroup(t, now time.Time) string {
	l := loc()
	t, now = t.In(l), now.In(l)
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return "Today"
	}
	if now.Sub(t) < 7*24*time.Hour {
		return "This week"
	}
	return "Earlier"
}

// DateRange is "Oct 20 – 27", or "Sep 28 – Oct 27" across months.
func DateRange(from, to time.Time) string {
	l := loc()
	from, to = from.In(l), to.In(l)
	if from.Month() == to.Month() {
		return fmt.Sprintf("%s – %d", from.Format("Jan 2"), to.Day())
	}
	return fmt.Sprintf("%s – %s", from.Format("Jan 2"), to.Format("Jan 2"))
}

// DropLabel is "Drop 03 · Fri 18:00".
func DropLabel(title string, releases time.Time) string {
	return title + " · " + releases.In(loc()).Format("Mon 15:04")
}
