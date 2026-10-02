package settings

import (
	"testing"

	"github.com/miista/automouse/internal/store"
)

func TestResolveClampsRunDelayToFloor(t *testing.T) {
	tests := []struct {
		name      string
		persisted int
		want      int
	}{
		{"zero would fire every scheduler tick", 0, MinRunDelayMinutes},
		{"negative", -5, MinRunDelayMinutes},
		{"one", 1, MinRunDelayMinutes},
		{"just below the floor", MinRunDelayMinutes - 1, MinRunDelayMinutes},
		{"at the floor", MinRunDelayMinutes, MinRunDelayMinutes},
		{"above the floor is left alone", 90, 90},
		{"well above", 1440, 1440},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(store.Settings{NextRunDelayMinutes: tc.persisted})
			if got.Settings.NextRunDelayMinutes != tc.want {
				t.Errorf("NextRunDelayMinutes = %d, want %d", got.Settings.NextRunDelayMinutes, tc.want)
			}
		})
	}
}

// TestEnvOverrideIsAlsoClamped is the gap the floor exists to close: the env
// path bypasses the API handler's validation entirely, so clamping only
// there would leave AUTOMOUSE_SETTING_NEXT_RUN_DELAY_MINUTES=0 able to
// schedule a run every 5-second tick.
func TestEnvOverrideIsAlsoClamped(t *testing.T) {
	for _, v := range []string{"0", "1", "-10", "59"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("AUTOMOUSE_SETTING_NEXT_RUN_DELAY_MINUTES", v)
			got := Resolve(store.Settings{NextRunDelayMinutes: 120})
			if got.Settings.NextRunDelayMinutes != MinRunDelayMinutes {
				t.Errorf("env value %q resolved to %d, want the floor %d", v, got.Settings.NextRunDelayMinutes, MinRunDelayMinutes)
			}
			if !got.IsManaged("next_run_delay_minutes") {
				t.Error("field not reported as env-managed")
			}
		})
	}
}

func TestEnvOverrideAboveFloorIsHonoured(t *testing.T) {
	t.Setenv("AUTOMOUSE_SETTING_NEXT_RUN_DELAY_MINUTES", "90")
	got := Resolve(store.Settings{NextRunDelayMinutes: 60})
	if got.Settings.NextRunDelayMinutes != 90 {
		t.Errorf("NextRunDelayMinutes = %d, want 90", got.Settings.NextRunDelayMinutes)
	}
}

// TestDefaultIsNotBelowFloor guards the pair: a default the floor would
// clamp means the shipped default silently isn't what store.DefaultSettings
// claims.
func TestDefaultIsNotBelowFloor(t *testing.T) {
	if d := store.DefaultSettings().NextRunDelayMinutes; d < MinRunDelayMinutes {
		t.Errorf("default delay %d is below the floor %d", d, MinRunDelayMinutes)
	}
}

func TestEnvOverridePerKey(t *testing.T) {
	t.Setenv("AUTOMOUSE_SETTING_POINTS_BUFFER", "5000")
	t.Setenv("AUTOMOUSE_SETTING_BUY_VIP", "false")
	t.Setenv("AUTOMOUSE_SETTING_UPLOAD_STRATEGY", "wedge_only")

	got := Resolve(store.DefaultSettings())

	if got.Settings.PointsBuffer != 5000 {
		t.Errorf("PointsBuffer = %d, want 5000", got.Settings.PointsBuffer)
	}
	if got.Settings.BuyVIP {
		t.Error("BuyVIP = true, want false")
	}
	if got.Settings.UploadStrategy != store.StrategyWedgeOnly {
		t.Errorf("UploadStrategy = %q, want %q", got.Settings.UploadStrategy, store.StrategyWedgeOnly)
	}
	for _, k := range []string{"points_buffer", "buy_vip", "upload_strategy"} {
		if !got.IsManaged(k) {
			t.Errorf("%s not reported as env-managed", k)
		}
	}
	if got.IsManaged("mam_id") {
		t.Error("mam_id reported as managed with no env var set")
	}
}

// TestInvalidEnvValuesFallBack pins that junk in an env var leaves the
// persisted value intact rather than zeroing the setting.
func TestInvalidEnvValuesFallBack(t *testing.T) {
	t.Setenv("AUTOMOUSE_SETTING_POINTS_BUFFER", "not-a-number")
	t.Setenv("AUTOMOUSE_SETTING_BUY_VIP", "maybe")
	t.Setenv("AUTOMOUSE_SETTING_UPLOAD_STRATEGY", "sideways")

	persisted := store.Settings{PointsBuffer: 7777, BuyVIP: true, UploadStrategy: store.StrategyAlternate}
	got := Resolve(persisted)

	if got.Settings.PointsBuffer != 7777 {
		t.Errorf("PointsBuffer = %d, want the persisted 7777", got.Settings.PointsBuffer)
	}
	if !got.Settings.BuyVIP {
		t.Error("BuyVIP was overwritten by an unparseable value")
	}
	if got.Settings.UploadStrategy != store.StrategyAlternate {
		t.Errorf("UploadStrategy = %q, want the persisted %q", got.Settings.UploadStrategy, store.StrategyAlternate)
	}
}

func TestMaskSecret(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "•"},
		{"abcd", "••••"},
		{"abcde", "•bcde"}, // only the last 4 stay visible
		{"0123456789", "••••••6789"},
	}
	for _, tc := range tests {
		if got := MaskSecret(tc.in); got != tc.want {
			t.Errorf("MaskSecret(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuthDisabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"nonsense", false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("AUTOMOUSE_AUTH_DISABLED", tc.value)
			if got := AuthDisabled(); got != tc.want {
				t.Errorf("AuthDisabled() with %q = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
