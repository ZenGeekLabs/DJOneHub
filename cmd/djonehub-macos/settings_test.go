package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSettingsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "DJOneHub", "settings.json")
	settings, err := loadSettings(path)
	if err != nil || !settings.wakeRecoveryOn() {
		t.Fatalf("missing file = %+v, %v; want wake recovery on", settings, err)
	}

	off := false
	if err := saveSettings(path, appSettings{WakeRecovery: &off}); err != nil {
		t.Fatal(err)
	}
	settings, err = loadSettings(path)
	if err != nil || settings.wakeRecoveryOn() {
		t.Fatalf("saved off = %+v, %v", settings, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temporary file should be renamed away")
	}

	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err = loadSettings(path)
	if err == nil || !settings.wakeRecoveryOn() {
		t.Fatalf("broken file = %+v, %v; want an error and the default", settings, err)
	}
}

func TestWakeRecoverySettingEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	a := &app{settingsPath: path}
	put := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/api/network/wake-recovery", strings.NewReader(body))
		a.routes().ServeHTTP(recorder, request)
		return recorder
	}

	if !a.wakeRecoveryEnabled() {
		t.Fatal("wake recovery is on by default")
	}
	recorder := put(`{"enabled":false}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT off = %d %s", recorder.Code, recorder.Body)
	}
	var status map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil || status["enabled"] != false {
		t.Fatalf("response = %s", recorder.Body)
	}
	if a.wakeRecoveryEnabled() {
		t.Fatal("the switch should be off")
	}
	saved, err := loadSettings(path)
	if err != nil || saved.wakeRecoveryOn() {
		t.Fatalf("saved = %+v, %v", saved, err)
	}

	for body, code := range map[string]int{
		`{}`:                         http.StatusBadRequest,
		`{"enabled":true,"other":1}`: http.StatusBadRequest,
	} {
		if recorder := put(body); recorder.Code != code {
			t.Fatalf("PUT %s = %d, want %d", body, recorder.Code, code)
		}
	}

	a.wakeRecoveryForcedOff = true
	if recorder := put(`{"enabled":true}`); recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "-wake-recovery=false") {
		t.Fatalf("PUT while forced off = %d %s", recorder.Code, recorder.Body)
	}
}

func TestWakeRecoveryStatusJSON(t *testing.T) {
	a := &app{}
	a.setWakeRecovery(wakeRecoveryResult{Reason: reasonWake, Action: outcomeReset, Detail: "已重启模块", At: time.Unix(1_700_000_000, 0).UTC()})
	data, err := json.Marshal(a.wakeRecoveryStatus())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enabled":true,"reason":"wake","action":"reset","detail":"已重启模块","at":"2023-11-14T22:13:20Z"}`
	if string(data) != want {
		t.Fatalf("status = %s\nwant     %s", data, want)
	}

	a.wakeRecoveryForcedOff = true
	data, _ = json.Marshal(a.wakeRecoveryStatus())
	if !strings.Contains(string(data), `"enabled":false,"forced_off":true`) {
		t.Fatalf("forced off status = %s", data)
	}
}

func TestRecoveryStopsBeforeResetWhenTurnedOff(t *testing.T) {
	module := fakeModule{mode: 1}
	deps := module.deps()
	deps.Enabled = func() bool { return false }
	result := runInternetRecovery(reasonWake, recoveryPolicy{ModeAttempts: 1, LinkAttempts: 1}, deps)
	if result.Action != outcomeSkipped || result.next != followNone || module.resets != 0 || !strings.Contains(result.Detail, "关闭") {
		t.Fatalf("result=%+v resets=%d", result, module.resets)
	}
}

func TestPauseAndResumeRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	busy := wakeMonitorState{
		LastWake: base, Pending: true, Watching: true, RetryAt: base.Add(time.Minute),
		Attempts: 4, LidClosed: true, SwallowUntil: base.Add(time.Second),
	}
	paused := pauseRecovery(busy)
	if paused != (wakeMonitorState{LastWake: base}) {
		t.Fatalf("paused = %+v", paused)
	}

	later := base.Add(time.Hour)
	resumed := resumeRecovery(paused, later)
	if !resumed.Pending || resumed.Reason != reasonEnabled || !resumed.LastWake.Equal(later) || resumed.Attempts != 0 {
		t.Fatalf("resumed = %+v", resumed)
	}
	// A wake that happened while the feature was off must not run a second,
	// "wake" check on top of the one queued here.
	state, action := nextWakeMonitorAction(resumed, wakeSignal{Wake: later, LidOpen: true, LidKnown: true, Now: later})
	if action != wakeMonitorRecover || state.Reason != reasonEnabled || attemptLabel(state) != "turned on" {
		t.Fatalf("after resume = %+v action=%d", state, action)
	}
	if resumeRecovery(paused, time.Time{}).LastWake != base {
		t.Fatal("an unreadable waketime keeps the last one")
	}
}
