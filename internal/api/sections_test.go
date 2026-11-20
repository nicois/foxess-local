package api

import (
	"slices"
	"testing"
)

// TestSectionsOf: register addresses map to the spec's config-event
// sections (ConfigEvent.changed), each once, in a stable order; live
// values and unmapped registers map to nothing.
func TestSectionsOf(t *testing.T) {
	cases := []struct {
		addrs []int
		want  []string
	}{
		{[]int{41000}, []string{"work-mode"}},
		{[]int{41021, 41022, 41027}, []string{"limits"}},
		{[]int{41029, 41000}, []string{"work-mode", "meter-compensation"}},
		{[]int{48000, 48010, 48089}, []string{"schedule"}},
		{[]int{30000, 30017}, []string{"identity"}},
		{[]int{31000, 31001, 41012}, nil},
	}
	for _, c := range cases {
		if got := SectionsOf(c.addrs); !slices.Equal(got, c.want) {
			t.Errorf("SectionsOf(%v) = %v, want %v", c.addrs, got, c.want)
		}
	}
}

// TestSectionsOfInstallerSettings: a write to any of the Inverter page's
// installer settings refreshes its limits section.
func TestSectionsOfInstallerSettings(t *testing.T) {
	for _, a := range []int{41007, 41008, 41015, 41016, 41027, 41028, 41200, 41201} {
		if got := SectionsOf([]int{a}); len(got) != 1 || got[0] != SectionLimits {
			t.Errorf("%d: %v, want [%s]", a, got, SectionLimits)
		}
	}
}
