package builtins

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
)

func TestMathCalc(t *testing.T) {
	for expr, want := range map[string]string{
		"0.1 + 0.2":                        "0.3",   // no float noise
		"3 * 19.99":                        "59.97", // ints and decimals mix
		"7 / 2":                            "3.5",
		"9999999999 * 9999999999":          "9.999999998e+19", // no silent int wrap-around
		"-7 % 3":                           "-1",              // % keeps integers
		"[10, 20, 30][1] + sum(1..4)":      "30",              // so do indexes and ranges
		"round(2.345 * 100) / 100":         "2.35",
		"max(2, 7.5) > 7 ? 1 : 0":          "1",
		"mean([1, 2]) + median([3, 1, 2])": "3.5",
		"2 ** 0.5":                         "1.4142135623731",
	} {
		res, _ := mathCalc(t.Context(), json.RawMessage(`{"expression":`+quote(expr)+`}`))
		if res.IsError || res.Content != want {
			t.Errorf("%s = %q (error %v), want %s", expr, res.Content, res.IsError, want)
		}
	}
	for _, bad := range []string{
		"1 / 0", "(1 + 2", "now()", `repeat("a", 100000000)`, `upper("x")`, strings.Repeat("1+", 1500) + "1", "",
	} {
		if res, _ := mathCalc(t.Context(), json.RawMessage(`{"expression":`+quote(bad)+`}`)); !res.IsError {
			t.Errorf("%.30q must be an error result, got %q", bad, res.Content)
		}
	}
}

func TestNow(t *testing.T) {
	res, _ := now(t.Context(), json.RawMessage(`{"timezone":"Asia/Tokyo"}`))
	var out map[string]string
	if res.IsError || json.Unmarshal([]byte(res.Content), &out) != nil {
		t.Fatalf("now: %+v", res)
	}
	utc, err1 := time.Parse(time.RFC3339, out["utc"])
	local, err2 := time.Parse(time.RFC3339, out["local"])
	if err1 != nil || err2 != nil || !utc.Equal(local) || out["utc_offset"] != "+09:00" || out["timezone"] != "Asia/Tokyo" ||
		out["weekday"] != local.Weekday().String() || time.Since(utc) > time.Minute {
		t.Fatalf("now in Tokyo: %v", out)
	}
	if res, _ := now(t.Context(), nil); res.IsError || !strings.Contains(res.Content, `"timezone":"UTC"`) {
		t.Fatalf("default must be UTC: %+v", res)
	}
	for _, tz := range []string{"Mars/Olympus", "Local"} {
		if res, _ := now(t.Context(), json.RawMessage(`{"timezone":"`+tz+`"}`)); !res.IsError {
			t.Fatalf("%s must be refused: %+v", tz, res)
		}
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// config reserves every built-in tool name, so no sub-agent (served on the
// same hfy server) can shadow one.
func TestBuiltinNamesAreReserved(t *testing.T) {
	names := []string{ToolSearch, ConversationSearch}
	for _, c := range CoreTools() {
		names = append(names, c.Tool.Name)
	}
	slices.Sort(names)
	reserved := slices.Sorted(slices.Values(config.BuiltinTools))
	if !slices.Equal(names, reserved) {
		t.Fatalf("config.BuiltinTools = %v, want %v", reserved, names)
	}
}
