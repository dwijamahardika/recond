package domain

import (
	"fmt"
	"time"
)

// Date is a civil calendar date stored as days since 1970-01-01. Settlement
// terms ("T+2") are calendar arithmetic in the provider's timezone; doing them
// on instants is how a payment made at 23:30 local time becomes "late".
// The zero value (1970-01-01) means "unset"; no payment data predates it.
type Date int32

const dateLayout = "2006-01-02"

// DateOf returns the calendar date of t as observed in loc.
func DateOf(t time.Time, loc *time.Location) Date {
	if t.IsZero() {
		return 0
	}
	y, m, d := t.In(loc).Date()
	return CivilDate(y, m, d)
}

// CivilDate builds a Date from year, month and day.
func CivilDate(y int, m time.Month, d int) Date {
	return Date(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400) //nolint:gosec // int32 days spans +-5.8 million years
}

// ParseDate parses s with a Go time layout containing only date fields.
func ParseDate(layout, s string) (Date, error) {
	t, err := time.Parse(layout, s)
	if err != nil {
		return 0, err
	}
	return CivilDate(t.Date()), nil
}

// IsZero reports whether d is unset.
func (d Date) IsZero() bool { return d == 0 }

// AddDays returns d shifted by n calendar days.
func (d Date) AddDays(n int) Date { return d + Date(n) } //nolint:gosec // n is a settlement term in days

// Time returns midnight UTC of d.
func (d Date) Time() time.Time { return time.Unix(int64(d)*86400, 0).UTC() }

// Format renders d with a Go time layout ("" when unset).
func (d Date) Format(layout string) string {
	if d == 0 {
		return ""
	}
	return d.Time().Format(layout)
}

func (d Date) String() string { return d.Format(dateLayout) }

// StartIn returns the instant the date begins in loc.
func (d Date) StartIn(loc *time.Location) time.Time {
	y, m, day := d.Time().Date()
	return time.Date(y, m, day, 0, 0, 0, 0, loc)
}

// MarshalText renders YYYY-MM-DD.
func (d Date) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses YYYY-MM-DD; empty means unset.
func (d *Date) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*d = 0
		return nil
	}
	v, err := ParseDate(dateLayout, string(b))
	if err != nil {
		return fmt.Errorf("date: %w", err)
	}
	*d = v
	return nil
}
