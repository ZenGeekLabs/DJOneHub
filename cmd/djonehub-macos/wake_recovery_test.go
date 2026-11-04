package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	healthyLink  = dongleLink{Name: "en7", Port: "Baiwang", Status: "active", IPv4: "192.168.225.20", Found: true}
	noAddrLink   = dongleLink{Name: "en7", Port: "Baiwang", Status: "active", Found: true}
	tempAddrLink = dongleLink{Name: "en10", Status: "active", IPv4: "192.0.0.2", Found: true}
	missingLink  = dongleLink{}
)

func TestSelectDongleLink(t *testing.T) {
	ports := parseHardwarePorts(`
Hardware Port: Wi-Fi
Device: en0
Ethernet Address: aa:aa:aa:aa:aa:aa

Hardware Port: Baiwang
Device: en7
Ethernet Address: bb:bb:bb:bb:bb:bb

Hardware Port: Thunderbolt Ethernet
Device: en6
Ethernet Address: cc:cc:cc:cc:cc:cc
`)
	if len(ports) != 3 {
		t.Fatalf("ports = %+v", ports)
	}

	healthy := selectDongleLink(ports, []macNetInterface{
		{Name: "en0", Status: "active", IPv4: "10.0.0.8", Kind: "ethernet"},
		{Name: "en6", Status: "active", IPv4: "192.168.1.20", Kind: "ethernet"},
		{Name: "en7", Status: "active", IPv4: "192.168.225.20", Kind: "ethernet"},
	})
	if !healthy.healthy() || healthy.Name != "en7" || healthy.Port != "Baiwang" {
		t.Fatalf("healthy link = %+v", healthy)
	}

	down := selectDongleLink(ports, []macNetInterface{
		{Name: "en0", Status: "active", IPv4: "10.0.0.8"},
		{Name: "en7", Status: "inactive"},
	})
	if down.healthy() || down.Name != "en7" || down.Status != "inactive" {
		t.Fatalf("down link = %+v", down)
	}

	byLAN := selectDongleLink(nil, []macNetInterface{
		{Name: "en8", Status: "active", IPv4: "192.168.225.16"},
		{Name: "en6", Status: "active", IPv4: "192.168.1.9"},
	})
	if byLAN.Name != "en8" || !byLAN.healthy() {
		t.Fatalf("lan link = %+v", byLAN)
	}

	missing := selectDongleLink(ports, []macNetInterface{
		{Name: "en0", Status: "active", IPv4: "10.0.0.8"},
	})
	if missing.healthy() || missing.provisional() || missing.Status != "missing" {
		t.Fatalf("missing link = %+v", missing)
	}
}

func TestDongleLinkStates(t *testing.T) {
	if !healthyLink.healthy() || healthyLink.provisional() {
		t.Fatal("192.168.225.x on an active NIC is healthy")
	}
	if tempAddrLink.healthy() || !tempAddrLink.provisional() {
		t.Fatal("192.0.0.2 is a temporary address, not the module LAN")
	}
	if noAddrLink.healthy() || noAddrLink.provisional() {
		t.Fatal("an active NIC without an address is neither healthy nor provisional")
	}
}

// fakeModule stands in for the module and macOS in recovery tests. Observe
// walks through before until the first reset and through after once one ran;
// the last entry of each repeats.

type fakeModule struct {
	mode         int
	modeErr      error
	modeFailures int   // QueryMode fails this many times first
	modeAfter    error // QueryMode error once a reset ran
	before       []dongleLink
	after        []dongleLink
	observeErr   error
	resetErr     error
	busy         bool
	lidClosed    bool
	cancelSleep  bool

	resets    int
	seen      int
	modeCalls int
	events    []string
	progress  []string
}

