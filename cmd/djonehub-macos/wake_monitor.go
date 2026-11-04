package main

import (
	"fmt"
	"strings"
	"time"
)

const (
	// wakeRecoveryMaxAttempts caps recovery runs per wake, so a module whose
	// NIC never comes back is not rebooted forever. Each run sends at most
	// one AT+CFUN=1,1.
	wakeRecoveryMaxAttempts = 10
	// wakeRecoverySettle is how long to keep looking for Baiwang after a
	// reset that did not see it before trying again. The interface can appear
	// late when the USB bus was asleep for part of the wait.
	wakeRecoverySettle = 3 * time.Minute
	// wakeRecoveryRetryDelay spaces out runs that were skipped because the
	// module was busy or AT or the NIC could not be read.
	wakeRecoveryRetryDelay = 30 * time.Second
	// wakeRecoverySwallow ignores kern.waketime bumps shortly after a run.
	// AT+CFUN re-enumeration moves waketime while the lid stays open.
	wakeRecoverySwallow = 20 * time.Second
)

// wakeMonitorAction is one iteration of the macOS wake loop.
// A USB dark wake while the lid is closed moves kern.waketime the same way a
// lid opening does, and resetting the module then drops a link that cannot
// enumerate until the lid is open.
type wakeMonitorAction int

const (
	wakeMonitorIdle wakeMonitorAction = iota
	wakeMonitorWaitForLid
	wakeMonitorRecover
	wakeMonitorObserve
	wakeMonitorGiveUp
)

type wakeSignal struct {
	Wake     time.Time
	LidOpen  bool
	LidKnown bool
	Now      time.Time
}

func (s wakeSignal) lidClosed() bool {
	return s.LidKnown && !s.LidOpen
}

type wakeMonitorState struct {
	LastWake time.Time
	Reason   recoveryReason
	// Pending means a recovery run is due as soon as the lid is open.
	Pending bool
	// LidClosed records that the lid was seen closed since the last run.
	LidClosed  bool
	LoggedWait bool
	// SwallowUntil ignores waketime bumps caused by our own AT+CFUN.
	SwallowUntil time.Time
	// Watching means a reset ran but the NIC was not ready. The monitor looks
	// for it every tick instead of resetting again.
	Watching bool
	// RetryAt is when the next run for this wake is due; zero if none is.
	RetryAt time.Time
	// Attempts counts recovery runs since the last real wake.
	Attempts int
	// Manual means the user pressed Enter. See requestManualRecovery.
	Manual bool
}

// following reports whether this wake still has a watch or a retry scheduled.
func (s wakeMonitorState) following() bool {
	return s.Watching || !s.RetryAt.IsZero()
}

// requestManualRecovery queues a run the user asked for by pressing Enter. It
// runs on the next tick even with the lid closed or the attempt budget spent:
// someone typing in the terminal means the Mac is fully awake.
func requestManualRecovery(state wakeMonitorState) wakeMonitorState {
	state.Pending = true
	state.Manual = true
	return state
}

// needsLid reports whether a tick has to read the lid state. With nothing
// pending or scheduled, only a waketime change matters.
func (s wakeMonitorState) needsLid(sig wakeSignal) bool {
	return sig.Wake.After(s.LastWake) || s.Pending || s.following() || sig.Now.Before(s.SwallowUntil)
}

func nextWakeMonitorAction(state wakeMonitorState, sig wakeSignal) (wakeMonitorState, wakeMonitorAction) {
	lidClosed := sig.lidClosed()
	if lidClosed {
		state.LidClosed = true
	}
	if !sig.Wake.IsZero() && sig.Wake.After(state.LastWake) {
		state.LastWake = sig.Wake
		// Swallow only while the lid has stayed open. A closed lid means this
		// bump is a dark wake (often the dongle's own USB2_wake) and the
		// recovery has to wait until the lid opens.
		if state.LidClosed || !sig.Now.Before(state.SwallowUntil) {
			// A wake after the lid closed, or with nothing scheduled, starts
			// a fresh attempt budget. A bump while this wake is still being
			// followed keeps counting, so it cannot restart the budget forever.
			if state.LidClosed || !state.following() {
				state.Attempts = 0
			}
			state.Pending = true
			state.Reason = reasonWake
		}
	}
	if !state.RetryAt.IsZero() && !sig.Now.Before(state.RetryAt) {
		state.RetryAt = time.Time{}
		state.Watching = false
		if !state.Pending {
			state.Pending = true
			state.Reason = reasonRetry
		}
	}

	if state.Manual {
		state.Manual = false
		state.Pending = false
		state.Reason = reasonManual
		state.LidClosed = false
		state.LoggedWait = false
		state.Watching = false
		state.RetryAt = time.Time{}
		return state, wakeMonitorRecover
	}
	switch {
	case !state.Pending && state.Watching && !lidClosed:
		return state, wakeMonitorObserve
	case !state.Pending:
		return state, wakeMonitorIdle
	case lidClosed:
		return state, wakeMonitorWaitForLid
	}
	state.Pending = false
	state.LidClosed = false
	state.LoggedWait = false
	if state.Watching {
		// The reset already ran and the NIC was still missing. Opening the
		// lid keeps looking instead of sending AT+CFUN again while USB is
		// still enumerating.
		return state, wakeMonitorObserve
	}
	if state.Attempts >= wakeRecoveryMaxAttempts {
		state.RetryAt = time.Time{}
		return state, wakeMonitorGiveUp
	}
	return state, wakeMonitorRecover
}

