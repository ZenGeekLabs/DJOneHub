package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPortScore(t *testing.T) {
	tests := []struct {
		name string
		port string
		want int
	}{
		{name: "named Quectel port", port: "/dev/cu.Quectel-AT", want: 100},
		{name: "usb modem", port: "/dev/cu.usbmodem2101", want: 80},
		{name: "usb serial", port: "/dev/cu.usbserial-1420", want: 60},
		{name: "bluetooth", port: "/dev/cu.Bluetooth-Incoming-Port", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := portScore(tt.port); got != tt.want {
				t.Fatalf("portScore(%q) = %d, want %d", tt.port, got, tt.want)
			}
		})
	}
}

func TestParseUSBNetMode(t *testing.T) {
	for _, tt := range []struct {
		response string
		want     string
	}{
		{response: "AT+QCFG=\"usbnet\"\r\n+QCFG: \"usbnet\",0\r\nOK", want: "0"},
		{response: "+QCFG: \"usbnet\",1\r\nOK", want: "1"},
		{response: "ERROR", want: ""},
	} {
		if got := parseUSBNetMode(tt.response); got != tt.want {
			t.Fatalf("parseUSBNetMode(%q) = %q, want %q", tt.response, got, tt.want)
		}
	}
}

func TestParseUSBATOperator(t *testing.T) {
	for _, tt := range []struct {
		name     string
		response string
		want     string
	}{
		{
			name:     "known numeric PLMN",
			response: "AT+COPS?\r\n+COPS: 0,2,\"46015\",7\r\nOK",
			want:     "中国广电",
		},
		{
			name:     "long operator name",
			response: "+COPS: 0,0,\"CHN-UNICOM\",7\r\nOK",
			want:     "CHN-UNICOM",
		},
		{
			name:     "missing operator",
			response: "+COPS: 0\r\nOK",
			want:     "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseUSBATOperator(tt.response); got != tt.want {
				t.Fatalf("parseUSBATOperator(%q) = %q, want %q", tt.response, got, tt.want)
			}
		})
	}
}

func TestParseUSBATIMS(t *testing.T) {
	configured, available, ok := parseUSBATIMS("AT+QCFG=\"ims\"\r\n+QCFG: \"ims\",1,1\r\nOK")
	if !ok || configured != 1 || available != 1 {
		t.Fatalf("parseUSBATIMS() = (%d, %d, %v), want (1, 1, true)", configured, available, ok)
	}
	if _, _, ok := parseUSBATIMS("ERROR"); ok {
		t.Fatal("parseUSBATIMS(ERROR) unexpectedly succeeded")
	}
}

func TestShouldRestoreAutomaticOperator(t *testing.T) {
	tests := []struct {
		name          string
		previousICCID string
		currentICCID  string
		operatorReply string
		want          bool
	}{
		{
			name:          "SIM changed while manually selected",
			previousICCID: "old-card",
			currentICCID:  "new-card",
			operatorReply: `+COPS: 1,0,"CHN-CT",7`,
			want:          true,
		},
		{
			name:          "SIM changed while automatic",
			previousICCID: "old-card",
			currentICCID:  "new-card",
			operatorReply: `+COPS: 0,0,"CHN-CT",7`,
			want:          false,
		},
		{
			name:          "same SIM remains manual",
			previousICCID: "same-card",
			currentICCID:  "same-card",
			operatorReply: `+COPS: 1,0,"CHN-CT",7`,
			want:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRestoreAutomaticOperator(tt.previousICCID, tt.currentICCID, tt.operatorReply); got != tt.want {
				t.Fatalf("shouldRestoreAutomaticOperator() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnableIMS(t *testing.T) {
	instance := &app{demo: true}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/network/enable-ims", nil)

	instance.enableIMS(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("enableIMS status = %d, want %d; body=%s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
}

func TestInitUSBATESIMManagerAfterDelayedUSBOpen(t *testing.T) {
	instance := &app{}

	manager, switchAllowed := instance.currentESIMManager()
	if manager != nil || switchAllowed {
		t.Fatalf("initial eSIM state = (%v, %v), want unavailable", manager, switchAllowed)
	}

	instance.initUSBATESIMManager()
	manager, switchAllowed = instance.currentESIMManager()
	if manager == nil {
		t.Fatal("USB AT recovery did not initialize the eSIM manager")
	}
	if !switchAllowed {
		t.Fatal("USB AT eSIM manager should allow profile switching")
	}

	instance.initUSBATESIMManager()
	managerAgain, _ := instance.currentESIMManager()
	if managerAgain != manager {
		t.Fatal("repeated USB AT recovery replaced the existing eSIM manager")
	}
}