func (f *fakeModule) deps() internetRecoveryDeps {
	return internetRecoveryDeps{
		QueryMode: func() (int, error) {
			f.events = append(f.events, "mode")
			f.modeCalls++
			if f.modeCalls <= f.modeFailures {
				return -1, errors.New("DJI USB device is not connected")
			}
			if f.resets > 0 && f.modeAfter != nil {
				return -1, f.modeAfter
			}
			return f.mode, f.modeErr
		},
		Observe: func() (dongleLink, error) {
			f.events = append(f.events, "observe")
			if f.observeErr != nil {
				return dongleLink{}, f.observeErr
			}
			links := f.before
			if f.resets > 0 {
				links = f.after
			}
			if len(links) == 0 {
				return missingLink, nil
			}
			link := links[min(f.seen, len(links)-1)]
			f.seen++
			return link, nil
		},
		LockModule: func() (func(), bool) {
			if f.busy {
				return nil, false
			}
			f.events = append(f.events, "lock")
			return func() { f.events = append(f.events, "unlock") }, true
		},
		AllowReset: func() bool { return !f.lidClosed },
		Reset: func() error {
			f.events = append(f.events, "reset")
			f.resets++
			f.seen = 0
			return f.resetErr
		},
		Sleep:    func(time.Duration) bool { return !f.cancelSleep },
		Progress: func(line string) { f.progress = append(f.progress, line) },
	}
}

func TestRunInternetRecovery(t *testing.T) {
	policy := recoveryPolicy{ModeAttempts: 2, LinkAttempts: 2, PostResetLinkAttempts: 3}
	cases := []struct {
		name       string
		module     fakeModule
		policy     *recoveryPolicy
		action     recoveryOutcome
		next       followUp
		resets     int
		detailHint string
	}{
		{name: "sms mode", module: fakeModule{mode: 0}, action: outcomeNone, next: followNone, detailHint: "usbnet=0"},
		{name: "already up", module: fakeModule{mode: 1, before: []dongleLink{healthyLink}}, action: outcomeNone, next: followNone, detailHint: "en7"},
		{name: "link comes up while polling", module: fakeModule{mode: 1, before: []dongleLink{missingLink, healthyLink}}, action: outcomeNone, next: followNone},
		{name: "temporary address waits instead of resetting", module: fakeModule{mode: 1, before: []dongleLink{tempAddrLink}}, action: outcomeNone, next: followWatch, detailHint: "192.0.0.2"},
		{name: "missing interface resets once", module: fakeModule{mode: 1, after: []dongleLink{healthyLink}}, action: outcomeReset, next: followNone, resets: 1, detailHint: "192.168.225.20"},
		{name: "active interface without address resets", module: fakeModule{mode: 1, before: []dongleLink{noAddrLink}, after: []dongleLink{healthyLink}}, action: outcomeReset, next: followNone, resets: 1},
		{name: "real address replaces temporary one after reset", module: fakeModule{mode: 1, after: []dongleLink{tempAddrLink, tempAddrLink, healthyLink}}, action: outcomeReset, next: followNone, resets: 1, detailHint: "192.168.225.20"},
		{name: "temporary address after reset is watched", module: fakeModule{mode: 1, after: []dongleLink{tempAddrLink}}, action: outcomeReset, next: followWatch, resets: 1, detailHint: "192.0.0.2"},
		{name: "interface still missing after reset", module: fakeModule{mode: 1}, action: outcomeFailed, next: followWatch, resets: 1, detailHint: "仍未恢复"},
		{name: "reset command fails", module: fakeModule{mode: 1, resetErr: errors.New("USB bulk write short transfer: 0/12")}, action: outcomeFailed, next: followWatch, resets: 1, detailHint: "重启模块失败"},
		{name: "at does not return after reset", module: fakeModule{mode: 1, modeAfter: errors.New("DJI USB device is not connected")}, action: outcomeFailed, next: followWatch, resets: 1, detailHint: "AT 没有重新连上"},
		{name: "at never answers", module: fakeModule{modeErr: errors.New("USB AT command timed out without response")}, action: outcomeSkipped, next: followRetry, detailHint: "AT"},
		{name: "nic inspection failure does not reset", module: fakeModule{mode: 1, observeErr: errors.New("networksetup failed")}, action: outcomeSkipped, next: followRetry, detailHint: "networksetup"},
		{name: "busy module is retried later", module: fakeModule{mode: 1, busy: true}, action: outcomeSkipped, next: followRetry, detailHint: "稍后再试"},
		{name: "closed lid defers", module: fakeModule{mode: 1, lidClosed: true}, action: outcomeDeferred, next: followNone, detailHint: "开盖"},
		{name: "cancel stops without retry", module: fakeModule{mode: 1, cancelSleep: true}, policy: &recoveryPolicy{ModeAttempts: 2, LinkAttempts: 2, Pause: time.Second}, action: outcomeSkipped, next: followNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			module := tc.module
			p := policy
			if tc.policy != nil {
				p = *tc.policy
			}
			result := runInternetRecovery(reasonWake, p, module.deps())
			if result.Action != tc.action || result.next != tc.next || module.resets != tc.resets {
				t.Fatalf("result=%+v resets=%d events=%v", result, module.resets, module.events)
			}
			if result.Reason != reasonWake {
				t.Fatalf("reason = %q", result.Reason)
			}
			if !strings.Contains(result.Detail, tc.detailHint) {
				t.Fatalf("detail %q does not mention %q", result.Detail, tc.detailHint)
			}
		})
	}
}

