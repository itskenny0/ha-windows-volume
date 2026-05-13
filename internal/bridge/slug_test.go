package bridge

import "testing"

func TestHostSlug_neverEmpty(t *testing.T) {
	got := hostSlug()
	if got == "" {
		t.Fatal("hostSlug() returned empty")
	}
	for _, r := range got {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			t.Fatalf("hostSlug() contains invalid rune %q in %q", r, got)
		}
	}
}
