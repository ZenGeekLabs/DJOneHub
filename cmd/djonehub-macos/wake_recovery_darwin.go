//go:build darwin

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	wakeMonitorTick = 2 * time.Second
	// wakeMonitorStartDelay lets the module settle after the app starts
	// before the startup check runs.
	wakeMonitorStartDelay = 5 * time.Second
	// wakeSettleDelay gives USB a moment after a wake before the lid is
	// checked again and the module is touched.
	wakeSettleDelay = 3 * time.Second
	// macToolTimeout bounds ioreg and networksetup, which can stall while
	// the system is going to sleep or waking up.
	macToolTimeout = 10 * time.Second
)

type wakeMonitor struct {
	app *app
	// enter delivers Enter presses from the terminal. It is nil when stdin
	// is not this process's terminal, and then the log offers no Enter hint.
	enter   <-chan struct{}
	running atomic.Bool
}

func startWakeMonitor(ctx context.Context, a *app) {
	m := &wakeMonitor{app: a}
	m.enter = m.watchEnterKey()
	go m.run(ctx)
}

func logRecovery(format string, args ...any) {
	log.Printf("wake recovery: "+format, args...)
}

func (m *wakeMonitor) interactive() bool {
	return m.enter != nil
}

func (m *wakeMonitor) run(ctx context.Context) {
	last, err := readSystemWakeTime()
	if err != nil {
		logRecovery("disabled, cannot read the system wake time: %v", err)
		return
	}
	on := m.app.wakeRecoveryEnabled()
	if on {
		lastText := "no sleep since boot"
		if !last.IsZero() {
			lastText = "last wake " + last.Local().Format(time.RFC3339)
		}
		enterText := ""
		if m.interactive() {
			enterText = " Press Enter in this terminal to retry at any time."
		}
		logRecovery("armed (%s). Checking the internet link in %s and after every wake.%s", lastText, durationText(wakeMonitorStartDelay), enterText)
	} else {
		logRecovery("turned off on the web page. Turning it back on in the Network tab checks the internet link right away.")
	}
	if !sleepCtx(ctx, wakeMonitorStartDelay) {
		return
	}
	state := wakeMonitorState{LastWake: last, Pending: on, Reason: reasonStartup}
	ticker := time.NewTicker(wakeMonitorTick)
	defer ticker.Stop()
	for {
		if enabled := m.app.wakeRecoveryEnabled(); enabled != on {
			on = enabled
			if on {
				wake, _ := readSystemWakeTime() // zero on error; resumeRecovery then keeps LastWake
				state = resumeRecovery(state, wake)
				logRecovery("turned on from the web page; checking the internet link now")
			} else {
				state = pauseRecovery(state)
				logRecovery("turned off from the web page; wakes no longer check the internet link or reboot the module")
			}
		}
		if on {
			state = m.advance(ctx, state)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.enter:
			if !on {
				logRecovery("turned off on the web page, so Enter does nothing; turn it on in the Network tab first")
				continue
			}
			logRecovery("Enter pressed; retrying now (a manual retry ignores the lid state and the attempt limit)")
			state = requestManualRecovery(state)
		}
	}
}

func (m *wakeMonitor) advance(ctx context.Context, state wakeMonitorState) wakeMonitorState {
	sig, err := readWakeSignal(ctx, state)
	if err != nil {
		return state
	}
	state, action := nextWakeMonitorAction(state, sig)
	switch action {
	case wakeMonitorWaitForLid:
		if !state.LoggedWait {
			hint := ""
			if m.interactive() {
				hint = " In clamshell mode with an external display, press Enter to recover manually."
			}
			logRecovery("the lid is closed (last wake %s); waiting for it to open before touching the module. A wake with the lid closed is usually a brief USB dark wake, and a module rebooted then cannot bring its interface back.%s",
				state.LastWake.Local().Format(time.RFC3339), hint)
			state.LoggedWait = true
		}
	case wakeMonitorRecover:
		state = m.recover(ctx, state)
	case wakeMonitorObserve:
		link, err := discoverDongleLink(ctx)
		if err != nil {
			return state
		}
		var result wakeRecoveryResult
		var changed bool
		state, result, changed = observeLateLink(state, m.app.wakeRecoverySnapshot(), link, time.Now())
		if changed {
			m.app.setWakeRecovery(result)
			logRecovery("%s", result.logText)
		}
	case wakeMonitorGiveUp:
		m.app.setWakeRecovery(giveUpResult(m.app.wakeRecoverySnapshot(), state.Attempts, time.Now()))
		hint := ""
		if m.interactive() {
			hint = ", or press Enter in this terminal to retry"
		}
		logRecovery("the internet link is still down after %d attempts; no more automatic reboots for this wake. Reboot the module from the web page or replug it%s.", state.Attempts, hint)
	}
	return state
}