// waitForLid queues another run for when the lid opens.
func waitForLid(state wakeMonitorState) wakeMonitorState {
	state.Pending = true
	state.LidClosed = true
	state.LoggedWait = false
	return state
}

// finishRecovery records a finished run and schedules what comes next. sig is
// read after the run, so a sleep that happened during it is not lost.
func finishRecovery(state wakeMonitorState, result wakeRecoveryResult, sig wakeSignal) wakeMonitorState {
	state.Watching = false
	state.RetryAt = time.Time{}
	if result.Action == outcomeDeferred {
		return waitForLid(state)
	}
	state.Attempts++
	state.SwallowUntil = sig.Now.Add(wakeRecoverySwallow)
	switch result.next {
	case followWatch:
		state.Watching = true
		state.RetryAt = sig.Now.Add(wakeRecoverySettle)
	case followRetry:
		state.RetryAt = sig.Now.Add(wakeRecoveryRetryDelay)
		if state.Attempts >= wakeRecoveryMaxAttempts {
			// Nothing to wait for: give up on the next tick.
			state.RetryAt = sig.Now
		}
	}
	if !sig.lidClosed() {
		state.LidClosed = false
	}
	if !sig.Wake.IsZero() && sig.Wake.After(state.LastWake) {
		state.LastWake = sig.Wake
		// With the lid open this bump came from our own reset. With the lid
		// closed the Mac slept during the run, which is a new wake.
		if sig.lidClosed() {
			state = waitForLid(state)
			state.Reason = reasonWake
			state.Attempts = 0
		}
	}
	return state
}

// observeLateLink handles one watch tick. A healthy NIC ends the watch, and
// if the last result was waiting for it, the result is updated.
func observeLateLink(state wakeMonitorState, last wakeRecoveryResult, link dongleLink, now time.Time) (wakeMonitorState, wakeRecoveryResult, bool) {
	if !link.healthy() {
		return state, last, false
	}
	state.Watching = false
	state.RetryAt = time.Time{}
	if last.next != followWatch {
		return state, last, false
	}
	if last.Action == outcomeNone {
		last = last.with(outcomeNone, followNone,
			note{zh: "上网网卡随后拿到地址：" + link.describe(), en: "the internet interface got its address: " + link.String()})
	} else {
		last = last.with(outcomeReset, followNone,
			note{zh: "已重启模块，上网网卡随后恢复：" + link.describe(), en: "the internet link came back after the reboot: " + link.String()})
	}
	last.At = now
	return state, last, true
}

// giveUpResult marks the last result as final once the attempt budget is spent.
func giveUpResult(last wakeRecoveryResult, attempts int, now time.Time) wakeRecoveryResult {
	last = last.with(outcomeFailed, followNone, note{
		zh: fmt.Sprintf("%s（已尝试 %d 次，不再自动重启模块；请手动重启模块或重新插拔）", last.Detail, attempts),
		en: fmt.Sprintf("%s (gave up after %d attempts)", last.logText, attempts),
	})
	last.At = now
	return last
}

func reasonLabel(reason recoveryReason) string {
	switch reason {
	case reasonStartup:
		return "startup check"
	case reasonManual:
		return "manual retry"
	default:
		return string(reason)
	}
}

// attemptLabel names the run about to start, for the log: "wake" for the
// first run of a wake, "retry, attempt 2/10" for the runs after it.
func attemptLabel(state wakeMonitorState) string {
	n := state.Attempts + 1
	if n == 1 || state.Reason == reasonManual {
		return reasonLabel(state.Reason)
	}
	return fmt.Sprintf("%s, attempt %d/%d", reasonLabel(state.Reason), n, wakeRecoveryMaxAttempts)
}

// followUpPlan tells the log what happens after a run, given the state
// finishRecovery returned. It is empty when nothing more is scheduled, or when
// the next tick handles it (a closed lid, or a spent budget).
func followUpPlan(state wakeMonitorState, interactive bool) string {
	if state.Pending || state.RetryAt.IsZero() {
		return ""
	}
	hint := ""
	if interactive {
		hint = " Press Enter to retry now."
	}
	next := state.Attempts + 1
	at := state.RetryAt.Local().Format("15:04:05")
	switch {
	case state.Watching && next > wakeRecoveryMaxAttempts:
		return fmt.Sprintf("All %d automatic attempts used. Watching the internet interface for %s; if it is still missing at %s, automatic reboots stop.%s",
			wakeRecoveryMaxAttempts, durationText(wakeRecoverySettle), at, hint)
	case state.Watching:
		return fmt.Sprintf("Watching the internet interface for %s while USB may still be enumerating; if it is still missing at %s, retrying (attempt %d/%d).%s",
			durationText(wakeRecoverySettle), at, next, wakeRecoveryMaxAttempts, hint)
	case next > wakeRecoveryMaxAttempts:
		return ""
	default:
		return fmt.Sprintf("Retrying in %s at %s (attempt %d/%d).%s",
			durationText(wakeRecoveryRetryDelay), at, next, wakeRecoveryMaxAttempts, hint)
	}
}

// durationText formats d for the log, for example "3 minutes" or "28 seconds".
func durationText(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= time.Minute && d%time.Minute == 0 {
		return plural(int(d/time.Minute), "minute")
	}
	return plural(int(d/time.Second), "second")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func parseClamshellOpen(out string) (open bool, known bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `"AppleClamshellState"`) {
			continue
		}
		switch {
		case strings.Contains(line, "= Yes"):
			return false, true
		case strings.Contains(line, "= No"):
			return true, true
		}
	}
	return false, false
}
