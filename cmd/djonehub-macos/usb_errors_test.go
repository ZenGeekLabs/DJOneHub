package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestUSBErrorClassification(t *testing.T) {
	usbErr := func(code int, name string) error {
		return fmt.Errorf("set SMS PDU mode: %w", &usbError{Op: "USB bulk write", Code: code, Name: name})
	}
	noDevice := usbErr(libusbErrorNoDevice, "LIBUSB_ERROR_NO_DEVICE")
	other := usbErr(libusbErrorOther, "LIBUSB_ERROR_OTHER")
	timeout := usbErr(libusbErrorTimeout, "LIBUSB_ERROR_TIMEOUT")
	noResponse := fmt.Errorf("select storage: %w", errUSBATNoResponse)
	notConnected := errors.New("DJI USB device is not connected")

	for _, err := range []error{noDevice, other, noResponse} {
		if !usbATDropped(err) || !resetCutOff(err) {
			t.Fatalf("%v should drop the handle and count as a reset cut-off", err)
		}
	}
	if usbATDropped(timeout) || !resetCutOff(timeout) {
		t.Fatal("a write timeout during AT+CFUN is the module rebooting, not a dropped handle")
	}
	if usbATDropped(notConnected) || resetCutOff(notConnected) || usbATDropped(nil) {
		t.Fatal("a missing device is not proof that AT+CFUN was accepted")
	}
	if noDevice.Error() != "set SMS PDU mode: USB bulk write: LIBUSB_ERROR_NO_DEVICE" {
		t.Fatalf("message = %q", noDevice.Error())
	}

	a := &app{usbAT: &usbAT{}}
	a.resetUSBATIfGone(other)
	if a.currentUSBAT() != nil {
		t.Fatal("LIBUSB_ERROR_OTHER should detach the AT bridge")
	}
}
