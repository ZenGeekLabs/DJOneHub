//go:build darwin && cgo

package main

/*
#cgo pkg-config: libusb-1.0
#include <stdlib.h>
#include <libusb.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	djiUSBVendorID  = 0x2c7c
	djiUSBProductID = 0x0125
)

type usbAT struct {
	ctx         *C.libusb_context
	handle      *C.libusb_device_handle
	iface       int
	endpointIn  byte
	endpointOut byte
	mu          sync.Mutex
}

type usbATCandidate struct {
	iface       int
	endpointIn  byte
	endpointOut byte
}

func debug(args ...interface{}) {
	fmt.Fprintln(os.Stderr, append([]interface{}{"[DJOneHub-DEBUG]"}, args...)...)
}

func openDJIUSBAT() (*usbAT, error) {
	debug("openDJIUSBAT: starting")
	var ctx *C.libusb_context
	debug("calling libusb_init")
	if rc := C.libusb_init(&ctx); rc != 0 {
		return nil, fmt.Errorf("libusb init: %s", usbErrorName(rc))
	}
	debug("libusb_init OK, trying to open", fmt.Sprintf("%04x:%04x", djiUSBVendorID, djiUSBProductID))
	handle := C.libusb_open_device_with_vid_pid(ctx, djiUSBVendorID, djiUSBProductID)
	if handle == nil {
		debug("device not found by VID/PID")
		C.libusb_exit(ctx)
		return nil, errors.New("USB AT device 2c7c:0125 not found")
	}
	debug("device opened OK")
	candidates, err := usbATCandidates(handle)
	if err != nil {
		debug("usbATCandidates error:", err)
		C.libusb_close(handle)
		C.libusb_exit(ctx)
		return nil, err
	}
	debug("found", len(candidates), "interface candidates")
	var lastErr error
	for i, candidate := range candidates {
		debug(fmt.Sprintf("candidate %d: iface=%d out=0x%02x in=0x%02x", i, candidate.iface, candidate.endpointOut, candidate.endpointIn))
		debug("claiming interface", candidate.iface)
		if rc := C.libusb_claim_interface(handle, C.int(candidate.iface)); rc != 0 {
			lastErr = fmt.Errorf("claim USB AT interface %d: %s", candidate.iface, usbErrorName(rc))
			debug("claim failed:", lastErr)
			continue
		}
		debug("interface claimed, sending AT probe")
		dev := &usbAT{
			ctx:         ctx,
			handle:      handle,
			iface:       candidate.iface,
			endpointIn:  candidate.endpointIn,
			endpointOut: candidate.endpointOut,
		}
		if response, err := dev.Command("AT", 900*time.Millisecond); err == nil && atProbeSucceeded(response) {
			debug("AT probe succeeded!")
			return dev, nil
		} else {
			if err == nil {
				err = fmt.Errorf("unexpected AT probe response %q", response)
			}
			lastErr = fmt.Errorf("probe USB AT interface %d out 0x%02x in 0x%02x: %w",
				candidate.iface, candidate.endpointOut, candidate.endpointIn, err)
			debug("probe failed:", lastErr)
		}
		C.libusb_release_interface(handle, C.int(candidate.iface))
	}
	debug("all candidates failed")
	C.libusb_close(handle)
	C.libusb_exit(ctx)
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no USB bulk interface candidates found for AT bridge")
}

func usbATCandidates(handle *C.libusb_device_handle) ([]usbATCandidate, error) {
	dev := C.libusb_get_device(handle)
	if dev == nil {
		return nil, errors.New("libusb device handle has no device")
	}
	var config *C.struct_libusb_config_descriptor
	if rc := C.libusb_get_active_config_descriptor(dev, &config); rc != 0 {
		return nil, fmt.Errorf("get active USB config descriptor: %s", usbErrorName(rc))
	}
	defer C.libusb_free_config_descriptor(config)

	var candidates []usbATCandidate
	interfaces := unsafe.Slice(config._interface, int(config.bNumInterfaces))
	for _, intf := range interfaces {
		altsettings := unsafe.Slice(intf.altsetting, int(intf.num_altsetting))
		for _, alt := range altsettings {
			var endpointIn, endpointOut byte
			endpoints := unsafe.Slice(alt.endpoint, int(alt.bNumEndpoints))
			for _, ep := range endpoints {
				attrs := byte(ep.bmAttributes) & byte(C.LIBUSB_TRANSFER_TYPE_MASK)
				if attrs != byte(C.LIBUSB_TRANSFER_TYPE_BULK) {
					continue
				}
				addr := byte(ep.bEndpointAddress)
				if addr&byte(C.LIBUSB_ENDPOINT_IN) != 0 {
					endpointIn = addr
				} else {
					endpointOut = addr
				}
			}
			if endpointIn != 0 && endpointOut != 0 {
				candidates = append(candidates, usbATCandidate{
					iface:       int(alt.bInterfaceNumber),
					endpointIn:  endpointIn,
					endpointOut: endpointOut,
				})
			}
		}
	}
	return candidates, nil
}

func (u *usbAT) Close() {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.handle == nil {
		return
	}
	C.libusb_release_interface(u.handle, C.int(u.iface))
	C.libusb_close(u.handle)
	C.libusb_exit(u.ctx)
	u.handle = nil
	u.ctx = nil
}

func (u *usbAT) Command(cmd string, timeout time.Duration) (string, error) {
	if u == nil {
		return "", errors.New("USB AT device is not open")
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", errors.New("AT command is empty")
	}
	if !strings.HasPrefix(strings.ToUpper(cmd), "AT") {
		return "", errors.New("command must start with AT")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.handle == nil {
		return "", errors.New("USB AT device is not open")
	}

	u.drainLocked()
	payload := []byte(cmd + "\r")
	if err := u.bulkWriteLocked(u.endpointOut, payload, timeout); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var chunks []string
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 900*time.Millisecond {
			remaining = 900 * time.Millisecond
		}
		data, err := u.bulkReadLocked(u.endpointIn, remaining)
		if err != nil {
			if errors.Is(err, errUSBTimeout) {
				continue
			}
			return strings.Join(chunks, ""), err
		}
		if len(data) == 0 {
			continue
		}
		chunks = append(chunks, string(data))
		joined := strings.Join(chunks, "")
		if atResponseComplete(joined) {
			return normalizeATResponse(joined), nil
		}
	}
	if len(chunks) == 0 {
		return "", errors.New("USB AT command timed out without response")
	}
	return normalizeATResponse(strings.Join(chunks, "")), errors.New("USB AT command timed out before completion")
}

func (u *usbAT) CommandWithPrompt(cmd string, followUp []byte, timeout time.Duration) (string, error) {
	if u == nil {
		return "", errors.New("USB AT device is not open")
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", errors.New("AT command is empty")
	}
	if !strings.HasPrefix(strings.ToUpper(cmd), "AT") {
		return "", errors.New("command must start with AT")
	}
	if len(followUp) == 0 {
		return "", errors.New("interactive AT follow-up is empty")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.handle == nil {
		return "", errors.New("USB AT device is not open")
	}

	u.drainLocked()
	if err := u.bulkWriteLocked(u.endpointOut, []byte(cmd+"\r"), timeout); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var response strings.Builder
	promptReceived := false
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 900*time.Millisecond {
			remaining = 900 * time.Millisecond
		}
		data, err := u.bulkReadLocked(u.endpointIn, remaining)
		if err != nil {
			if errors.Is(err, errUSBTimeout) {
				continue
			}
			return normalizeATResponse(response.String()), err
		}
		if len(data) == 0 {
			continue
		}
		response.Write(data)
		joined := response.String()

		if !promptReceived {
			if atResponseIsError(joined) {
				return normalizeATResponse(joined), nil
			}
			if !atResponseHasPrompt(joined) {
				continue
			}
			if err := u.bulkWriteLocked(u.endpointOut, followUp, time.Until(deadline)); err != nil {
				return normalizeATResponse(joined), err
			}
			promptReceived = true
			continue
		}

		if atResponseComplete(joined) {
			return normalizeATResponse(joined), nil
		}
	}

	if promptReceived {
		_ = u.bulkWriteLocked(u.endpointOut, []byte{0x1b}, 300*time.Millisecond)
	}
	if response.Len() == 0 {
		return "", errors.New("USB interactive AT command timed out without response")
	}
	return normalizeATResponse(response.String()), errors.New("USB interactive AT command timed out before completion")
}

var errUSBTimeout = errors.New("usb timeout")

func (u *usbAT) drainLocked() {
	for {
		if _, err := u.bulkReadLocked(u.endpointIn, 80*time.Millisecond); err != nil {
			return
		}
	}
}

func (u *usbAT) Description() string {
	if u == nil {
		return "USB AT"
	}
	return fmt.Sprintf("USB AT · 2c7c:0125 interface %d out 0x%02x in 0x%02x",
		u.iface, u.endpointOut, u.endpointIn)
}

func (u *usbAT) bulkWriteLocked(endpoint byte, payload []byte, timeout time.Duration) error {
	var transferred C.int
	ptr := unsafe.Pointer(&payload[0])
	rc := C.libusb_bulk_transfer(
		u.handle,
		C.uchar(endpoint),
		(*C.uchar)(ptr),
		C.int(len(payload)),
		&transferred,
		C.uint(timeout.Milliseconds()),
	)
	if rc != 0 {
		return fmt.Errorf("USB bulk write: %s", usbErrorName(rc))
	}
	if int(transferred) != len(payload) {
		return fmt.Errorf("USB bulk write short transfer: %d/%d", int(transferred), len(payload))
	}
	return nil
}

func (u *usbAT) bulkReadLocked(endpoint byte, timeout time.Duration) ([]byte, error) {
	buf := make([]byte, 512)
	var transferred C.int
	rc := C.libusb_bulk_transfer(
		u.handle,
		C.uchar(endpoint),
		(*C.uchar)(unsafe.Pointer(&buf[0])),
		C.int(len(buf)),
		&transferred,
		C.uint(timeout.Milliseconds()),
	)
	if rc == C.LIBUSB_ERROR_TIMEOUT {
		return nil, errUSBTimeout
	}
	if rc != 0 {
		return nil, fmt.Errorf("USB bulk read: %s", usbErrorName(rc))
	}
	return buf[:int(transferred)], nil
}

func usbErrorName(rc C.int) string {
	return C.GoString(C.libusb_error_name(rc))
}

func atResponseComplete(resp string) bool {
	normalized := strings.ReplaceAll(resp, "\r\n", "\n")
	return strings.Contains(normalized, "\nOK\n") ||
		strings.HasSuffix(normalized, "\nOK") ||
		atResponseIsError(normalized)
}

func atResponseIsError(resp string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(resp, "\r\n", "\n"))
	return strings.Contains(normalized, "\nERROR\n") ||
		strings.HasSuffix(normalized, "\nERROR") ||
		strings.Contains(normalized, "+CME ERROR:") ||
		strings.Contains(normalized, "+CMS ERROR:")
}

func atResponseHasPrompt(resp string) bool {
	trimmed := strings.TrimRight(resp, " \t\r\n")
	return strings.HasSuffix(trimmed, ">")
}

func atProbeSucceeded(resp string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(resp), "\r\n", "\n")
	return normalized == "OK" || strings.HasSuffix(normalized, "\nOK")
}

func normalizeATResponse(resp string) string {
	resp = strings.ReplaceAll(resp, "\r\r\n", "\r\n")
	resp = strings.TrimSpace(resp)
	lines := strings.Split(resp, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\r\n")
}