func (m *wakeMonitor) recover(ctx context.Context, state wakeMonitorState) wakeMonitorState {
	m.running.Store(true)
	defer m.running.Store(false)
	if state.Reason == reasonWake {
		logRecovery("system woke at %s; USB devices need a few seconds to power up, checking in %s",
			state.LastWake.Local().Format("15:04:05"), durationText(wakeSettleDelay))
		if !sleepCtx(ctx, wakeSettleDelay) {
			return state
		}
		if open, known := lidIsOpen(ctx); known && !open {
			return waitForLid(state)
		}
	}
	label := attemptLabel(state)
	logRecovery("starting (%s): checking the module's usbnet mode and the macOS internet interface", label)
	result := m.app.recoverInternetLink(ctx, state.Reason)

	sig := wakeSignal{Now: time.Now()}
	sig.Wake, _ = readSystemWakeTime() // zero on error; finishRecovery then ignores it
	sig.LidOpen, sig.LidKnown = lidIsOpen(ctx)
	state = finishRecovery(state, result, sig)
	m.app.setWakeRecovery(result)
	logRecovery("result (%s): %s", label, result.logText)
	if plan := followUpPlan(state, m.interactive()); plan != "" {
		logRecovery("%s", plan)
	}
	return state
}

// watchEnterKey turns each Enter typed in the terminal into a manual retry.
// It only reads stdin when stdin is a terminal and this process is its
// foreground job, so a service started by the launcher or launchd never does.
func (m *wakeMonitor) watchEnterKey() <-chan struct{} {
	fd := int(os.Stdin.Fd())
	foreground, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil || foreground != unix.Getpgrp() {
		return nil
	}
	// Reading the terminal after being moved to the background would stop
	// the process with SIGTTIN. With the signal ignored the read fails, and
	// the loop below ends instead.
	signal.Ignore(syscall.SIGTTIN)
	presses := make(chan struct{}, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}
			if m.running.Load() {
				logRecovery("recovery is already running; Enter ignored, press it again once this run ends")
				continue
			}
			select {
			case presses <- struct{}{}:
			default:
			}
		}
	}()
	return presses
}

// recoverInternetLink runs one recovery pass against the module and macOS. A
// manual run skips the lid check: the user is typing, so the Mac is awake.
func (a *app) recoverInternetLink(ctx context.Context, reason recoveryReason) wakeRecoveryResult {
	result := runInternetRecovery(reason, defaultRecoveryPolicy(), internetRecoveryDeps{
		Enabled:    a.wakeRecoveryEnabled,
		QueryMode:  a.readUSBNetMode,
		Observe:    func() (dongleLink, error) { return discoverDongleLink(ctx) },
		LockModule: a.tryLockModule,
		AllowReset: func() bool {
			if reason == reasonManual {
				return true
			}
			open, known := lidIsOpen(ctx)
			return !known || open
		},
		Reset:    a.resetModuleForRecovery,
		Sleep:    func(d time.Duration) bool { return sleepCtx(ctx, d) },
		Progress: func(line string) { logRecovery("%s", line) },
	})
	result.At = time.Now()
	return result
}

// readWakeSignal reads kern.waketime every tick and the lid only when the
// monitor state needs it, so an idle tick does not start a process.
func readWakeSignal(ctx context.Context, state wakeMonitorState) (wakeSignal, error) {
	wake, err := readSystemWakeTime()
	if err != nil {
		return wakeSignal{}, err
	}
	sig := wakeSignal{Wake: wake, Now: time.Now()}
	if state.needsLid(sig) {
		sig.LidOpen, sig.LidKnown = lidIsOpen(ctx)
	}
	return sig, nil
}

// readSystemWakeTime returns the last wake, or the zero time if the Mac has
// not slept since boot.
func readSystemWakeTime() (time.Time, error) {
	tv, err := unix.SysctlTimeval("kern.waketime")
	if err != nil {
		return time.Time{}, fmt.Errorf("read kern.waketime: %w", err)
	}
	if tv.Sec == 0 && tv.Usec == 0 {
		return time.Time{}, nil
	}
	return time.Unix(tv.Sec, int64(tv.Usec)*int64(time.Microsecond)), nil
}

func lidIsOpen(ctx context.Context) (open bool, known bool) {
	out, err := macToolOutput(ctx, "/usr/sbin/ioreg", "-r", "-n", "IOPMrootDomain", "-d", "1")
	if err != nil {
		return false, false
	}
	return parseClamshellOpen(string(out))
}

func discoverDongleLink(ctx context.Context) (dongleLink, error) {
	portOut, portErr := macToolOutput(ctx, "/usr/sbin/networksetup", "-listallhardwareports")
	ifaces := discoverMacNetworkInterfaces()
	var ports []hardwarePort
	if portErr == nil {
		ports = parseHardwarePorts(string(portOut))
	}
	link := selectDongleLink(ports, ifaces)
	if portErr != nil && !link.Found {
		return dongleLink{}, fmt.Errorf("list hardware ports: %w", portErr)
	}
	return link, nil
}

func macToolOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, macToolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", name, macToolTimeout)
	}
	return out, err
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
