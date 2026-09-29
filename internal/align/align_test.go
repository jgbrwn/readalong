package align

import "testing"

func TestAlignTokens(t *testing.T) {
	got := AlignTokens([]string{"Call", "me", "Ishmael"}, []TimedToken{{"Call", 100, 200}, {"me", 210, 260}, {"Ishmael", 270, 500}})
	if got[2].StartMS != 270 || got[0].Confidence != 1 {
		t.Fatalf("unexpected: %#v", got)
	}
}
