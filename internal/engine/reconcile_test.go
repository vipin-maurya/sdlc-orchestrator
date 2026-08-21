package engine

import (
	"reflect"
	"testing"
)

func TestReconcileClaims(t *testing.T) {
	cases := []struct {
		name            string
		claimed, actual []string
		phantom, unrepd []string
	}{
		{
			name:    "exact match",
			claimed: []string{"src/app.txt"},
			actual:  []string{"src/app.txt"},
		},
		{
			// JOB-1: implementation.json listed MonthStepperLayoutTest.kt,
			// which git showed untouched, and created a different test file
			// instead. Both halves of that divergence must surface.
			name:    "substituted file",
			claimed: []string{"src/a.txt", "test/Planned.kt"},
			actual:  []string{"src/a.txt", "test/Actual.kt"},
			phantom: []string{"test/Planned.kt"},
			unrepd:  []string{"test/Actual.kt"},
		},
		{
			name:    "separator and prefix differences are not divergences",
			claimed: []string{`src\app.txt`, "./src/b.txt", ` "src/c.txt" `},
			actual:  []string{"src/app.txt", "src/b.txt", "src/c.txt"},
		},
		{
			name:    "duplicate claim reported once",
			claimed: []string{"src/gone.txt", "src/gone.txt"},
			actual:  []string{"src/app.txt"},
			phantom: []string{"src/gone.txt"},
			unrepd:  []string{"src/app.txt"},
		},
		{
			name:    "claiming nothing surfaces everything",
			claimed: nil,
			actual:  []string{"src/b.txt", "src/a.txt"},
			unrepd:  []string{"src/a.txt", "src/b.txt"},
		},
		{
			name:    "empty entries ignored",
			claimed: []string{"", "  "},
			actual:  []string{"src/a.txt"},
			unrepd:  []string{"src/a.txt"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phantom, unrepd := reconcileClaims(c.claimed, c.actual)
			if !reflect.DeepEqual(phantom, c.phantom) {
				t.Errorf("phantom = %v, want %v", phantom, c.phantom)
			}
			if !reflect.DeepEqual(unrepd, c.unrepd) {
				t.Errorf("unreported = %v, want %v", unrepd, c.unrepd)
			}
		})
	}
}
