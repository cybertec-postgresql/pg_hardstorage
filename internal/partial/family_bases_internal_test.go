package partial

import "testing"

// familyBases must be a superset filter for isFamilyMember: whenever rp is
// a family member of base, base is among rp's candidate bases, so the
// single-pass lookup in materialiseFamilies can never miss a file.
func TestFamilyBases_CoversIsFamilyMember(t *testing.T) {
	bases := []string{"base/16384/2619", "base/1/100", "pg_tblspc/16400/PG_17_1/5/16401", "x_vm", "b.1"}
	tags := []string{"", "_vm", "_fsm", "_init"}
	segs := []string{"", ".1", ".25", ".", ".x", ".1.2"}
	for _, b := range bases {
		for _, tag := range tags {
			for _, seg := range segs {
				rp := b + tag + seg
				if !isFamilyMember(rp, b) {
					continue
				}
				found := false
				for _, c := range familyBases(rp) {
					found = found || c == b
				}
				if !found {
					t.Errorf("%q is a family member of %q but familyBases = %v", rp, b, familyBases(rp))
				}
			}
		}
	}
}
