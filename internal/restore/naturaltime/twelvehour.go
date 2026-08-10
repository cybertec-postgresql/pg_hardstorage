//go:build !mutation_twelvehour_unchecked

package naturaltime

// twelvehour.go — a 12-hour clock has hours 1..12; there is no 0am or
// 13am. The post-am/pm range check in parseClock catches 13pm
// (13+12=25) but NOT 13am (stays 13, in range) or 0am (stays 0, in
// range), so those slid through and resolved to a WRONG instant
// ("13am" -> 13:00 = 1pm, for an operator who typo'd 1am). Rejecting
// malformed hours makes a bad clock ERROR loudly instead of silently
// misdirecting a recovery target — the package's stated
// reject-ambiguity-don't-guess contract. Own file so the mutation
// registry can carry the always-true variant.
func validTwelveHour(h int) bool { return h >= 1 && h <= 12 }
