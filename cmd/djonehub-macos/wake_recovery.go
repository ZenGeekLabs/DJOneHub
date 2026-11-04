// Wake recovery brings back the macOS ECM interface after sleep. usbnet=1
// stays stored on the module, but sleep can drop the USB enumeration, so the
// Baiwang service and its 192.168.225.x address disappear. Recovery sends one
// AT+CFUN=1,1 when that interface is missing or has no address. It does not
// rewrite usbnet, and it does not reset the module while the lid is closed,
// because kern.waketime also moves on a USB dark wake.
package main

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// usbNetECM is the AT+QCFG="usbnet" mode that gives macOS a CDC-ECM NIC.
	usbNetECM       = 1
	moduleLANPrefix = "192.168.225."
)

var (
	errRecoveryBusy      = errors.New("module operation in progress")
	errRecoveryCancelled = errors.New("recovery cancelled")
	enInterfacePattern   = regexp.MustCompile(`^en[0-9]+$`)
)

type recoveryReason string

const (
	reasonStartup recoveryReason = "startup"
	reasonWake    recoveryReason = "wake"
	reasonRetry   recoveryReason = "retry"
	reasonManual  recoveryReason = "manual"
	reasonEnabled recoveryReason = "enabled"
)

// recoveryOutcome is the "action" field of the web API. app.js labels each value.
type recoveryOutcome string

const (
	outcomeNone     recoveryOutcome = "none"     // link already up, or usbnet is not the internet mode
	outcomeReset    recoveryOutcome = "reset"    // AT+CFUN=1,1 brought the link back
	outcomeSkipped  recoveryOutcome = "skipped"  // module busy, or AT or the NIC could not be read yet
	outcomeDeferred recoveryOutcome = "deferred" // lid closed; recovery runs once it opens
	outcomeFailed   recoveryOutcome = "failed"   // the link is still not back
)

// followUp tells the wake monitor what to do after a recovery run.
type followUp int

const (
	followNone  followUp = iota
	followRetry          // run again after wakeRecoveryRetryDelay
	followWatch          // watch for the NIC until wakeRecoverySettle, then run again
)

// note is a result message in the two places it appears: Chinese on the web
// page, which has no other language, and English in the log, like the rest
// of the log.
type note struct {
	zh string
	en string
}

func notef(zh, en string, args ...any) note {
	return note{zh: fmt.Sprintf(zh, args...), en: fmt.Sprintf(en, args...)}
}

// because appends err to both texts.
func (n note) because(err error) note {
	return note{zh: n.zh + "：" + err.Error(), en: n.en + ": " + err.Error()}
}

type wakeRecoveryResult struct {
	Reason recoveryReason  `json:"reason,omitempty"`
	Action recoveryOutcome `json:"action,omitempty"`
	Detail string          `json:"detail,omitempty"`
	At     time.Time       `json:"at,omitzero"`
	// logText is the English Detail for the log.
	logText string
	next    followUp
}

func (r wakeRecoveryResult) with(action recoveryOutcome, next followUp, n note) wakeRecoveryResult {
	r.Action = action
	r.next = next
	r.Detail = n.zh
	r.logText = n.en
	return r
}

var (
	noteCancelled = note{zh: "恢复被取消", en: "recovery cancelled"}
	noteBusy      = note{
		zh: "模块正在执行短信、eSIM 或模式切换，稍后再试",
		en: "the module is busy with an SMS, eSIM or usbnet request; trying again later",
	}
)

// skipped reports a run that could not tell whether the link needs a reset.
// It is retried, except when the app is stopping.
func (r wakeRecoveryResult) skipped(what note, err error) wakeRecoveryResult {
	switch {
	case errors.Is(err, errRecoveryCancelled):
		return r.with(outcomeSkipped, followNone, noteCancelled)
	case errors.Is(err, errRecoveryBusy):
		return r.with(outcomeSkipped, followRetry, noteBusy)
	default:
		return r.with(outcomeSkipped, followRetry, what.because(err))
	}
}