func TestRecoveryHoldsModuleUntilATReturns(t *testing.T) {
	module := fakeModule{mode: 1, after: []dongleLink{healthyLink}}
	runInternetRecovery(reasonWake, recoveryPolicy{ModeAttempts: 1, LinkAttempts: 1, PostResetLinkAttempts: 1}, module.deps())
	got := strings.Join(module.events, " ")
	want := "mode observe lock reset mode unlock observe"
	if got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestRecoveryProgress(t *testing.T) {
	module := fakeModule{mode: 1, modeFailures: 1, after: []dongleLink{missingLink, healthyLink}}
	policy := recoveryPolicy{ModeAttempts: 3, LinkAttempts: 2, PostResetLinkAttempts: 3, Pause: 2 * time.Second}
	result := runInternetRecovery(reasonWake, policy, module.deps())
	if result.Action != outcomeReset || result.logText != "module rebooted; internet link is back: en7 active 192.168.225.20" ||
		result.Detail != "已重启模块，上网网卡恢复：en7 active 192.168.225.20" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{
		"module AT is not answering yet; USB devices need a few seconds to power up after a wake (now: DJI USB device is not connected); waiting up to about 4 seconds",
		"internet interface not ready; giving macOS a chance to bring it up (now: no Baiwang or 192.168.225.x interface); waiting up to about 2 seconds",
		"macOS still has no internet interface (no Baiwang or 192.168.225.x interface); rebooting the module",
		"module rebooted; waiting for macOS to bring up the internet interface (now: no Baiwang or 192.168.225.x interface); waiting up to about 4 seconds",
	}
	if strings.Join(module.progress, "\n") != strings.Join(want, "\n") {
		t.Fatalf("progress:\n%s", strings.Join(module.progress, "\n"))
	}

	quiet := fakeModule{mode: 1, before: []dongleLink{healthyLink}}
	runInternetRecovery(reasonWake, policy, quiet.deps())
	if len(quiet.progress) != 0 {
		t.Fatalf("a healthy link needs no progress lines: %q", quiet.progress)
	}
}

func TestModuleLock(t *testing.T) {
	a := &app{}
	release := a.holdModuleOp()
	if _, ok := a.tryLockModule(); ok {
		t.Fatal("recovery must not lock the module while a request holds it")
	}
	release()
	unlock, ok := a.tryLockModule()
	if !ok {
		t.Fatal("recovery should lock an idle module")
	}
	held := make(chan struct{})
	go func() {
		defer close(held)
		a.holdModuleOp()()
	}()
	select {
	case <-held:
		t.Fatal("a request must wait while recovery reboots the module")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	<-held
}

func TestModuleRebootQuietsExpectedUSBErrors(t *testing.T) {
	buf := captureLog(t)
	a := &app{usbAT: &usbAT{}}
	lost := errors.New("DJI USB device is not connected")

	a.beginModuleReboot()
	a.markUSBATDetached("module reset")
	a.reportUSBError(&a.smsPollLog, "SMS poll failed", lost)
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "module reboot: sending AT+CFUN=1,1") {
		t.Fatalf("during a planned reboot only the explanation is logged:\n%s", got)
	}

	a.usbMu.Lock()
	a.rebootStarted = time.Now().Add(-moduleRebootGrace - time.Second)
	a.usbMu.Unlock()
	a.reportUSBError(&a.smsPollLog, "SMS poll failed", lost)
	if !strings.Contains(buf.String(), "SMS poll failed: DJI USB device is not connected") {
		t.Fatalf("a reboot that never finishes must not hide errors forever:\n%s", buf.String())
	}

	buf.Reset()
	a.beginModuleReboot()
	a.abortModuleReboot(errors.New("USB bulk write short transfer: 0/12"))
	if a.moduleRebooting() || !strings.Contains(buf.String(), "AT+CFUN=1,1 failed") {
		t.Fatalf("abort = %v\n%s", a.moduleRebooting(), buf.String())
	}
}

func TestWakeMonitorClosedLid(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	state := wakeMonitorState{LastWake: base, Pending: true, Reason: reasonStartup}

	state, action := nextWakeMonitorAction(state, wakeSignal{
		Wake: base, LidOpen: false, LidKnown: true, Now: base,
	})
	if action != wakeMonitorWaitForLid || !state.Pending || state.Reason != reasonStartup {
		t.Fatalf("startup with a closed lid = %+v action=%d", state, action)
	}

	dark := base.Add(18 * time.Second)
	state, action = nextWakeMonitorAction(state, wakeSignal{
		Wake: dark, LidOpen: false, LidKnown: true, Now: dark,
	})
	if action != wakeMonitorWaitForLid || !state.Pending || state.Reason != reasonWake || !state.LastWake.Equal(dark) {
		t.Fatalf("dark wake = %+v action=%d", state, action)
	}

	opened := dark.Add(16 * time.Second)
	state, action = nextWakeMonitorAction(state, wakeSignal{
		Wake: dark, LidOpen: true, LidKnown: true, Now: opened,
	})
	if action != wakeMonitorRecover || state.Pending || state.LidClosed || state.Reason != reasonWake {
		t.Fatalf("lid open = %+v action=%d", state, action)
	}
}

func TestWakeMonitorDoesNotSwallowClosedLid(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	state := wakeMonitorState{
		LastWake:     base,
		SwallowUntil: base.Add(wakeRecoverySwallow),
	}
	// The module's own USB wake while the lid stays open is not another sleep.
	openBump := base.Add(5 * time.Second)
	next, action := nextWakeMonitorAction(state, wakeSignal{
		Wake: openBump, LidOpen: true, LidKnown: true, Now: openBump,
	})
	if action != wakeMonitorIdle || next.Pending || !next.LastWake.Equal(openBump) {
		t.Fatalf("open-lid bump = %+v action=%d", next, action)
	}

	// The 18s-later USB2_wake in the hardware log happened with the lid closed.
	// It must stay pending so the later lid opening can recover once.
	dark := base.Add(18 * time.Second)
	next, action = nextWakeMonitorAction(state, wakeSignal{
		Wake: dark, LidOpen: false, LidKnown: true, Now: dark,
	})
	if action != wakeMonitorWaitForLid || !next.Pending || next.Reason != reasonWake {
		t.Fatalf("closed-lid bump = %+v action=%d", next, action)
	}
	opened := dark.Add(16 * time.Second)
	next, action = nextWakeMonitorAction(next, wakeSignal{
		Wake: dark, LidOpen: true, LidKnown: true, Now: opened,
	})
	if action != wakeMonitorRecover || next.Pending {
		t.Fatalf("recover after closed-lid bump = %+v action=%d", next, action)
	}
}

func TestWakeMonitorWatchesAfterFailedReset(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	state := finishRecovery(
		wakeMonitorState{LastWake: base, Reason: reasonWake},
		wakeRecoveryResult{Action: outcomeFailed, next: followWatch},
		wakeSignal{Wake: base, LidOpen: true, LidKnown: true, Now: base},
	)
	if !state.Watching || !state.RetryAt.Equal(base.Add(wakeRecoverySettle)) || state.Attempts != 1 {
		t.Fatalf("after failed reset = %+v", state)
	}
	state, action := nextWakeMonitorAction(state, wakeSignal{Wake: base, LidOpen: true, LidKnown: true, Now: base.Add(time.Minute)})
	if action != wakeMonitorObserve {
		t.Fatalf("during settle = %+v action=%d", state, action)
	}

	dark := base.Add(18 * time.Second)
	state, action = nextWakeMonitorAction(state, wakeSignal{
		Wake: dark, LidOpen: false, LidKnown: true, Now: dark,
	})
	if action != wakeMonitorWaitForLid || !state.Pending || state.Attempts != 0 {
		t.Fatalf("dark wake during settle = %+v action=%d", state, action)
	}
	opened := dark.Add(16 * time.Second)
	state, action = nextWakeMonitorAction(state, wakeSignal{
		Wake: dark, LidOpen: true, LidKnown: true, Now: opened,
	})
	if action != wakeMonitorObserve || state.Pending || !state.Watching {
		t.Fatalf("open during settle must not reset again = %+v action=%d", state, action)
	}

	expired := base.Add(wakeRecoverySettle + time.Second)
	state, action = nextWakeMonitorAction(state, wakeSignal{
		Wake: dark, LidOpen: true, LidKnown: true, Now: expired,
	})
	if action != wakeMonitorRecover || state.Watching || !state.RetryAt.IsZero() || state.Reason != reasonRetry {
		t.Fatalf("retry after settle = %+v action=%d", state, action)
	}
}

func TestWakeMonitorRetriesSkippedRun(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	state := finishRecovery(
		wakeMonitorState{LastWake: base, Reason: reasonWake},
		wakeRecoveryResult{Action: outcomeSkipped, next: followRetry},
		wakeSignal{Wake: base, LidOpen: true, LidKnown: true, Now: base},
	)
	if state.Watching || !state.RetryAt.Equal(base.Add(wakeRecoveryRetryDelay)) {
		t.Fatalf("after skipped run = %+v", state)
	}
	state, action := nextWakeMonitorAction(state, wakeSignal{Wake: base, Now: base.Add(10 * time.Second)})
	if action != wakeMonitorIdle {
		t.Fatalf("before retry = %+v action=%d", state, action)
	}
	state, action = nextWakeMonitorAction(state, wakeSignal{Wake: base, LidOpen: true, LidKnown: true, Now: base.Add(wakeRecoveryRetryDelay)})
	if action != wakeMonitorRecover || state.Reason != reasonRetry {
		t.Fatalf("retry = %+v action=%d", state, action)
	}
}

func TestWakeMonitorStopsAfterMaxAttempts(t *testing.T) {
	wake := time.Unix(1_700_000_000, 0)
	now := wake
	state := wakeMonitorState{LastWake: wake, Pending: true, Reason: reasonWake}
	tick := func(lidOpen bool) wakeMonitorAction {
		var action wakeMonitorAction
		state, action = nextWakeMonitorAction(state, wakeSignal{Wake: wake, LidOpen: lidOpen, LidKnown: true, Now: now})
		return action
	}

	for run := 1; run <= wakeRecoveryMaxAttempts; run++ {
		if action := tick(true); action != wakeMonitorRecover {
			t.Fatalf("run %d: action=%d state=%+v", run, action, state)
		}
		state = finishRecovery(state, wakeRecoveryResult{Action: outcomeFailed, next: followWatch},
			wakeSignal{Wake: wake, LidOpen: true, LidKnown: true, Now: now})
		// The module's own USB wake a minute after the reset, with the lid
		// open, must not restart the budget.
		wake = now.Add(time.Minute)
		now = wake
		if action := tick(true); action != wakeMonitorObserve || state.Attempts != run {
			t.Fatalf("run %d bump: action=%d state=%+v", run, action, state)
		}
		now = state.RetryAt
	}
	if action := tick(true); action != wakeMonitorGiveUp || state.following() || state.Pending {
		t.Fatalf("after %d runs: action=%d state=%+v", wakeRecoveryMaxAttempts, action, state)
	}
	now = now.Add(time.Hour)
	if action := tick(true); action != wakeMonitorIdle {
		t.Fatalf("after giving up: action=%d", action)
	}

	// Closing the lid and waking again starts a new budget.
	wake = now.Add(time.Hour)
	now = wake
	if action := tick(false); action != wakeMonitorWaitForLid || state.Attempts != 0 {
		t.Fatalf("new wake: action=%d state=%+v", action, state)
	}
}

func TestFinishRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	open := wakeSignal{Wake: base, LidOpen: true, LidKnown: true, Now: base}

	deferred := finishRecovery(wakeMonitorState{LastWake: base, Attempts: 3}, wakeRecoveryResult{Action: outcomeDeferred}, open)
	if !deferred.Pending || !deferred.LidClosed || deferred.Attempts != 3 || deferred.following() {
		t.Fatalf("deferred = %+v", deferred)
	}

	done := finishRecovery(wakeMonitorState{LastWake: base}, wakeRecoveryResult{Action: outcomeReset}, open)
	if done.Pending || done.following() || done.Attempts != 1 || !done.SwallowUntil.Equal(base.Add(wakeRecoverySwallow)) {
		t.Fatalf("reset = %+v", done)
	}

	spent := finishRecovery(wakeMonitorState{LastWake: base, Attempts: wakeRecoveryMaxAttempts - 1},
		wakeRecoveryResult{Action: outcomeSkipped, next: followRetry}, open)
	if !spent.RetryAt.Equal(base) {
		t.Fatalf("a skipped last attempt should give up on the next tick: %+v", spent)
	}

	// The lid closed and the Mac slept while the run was going.
	later := base.Add(time.Minute)
	slept := finishRecovery(wakeMonitorState{LastWake: base, Attempts: 4}, wakeRecoveryResult{Action: outcomeReset},
		wakeSignal{Wake: later, LidOpen: false, LidKnown: true, Now: later})
	if !slept.Pending || !slept.LidClosed || slept.Attempts != 0 || slept.Reason != reasonWake || !slept.LastWake.Equal(later) {
		t.Fatalf("sleep during run = %+v", slept)
	}

	// With the lid open, a bump during the run came from the reset.
	bumped := finishRecovery(wakeMonitorState{LastWake: base}, wakeRecoveryResult{Action: outcomeReset},
		wakeSignal{Wake: later, LidOpen: true, LidKnown: true, Now: later})
	if bumped.Pending || !bumped.LastWake.Equal(later) {
		t.Fatalf("reset bump = %+v", bumped)
	}
}

