package main

import "testing"

func TestLedgerMigrateRefusesMissingTarget(t *testing.T) {
	for _, args := range [][]string{nil, {"-owner-role", "owner", "extra"}, {"-unknown"}} {
		if code := runLedgerMigrate(args); code != 2 {
			t.Fatalf("argument refusal = %d", code)
		}
	}
	if code := runLedgerMigrate([]string{"-owner-role", "owner"}); code != 1 {
		t.Fatalf("incomplete connection = %d", code)
	}
}