// afterReset reports a reset whose result could not be confirmed. USB may
// still be enumerating, so the monitor watches for the NIC before trying again.
func (r wakeRecoveryResult) afterReset(what note, err error) wakeRecoveryResult {
	if errors.Is(err, errRecoveryCancelled) {
		return r.with(outcomeSkipped, followNone, noteCancelled)
	}
	return r.with(outcomeFailed, followWatch, what.because(err))
}

type recoveryPolicy struct {
	ModeAttempts          int // polls for AT+QCFG="usbnet" to answer
	LinkAttempts          int // polls for the NIC before deciding to reset
	PostResetLinkAttempts int // polls for the NIC to re-enumerate after AT+CFUN
	Pause                 time.Duration
}

func defaultRecoveryPolicy() recoveryPolicy {
	return recoveryPolicy{ModeAttempts: 15, LinkAttempts: 10, PostResetLinkAttempts: 20, Pause: 2 * time.Second}
}

type internetRecoveryDeps struct {
	// Enabled is checked right before AT+CFUN, so turning the feature off on
	// the web page stops a run that has not reset the module yet. Nil means on.
	Enabled   func() bool
	QueryMode func() (int, error)
	Observe   func() (dongleLink, error)
	// LockModule takes the module for AT+CFUN=1,1 without waiting. It fails
	// while an SMS, usbnet or eSIM request is running. Nil always succeeds.
	LockModule func() (unlock func(), ok bool)
	// AllowReset is checked right before AT+CFUN. Wake recovery passes false
	// while the lid is closed. Nil allows the reset.
	AllowReset func() bool
	Reset      func() error
	Sleep      func(time.Duration) bool
	// Progress receives one line per step, so the log says what recovery is
	// doing and what it waits for. Nil drops them.
	Progress func(string)
}

func (d internetRecoveryDeps) progress(format string, args ...any) {
	if d.Progress != nil {
		d.Progress(fmt.Sprintf(format, args...))
	}
}

func (d internetRecoveryDeps) lockModule() (func(), bool) {
	if d.LockModule == nil {
		return func() {}, true
	}
	return d.LockModule()
}

func (d internetRecoveryDeps) pause(wait time.Duration) bool {
	if wait <= 0 || d.Sleep == nil {
		return true
	}
	return d.Sleep(wait)
}

func runInternetRecovery(reason recoveryReason, policy recoveryPolicy, deps internetRecoveryDeps) wakeRecoveryResult {
	result := wakeRecoveryResult{Reason: reason}
	mode, err := pollUSBNetMode(policy, deps, "module AT is not answering yet; USB devices need a few seconds to power up after a wake")
	if err != nil {
		return result.skipped(note{zh: "模块 AT 没有及时恢复", en: "module AT did not answer"}, err)
	}
	if mode != usbNetECM {
		return result.with(outcomeNone, followNone,
			notef("当前 usbnet=%d，不是上网模式，不重启模块", "usbnet=%d is not the internet mode; leaving the module alone", mode))
	}
	link, err := pollDongleLink(policy.LinkAttempts, policy, deps, "internet interface not ready; giving macOS a chance to bring it up")
	if err != nil {
		return result.skipped(note{zh: "无法确认 macOS 上网网卡", en: "could not read the macOS network interfaces"}, err)
	}
	switch {
	case link.healthy():
		return result.with(outcomeNone, followNone,
			note{zh: "上网网卡已连通：" + link.describe(), en: "internet link is up: " + link.String()})
	case link.provisional():
		// macOS is still waiting for the module's DHCP reply. A reset would
		// only restart that wait.
		return result.with(outcomeNone, followWatch, notef(
			"上网网卡已出现，当前仍是临时地址 %s，等待模块分配地址",
			"interface is up with placeholder address %s; waiting for the module's DHCP reply", link.IPv4))
	}

	unlock, ok := deps.lockModule()
	if !ok {
		return result.skipped(note{}, errRecoveryBusy)
	}
	result, rebooted := sendReset(result, policy, deps, link)
	unlock()
	if !rebooted {
		return result
	}

	link, err = pollDongleLink(policy.PostResetLinkAttempts, policy, deps, "module rebooted; waiting for macOS to bring up the internet interface")
	if err != nil {
		return result.afterReset(note{zh: "模块已重启，但没有读到上网网卡", en: "module rebooted, but the network interfaces could not be read"}, err)
	}
	switch {
	case link.healthy():
		return result.with(outcomeReset, followNone,
			note{zh: "已重启模块，上网网卡恢复：" + link.describe(), en: "module rebooted; internet link is back: " + link.String()})
	case link.provisional():
		return result.with(outcomeReset, followWatch, notef(
			"已重启模块，网卡已出现，当前仍是临时地址 %s",
			"module rebooted; interface is up with placeholder address %s", link.IPv4))
	default:
		return result.with(outcomeFailed, followWatch,
			note{zh: "已重启模块，上网网卡仍未恢复：" + link.describe(), en: "module rebooted, but the internet link is still down: " + link.String()})
	}
}