func TestManualRecovery(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	// Lid reported closed (clamshell mode), budget spent, still watching.
	state := wakeMonitorState{
		LastWake: base, Attempts: wakeRecoveryMaxAttempts, Watching: true,
		RetryAt: base.Add(time.Minute), LidClosed: true,
	}
	state = requestManualRecovery(state)
	state, action := nextWakeMonitorAction(state, wakeSignal{Wake: base, LidOpen: false, LidKnown: true, Now: base})
	if action != wakeMonitorRecover || state.Reason != reasonManual || state.Manual || state.Pending || state.following() {
		t.Fatalf("manual = %+v action=%d", state, action)
	}
	if got := attemptLabel(state); got != "manual retry" {
		t.Fatalf("label = %q", got)
	}
}

func TestAttemptLabel(t *testing.T) {
	cases := map[string]wakeMonitorState{
		"wake":                {Reason: reasonWake},
		"startup check":       {Reason: reasonStartup},
		"retry, attempt 2/10": {Reason: reasonRetry, Attempts: 1},
		"wake, attempt 3/10":  {Reason: reasonWake, Attempts: 2},
	}
	for want, state := range cases {
		if got := attemptLabel(state); got != want {
			t.Fatalf("attemptLabel(%+v) = %q, want %q", state, got, want)
		}
	}
}

