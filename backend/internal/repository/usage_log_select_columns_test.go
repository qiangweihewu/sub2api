package repository

import (
	"strings"
	"testing"
)

// scanUsageLogDestinationCount is the number of destinations scanUsageLog passes
// to scanner.Scan (see usage_log_repo_query.go). usageLogSelectColumns MUST select
// exactly this many columns in the same order, or every usage_logs read fails at
// runtime with "sql: expected N destination arguments in Scan, got M" — which
// surfaces as an empty 用量明细 / 使用记录 table. This guard fails CI on drift.
//
// If you add/remove a column, update scanUsageLog, usageLogInsertArgTypes,
// usageLogSelectColumns, and this constant together.
const scanUsageLogDestinationCount = 64

func TestUsageLogSelectColumnsMatchesScanDestinations(t *testing.T) {
	cols := strings.Split(usageLogSelectColumns, ",")
	got := len(cols)
	if got != scanUsageLogDestinationCount {
		t.Fatalf("usageLogSelectColumns has %d columns, but scanUsageLog scans %d destinations; "+
			"they must match or every usage_logs read fails at runtime", got, scanUsageLogDestinationCount)
	}
	// Guard against accidental blank/duplicate entries from a botched edit.
	seen := make(map[string]struct{}, got)
	for _, c := range cols {
		name := strings.TrimSpace(c)
		if name == "" {
			t.Fatalf("usageLogSelectColumns contains an empty column entry")
		}
		if _, dup := seen[name]; dup {
			t.Fatalf("usageLogSelectColumns contains duplicate column %q", name)
		}
		seen[name] = struct{}{}
	}
	// The two columns whose omission caused the 2026-07-18 merge regression.
	for _, required := range []string{"image_input_tokens", "image_input_cost"} {
		if _, ok := seen[required]; !ok {
			t.Errorf("usageLogSelectColumns is missing required column %q", required)
		}
	}
}