// sendReset sends AT+CFUN=1,1 and waits for AT to answer again. The caller
// holds the module lock for this window, so a queued SMS or eSIM request
// waits for the module instead of running into the reboot.
func sendReset(result wakeRecoveryResult, policy recoveryPolicy, deps internetRecoveryDeps, link dongleLink) (wakeRecoveryResult, bool) {
	if deps.Enabled != nil && !deps.Enabled() {
		return result.with(outcomeSkipped, followNone,
			note{zh: "唤醒恢复已在网页上关闭，没有重启模块", en: "turned off on the web page; the module was not rebooted"}), false
	}
	if deps.AllowReset != nil && !deps.AllowReset() {
		return result.with(outcomeDeferred, followNone,
			note{zh: "合盖休眠中，等开盖后再恢复上网网卡", en: "the lid is closed; recovery continues once it opens"}), false
	}
	deps.progress("macOS still has no internet interface (%s); rebooting the module", link)
	if err := deps.Reset(); err != nil {
		// The command may still have reached the module, so watch before
		// sending another one.
		return result.with(outcomeFailed, followWatch, note{zh: "重启模块失败", en: "module reboot failed"}.because(err)), false
	}
	if _, err := pollUSBNetMode(policy, deps, "waiting for the module to finish rebooting and USB AT to reconnect"); err != nil {
		return result.afterReset(note{zh: "模块已重启，但 AT 没有重新连上", en: "module rebooted, but AT did not come back"}, err), false
	}
	return result, true
}

// pollUSBNetMode queries usbnet until AT answers. If the first query fails,
// it logs waitingFor with the current error and the longest remaining wait.
func pollUSBNetMode(policy recoveryPolicy, deps internetRecoveryDeps, waitingFor string) (int, error) {
	var mode int
	var lastErr error
	err := pollAttempts(policy.ModeAttempts, policy, deps, func() bool {
		mode, lastErr = deps.QueryMode()
		return lastErr == nil
	}, func(remaining time.Duration) {
		deps.progress("%s (now: %v); waiting up to about %s", waitingFor, lastErr, durationText(remaining))
	})
	if err != nil {
		return -1, err
	}
	if lastErr != nil {
		return -1, lastErr
	}
	return mode, nil
}

// pollDongleLink observes the NIC until it is healthy or the attempts run out,
// and returns the last observation. It fails only if no observation succeeded.
// If the first observation is not healthy, it logs waitingFor like pollUSBNetMode.
func pollDongleLink(attempts int, policy recoveryPolicy, deps internetRecoveryDeps, waitingFor string) (dongleLink, error) {
	var last dongleLink
	var lastErr error
	seen := false
	err := pollAttempts(attempts, policy, deps, func() bool {
		link, observeErr := deps.Observe()
		if observeErr != nil {
			lastErr = observeErr
			return false
		}
		last, seen = link, true
		return link.healthy()
	}, func(remaining time.Duration) {
		current := last.String()
		if !seen {
			current = "unreadable: " + lastErr.Error()
		}
		deps.progress("%s (now: %s); waiting up to about %s", waitingFor, current, durationText(remaining))
	})
	if err != nil {
		return last, err
	}
	if !seen {
		return dongleLink{}, lastErr
	}
	return last, nil
}

