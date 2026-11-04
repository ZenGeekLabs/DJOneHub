package main

import "errors"

// libusb error codes from libusb.h. They are part of the libusb ABI, so builds
// without cgo can classify errors without the header.
const (
	libusbErrorNoDevice = -4
	libusbErrorNotFound = -5
	libusbErrorTimeout  = -7
	libusbErrorOther    = -99
)

// usbError is a failed libusb transfer. Its message keeps the libusb name
// (for example "USB bulk write: LIBUSB_ERROR_NO_DEVICE").
type usbError struct {
	Op   string
	Code int
	Name string
}

func (e *usbError) Error() string {
	return e.Op + ": " + e.Name
}

// errUSBATNoResponse means a command was written but the module never answered.
var errUSBATNoResponse = errors.New("USB AT command timed out without response")

func usbErrorCode(err error) (int, bool) {
	var usbErr *usbError
	if !errors.As(err, &usbErr) {
		return 0, false
	}
	return usbErr.Code, true
}

// usbATDropped reports whether err means the USB AT handle no longer reaches
// the module, so it has to be closed and reopened after USB re-enumerates.
func usbATDropped(err error) bool {
	if errors.Is(err, errUSBATNoResponse) {
		return true
	}
	code, ok := usbErrorCode(err)
	return ok && (code == libusbErrorNoDevice || code == libusbErrorNotFound || code == libusbErrorOther)
}

// resetCutOff reports whether AT+CFUN=1,1 failed only because the module left
// USB to reboot before its reply arrived.
func resetCutOff(err error) bool {
	if usbATDropped(err) {
		return true
	}
	code, ok := usbErrorCode(err)
	return ok && code == libusbErrorTimeout
}