func TestFollowUpPlan(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 28, 9, 0, time.Local)
	watch := wakeMonitorState{Attempts: 1, Watching: true, RetryAt: at}
	if got := followUpPlan(watch, true); got != "Watching the internet interface for 3 minutes while USB may still be enumerating; if it is still missing at 12:28:09, retrying (attempt 2/10). Press Enter to retry now." {
		t.Fatalf("watch plan = %q", got)
	}
	retry := wakeMonitorState{Attempts: 3, RetryAt: at}
	if got := followUpPlan(retry, false); got != "Retrying in 30 seconds at 12:28:09 (attempt 4/10)." {
		t.Fatalf("retry plan = %q", got)
	}
	last := wakeMonitorState{Attempts: wakeRecoveryMaxAttempts, Watching: true, RetryAt: at}
	if got := followUpPlan(last, false); !strings.Contains(got, "All 10 automatic attempts used") || !strings.Contains(got, "automatic reboots stop") {
		t.Fatalf("last plan = %q", got)
	}
	for name, state := range map[string]wakeMonitorState{
		"done":         {Attempts: 1},
		"lid closed":   {Attempts: 1, Pending: true, RetryAt: at},
		"budget spent": {Attempts: wakeRecoveryMaxAttempts, RetryAt: at},
	} {
		if got := followUpPlan(state, true); got != "" {
			t.Fatalf("%s: plan = %q", name, got)
		}
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		3 * time.Minute:        "3 minutes",
		time.Minute:            "1 minute",
		30 * time.Second:       "30 seconds",
		23*time.Second + 400e6: "23 seconds",
		90 * time.Second:       "90 seconds",
	} {
		if got := durationText(d); got != want {
			t.Fatalf("durationText(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestNeedsLid(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	idle := wakeMonitorState{LastWake: base}
	if idle.needsLid(wakeSignal{Wake: base, Now: base.Add(time.Hour)}) {
		t.Fatal("an idle tick without a new wake should not read the lid")
	}
	for name, state := range map[string]wakeMonitorState{
		"pending":  {LastWake: base, Pending: true},
		"watching": {LastWake: base, Watching: true},
		"retry":    {LastWake: base, RetryAt: base.Add(time.Hour)},
		"swallow":  {LastWake: base, SwallowUntil: base.Add(2 * time.Hour)},
	} {
		if !state.needsLid(wakeSignal{Wake: base, Now: base.Add(time.Hour)}) {
			t.Fatalf("%s should read the lid", name)
		}
	}
	if !idle.needsLid(wakeSignal{Wake: base.Add(time.Second), Now: base.Add(time.Hour)}) {
		t.Fatal("a new wake should read the lid")
	}
}

func TestObserveLateLink(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	watching := wakeMonitorState{Watching: true, RetryAt: now.Add(wakeRecoverySettle)}

	failed := wakeRecoveryResult{Reason: reasonWake, Action: outcomeFailed, next: followWatch, Detail: "已重启模块，上网网卡仍未恢复"}
	state, got, changed := observeLateLink(watching, failed, healthyLink, now)
	if !changed || got.Action != outcomeReset || got.next != followNone || !strings.Contains(got.Detail, "随后恢复") ||
		!strings.Contains(got.Detail, "192.168.225.20") || !got.At.Equal(now) || state.following() {
		t.Fatalf("promoted = %+v state=%+v", got, state)
	}

	waiting := wakeRecoveryResult{Action: outcomeNone, next: followWatch, Detail: "当前仍是临时地址 192.0.0.2"}
	if _, got, changed := observeLateLink(watching, waiting, healthyLink, now); !changed || got.Action != outcomeNone || !strings.Contains(got.Detail, "随后拿到地址") {
		t.Fatalf("address without reset = %+v", got)
	}

	if state, _, changed := observeLateLink(watching, failed, tempAddrLink, now); changed || !state.Watching {
		t.Fatal("a temporary address keeps the watch going")
	}

	done := wakeRecoveryResult{Action: outcomeReset}
	if state, got, changed := observeLateLink(watching, done, healthyLink, now); changed || got != done || state.following() {
		t.Fatalf("a finished result stays as it is, but the watch ends: %+v %+v", got, state)
	}
}

func TestGiveUpResult(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := giveUpResult(wakeRecoveryResult{Action: outcomeNone, next: followWatch, Detail: "当前仍是临时地址 192.0.0.2", logText: "placeholder address 192.0.0.2"}, 11, now)
	if got.logText != "placeholder address 192.0.0.2 (gave up after 11 attempts)" {
		t.Fatalf("log text = %q", got.logText)
	}
	if got.Action != outcomeFailed || got.next != followNone || !got.At.Equal(now) ||
		!strings.Contains(got.Detail, "192.0.0.2") || !strings.Contains(got.Detail, "已尝试 11 次") {
		t.Fatalf("give up = %+v", got)
	}
}

func TestParseClamshellOpen(t *testing.T) {
	open, known := parseClamshellOpen("\"AppleClamshellCausesSleep\" = No\n\"AppleClamshellState\" = No\n")
	if !open || !known {
		t.Fatalf("open lid = %v %v", open, known)
	}
	open, known = parseClamshellOpen("\"AppleClamshellState\" = Yes")
	if open || !known {
		t.Fatalf("closed lid = %v %v", open, known)
	}
	if _, known = parseClamshellOpen("\"AppleClamshellCausesSleep\" = No"); known {
		t.Fatal("a missing clamshell state is unknown")
	}
}