// pollAttempts runs step until it reports done or attempts run out, pausing
// between tries. Before the first pause it calls waiting with the longest
// remaining wait. It returns errRecoveryCancelled if a pause is cut short.
func pollAttempts(attempts int, policy recoveryPolicy, deps internetRecoveryDeps, step func() bool, waiting func(remaining time.Duration)) error {
	for i := 1; ; i++ {
		if step() || i >= attempts {
			return nil
		}
		if i == 1 {
			waiting(time.Duration(attempts-1) * policy.Pause)
		}
		if !deps.pause(policy.Pause) {
			return errRecoveryCancelled
		}
	}
}

// holdModuleOp marks a request that must not be cut off by a module reboot.
// Wake recovery does not send AT+CFUN=1,1 while one is running.
func (a *app) holdModuleOp() func() {
	a.moduleOps.RLock()
	return a.moduleOps.RUnlock
}

func (a *app) tryLockModule() (func(), bool) {
	if !a.moduleOps.TryLock() {
		return nil, false
	}
	return a.moduleOps.Unlock, true
}

func (a *app) readUSBNetMode() (int, error) {
	response, err := a.runATCommand(`AT+QCFG="usbnet"`, 5*time.Second)
	if err != nil {
		return -1, err
	}
	parsed := parseUSBNetMode(response)
	if parsed == "" {
		return -1, fmt.Errorf("usbnet mode missing in %q", response)
	}
	return strconv.Atoi(parsed)
}

func (a *app) resetModuleForRecovery() error {
	viaUSBAT := a.modem == nil
	if viaUSBAT {
		a.beginModuleReboot()
	}
	_, err := a.runATCommand("AT+CFUN=1,1", 8*time.Second)
	if viaUSBAT {
		// Drop the old libusb handle right away. A QCFG reply on that handle
		// can arrive before the bus actually disconnects, and treating it as
		// "AT is back" makes the NIC check time out while the module is
		// still rebooting.
		a.markUSBATDetached("module reset")
	}
	if err == nil || resetCutOff(err) {
		return nil
	}
	if viaUSBAT {
		a.abortModuleReboot(err)
	}
	return err
}

func (a *app) setWakeRecovery(result wakeRecoveryResult) {
	a.recoveryMu.Lock()
	a.recoveryResult = result
	a.recoveryMu.Unlock()
}

func (a *app) wakeRecoverySnapshot() wakeRecoveryResult {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	return a.recoveryResult
}

// wakeRecoveryStatus is the "wake_recovery" object of GET /api/network: the
// switch on the web page plus the last result.
type wakeRecoveryStatus struct {
	Enabled bool `json:"enabled"`
	// ForcedOff means -wake-recovery=false, which the page cannot override.
	ForcedOff bool `json:"forced_off,omitempty"`
	wakeRecoveryResult
}

func (a *app) wakeRecoveryStatus() wakeRecoveryStatus {
	return wakeRecoveryStatus{
		Enabled:            a.wakeRecoveryEnabled(),
		ForcedOff:          a.wakeRecoveryForcedOff,
		wakeRecoveryResult: a.wakeRecoverySnapshot(),
	}
}

