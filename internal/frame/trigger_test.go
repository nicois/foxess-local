package frame

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The trigger reads must be single valid frames that classify as
// Modbus reads with the corpus PDUs (guards accidental edits).
func TestTriggerReadsAreValidCorpusReads(t *testing.T) {
	want := []Modbus{
		{Fn: 0x03, Addr: 0x9c40, Count: 6},
		{Fn: 0x04, Addr: 0x277e, Count: 28},
	}
	for i, raw := range TriggerReads {
		t.Run(fmt.Sprintf("read%d", i+1), func(t *testing.T) {
			require.Len(t, raw, 19)
			frs, n := Split(raw, 0)
			require.Len(t, frs, 1)
			require.Equal(t, len(raw), n)
			kind, p := Decode(frs[0])
			require.Equal(t, KindMbread, kind)
			m, err := ParseModbus(p)
			require.NoError(t, err)
			require.Equal(t, want[i], m)
		})
	}
}
