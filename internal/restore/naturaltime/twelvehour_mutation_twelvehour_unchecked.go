//go:build mutation_twelvehour_unchecked

package naturaltime

// validTwelveHour — MUTATED: the range guard is a no-op, restoring the
// pre-fix world where "13am" resolves to 13:00 (1pm) and "0am"/"0pm"
// to midnight/noon — a typo'd 12-hour clock silently misdirecting a
// PITR target by twelve hours.
func validTwelveHour(h int) bool { return true }