func (a *app) wakeRecoveryEnabled() bool {
	if a.wakeRecoveryForcedOff {
		return false
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	return a.settings.wakeRecoveryOn()
}

func (a *app) setWakeRecoveryEnabled(on bool) error {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	next := a.settings
	next.WakeRecovery = &on
	if a.settingsPath != "" {
		if err := saveSettings(a.settingsPath, next); err != nil {
			return err
		}
	}
	a.settings = next
	return nil
}

// updateWakeRecoverySetting handles PUT /api/network/wake-recovery. The wake
// monitor picks the change up on its next tick.
func (a *app) updateWakeRecoverySetting(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if a.wakeRecoveryForcedOff {
		writeError(w, http.StatusConflict, "唤醒恢复已通过启动参数 -wake-recovery=false 关闭；去掉该参数并重启 DJOneHub 后才能在网页上开启")
		return
	}
	if err := a.setWakeRecoveryEnabled(*body.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.wakeRecoveryStatus())
}

type hardwarePort struct {
	Name   string
	Device string
}

// dongleLink is the macOS Ethernet service created by usbnet=1 (CDC-ECM).
// On this module that service is named Baiwang and gets a 192.168.225.x address.
type dongleLink struct {
	Port   string
	Name   string
	Status string
	IPv4   string
	Found  bool
}

func (l dongleLink) active() bool {
	return l.Found && strings.EqualFold(l.Status, "active")
}

func (l dongleLink) healthy() bool {
	return l.active() && usableIPv4(l.IPv4) && !l.provisional()
}

// provisional is the placeholder address (192.0.0.2) that macOS puts on a
// freshly enumerated USB NIC before the module's DHCP server offers 192.168.225.x.
func (l dongleLink) provisional() bool {
	return l.active() && strings.HasPrefix(l.IPv4, "192.0.0.")
}

// String describes the link for the log.
func (l dongleLink) String() string {
	if !l.Found {
		return "no Baiwang or 192.168.225.x interface"
	}
	ip := l.IPv4
	if !usableIPv4(ip) {
		ip = "no IPv4"
	}
	name := cmp.Or(l.Name, l.Port, "unknown interface")
	return fmt.Sprintf("%s %s %s", name, cmp.Or(l.Status, "unknown"), ip)
}

// describe is String for the web page.
func (l dongleLink) describe() string {
	if !l.Found {
		return "未发现 Baiwang / 192.168.225.x 网卡"
	}
	ip := l.IPv4
	if !usableIPv4(ip) {
		ip = "无 IPv4"
	}
	return fmt.Sprintf("%s %s %s", cmp.Or(l.Name, l.Port, "未知接口"), cmp.Or(l.Status, "unknown"), ip)
}

func usableIPv4(ip string) bool {
	return ip != "" && ip != "0.0.0.0"
}

func donglePortName(name string) bool {
	folded := strings.ToLower(name)
	return strings.Contains(folded, "baiwang") ||
		strings.Contains(folded, "dji") ||
		strings.Contains(folded, "quectel")
}

func isModuleLAN(ip string) bool {
	return strings.HasPrefix(ip, moduleLANPrefix) && usableIPv4(ip)
}

func parseHardwarePorts(out string) []hardwarePort {
	var ports []hardwarePort
	var current hardwarePort
	flush := func() {
		if current.Name == "" && current.Device == "" {
			return
		}
		ports = append(ports, current)
		current = hardwarePort{}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Hardware Port:"):
			flush()
			current.Name = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		case strings.HasPrefix(line, "Device:"):
			current.Device = strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
		}
	}
	flush()
	return ports
}

func selectDongleLink(ports []hardwarePort, ifaces []macNetInterface) dongleLink {
	byName := make(map[string]macNetInterface, len(ifaces))
	for _, iface := range ifaces {
		byName[iface.Name] = iface
	}
	var found []dongleLink
	seen := make(map[string]bool)
	for _, port := range ports {
		if !donglePortName(port.Name) || !enInterfacePattern.MatchString(port.Device) {
			continue
		}
		found = append(found, linkFromInterface(port.Name, port.Device, byName[port.Device]))
		seen[port.Device] = true
	}
	for _, iface := range ifaces {
		if seen[iface.Name] || !isModuleLAN(iface.IPv4) {
			continue
		}
		found = append(found, linkFromInterface("", iface.Name, iface))
	}
	best := dongleLink{}
	for _, link := range found {
		if linkRank(link) > linkRank(best) {
			best = link
		}
	}
	return best
}

func linkFromInterface(portName, device string, iface macNetInterface) dongleLink {
	link := dongleLink{Port: portName, Name: device, Found: true}
	if iface.Name == "" {
		link.Status = "missing"
		return link
	}
	link.Name = iface.Name
	link.Status = iface.Status
	link.IPv4 = iface.IPv4
	return link
}

func linkRank(link dongleLink) int {
	switch {
	case !link.Found:
		return 0
	case link.healthy():
		return 3
	case link.active():
		return 2
	default:
		return 1
	}
}
