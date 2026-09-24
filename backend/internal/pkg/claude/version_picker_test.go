package claude

import (
	"testing"
)

func TestPickVersionForAccount_Empty(t *testing.T) {
	got := PickVersionForAccount(123, nil)
	if got == "" {
		t.Fatalf("expected non-empty fallback")
	}
	if got != GetCLICurrentVersion() {
		t.Errorf("empty recent should fallback to current=%q, got %q", GetCLICurrentVersion(), got)
	}
}

func TestPickVersionForAccount_Single(t *testing.T) {
	got := PickVersionForAccount(456, []string{"2.1.117"})
	if got != "2.1.117" {
		t.Errorf("single-element list should always return that element, got %q", got)
	}
}

func TestPickVersionForAccount_NonPositiveAccountID(t *testing.T) {
	for _, id := range []int64{0, -1, -999} {
		got := PickVersionForAccount(id, []string{"2.1.117", "2.1.116", "2.1.115"})
		if got != "2.1.117" {
			t.Errorf("accountID=%d should return latest, got %q", id, got)
		}
	}
}

func TestPickVersionForAccount_Stability(t *testing.T) {
	recent := []string{"2.1.117", "2.1.116", "2.1.115"}
	for i := int64(1); i <= 100; i++ {
		a := PickVersionForAccount(i, recent)
		b := PickVersionForAccount(i, recent)
		if a != b {
			t.Errorf("accountID=%d: non-deterministic pick: %q vs %q", i, a, b)
		}
	}
}

func TestPickVersionForAccount_Distribution(t *testing.T) {
	recent := []string{"2.1.117", "2.1.116", "2.1.115"}
	counts := map[string]int{}
	const N = 5000
	for i := int64(1); i <= N; i++ {
		counts[PickVersionForAccount(i, recent)]++
	}
	// 期望比例：75/20/5；允许 ±5pp 容差。
	pct := func(v int) float64 { return float64(v) / float64(N) * 100 }
	if p := pct(counts["2.1.117"]); p < 65 || p > 85 {
		t.Errorf("latest pct=%.1f out of [65,85]", p)
	}
	if p := pct(counts["2.1.116"]); p < 12 || p > 28 {
		t.Errorf("N-1 pct=%.1f out of [12,28]", p)
	}
	if p := pct(counts["2.1.115"]); p < 1 || p > 12 {
		t.Errorf("N-2 pct=%.1f out of [1,12]", p)
	}
}

func TestPickVersionForAccount_TwoElement(t *testing.T) {
	// len(recent)==2: 5% N-2 桶降级到 latest（因为 N-2 不可用），20% 走 N-1
	recent := []string{"2.1.117", "2.1.116"}
	counts := map[string]int{}
	const N = 3000
	for i := int64(1); i <= N; i++ {
		counts[PickVersionForAccount(i, recent)]++
	}
	if counts["2.1.117"]+counts["2.1.116"] != N {
		t.Errorf("output contains unexpected version: %v", counts)
	}
	// N-1 桶仍是 [5,25)，约 20% 落到 2.1.116
	pct116 := float64(counts["2.1.116"]) / float64(N) * 100
	if pct116 < 12 || pct116 > 28 {
		t.Errorf("N-1 pct=%.1f out of [12,28]", pct116)
	}
}

func TestBuildUserAgentForVersion(t *testing.T) {
	ua := BuildUserAgentForVersion("2.1.999")
	want := "claude-cli/2.1.999 (external, cli)"
	if ua != want {
		t.Errorf("got %q want %q", ua, want)
	}
	ua = BuildUserAgentForVersion("")
	if ua == "" || ua[:len("claude-cli/")] != "claude-cli/" {
		t.Errorf("empty version should fallback, got %q", ua)
	}
}

func TestSetCLICurrentVersion_Roundtrip(t *testing.T) {
	orig := GetCLICurrentVersion()
	t.Cleanup(func() { SetCLICurrentVersion(orig) })

	if !SetCLICurrentVersion("2.1.999") {
		t.Fatal("valid semver was rejected")
	}
	if got := GetCLICurrentVersion(); got != "2.1.999" {
		t.Errorf("after Set, got %q want 2.1.999", got)
	}
	if ua := DefaultHeaders()["User-Agent"]; ua != "claude-cli/2.1.999 (external, cli)" {
		t.Errorf("DefaultHeaders[User-Agent] not synced, got %q", ua)
	}
}

func TestSetCLICurrentVersion_InvalidRejected(t *testing.T) {
	orig := GetCLICurrentVersion()
	t.Cleanup(func() { SetCLICurrentVersion(orig) })

	for _, bad := range []string{"abc", "2.1", "2.1.0-beta", "v2.1.0", "2.1.0.1"} {
		if SetCLICurrentVersion(bad) {
			t.Errorf("invalid version %q was accepted", bad)
		}
	}
	// 状态没有改变
	if got := GetCLICurrentVersion(); got != orig {
		t.Errorf("state corrupted after rejected sets, got %q want %q", got, orig)
	}
}

func TestSetCLICurrentVersion_EmptyResetsToDefault(t *testing.T) {
	orig := GetCLICurrentVersion()
	t.Cleanup(func() { SetCLICurrentVersion(orig) })

	if !SetCLICurrentVersion("") {
		t.Fatal("empty should reset to default and return true")
	}
	if got := GetCLICurrentVersion(); got != CLIDefaultVersion {
		t.Errorf("empty should reset to default %q, got %q", CLIDefaultVersion, got)
	}
}
