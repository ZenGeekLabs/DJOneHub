package main

import (
	"strings"
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

func TestIsSupportedDJIUSBDevice(t *testing.T) {
	tests := []struct {
		name      string
		vendorID  int
		productID int
		want      bool
	}{
		{name: "DJI management mode", vendorID: 0x2ca3, productID: 0x4006, want: true},
		{name: "Quectel compatible mode", vendorID: 0x2c7c, productID: 0x0125, want: true},
		{name: "unrelated Quectel device", vendorID: 0x2c7c, productID: 0x9999, want: false},
		{name: "unrelated USB device", vendorID: 0x1234, productID: 0x5678, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSupportedDJIUSBDevice(tt.vendorID, tt.productID); got != tt.want {
				t.Fatalf("isSupportedDJIUSBDevice(%04x:%04x) = %v, want %v", tt.vendorID, tt.productID, got, tt.want)
			}
		})
	}
}

func TestParseDJIUSBDevicesKeepsIdenticalModulesSeparate(t *testing.T) {
	block := func(location, iface string) string {
		return strings.NewReplacer("LOCATION", location, "IFACE", iface).Replace(`
    "idVendor" = 11388
    "idProduct" = 293
    "locationID" = LOCATION
    "USB Product Name" = "Baiwang"
    "USB Vendor Name" = "BAIWANG"
    "USBSpeed" = 3
    "bInterfaceNumber" = IFACE
    "bInterfaceClass" = 255
    "bInterfaceSubClass" = 0
    "bInterfaceProtocol" = 0
    "bNumEndpoints" = 3`)
	}
	raw := block("34668544", "0") + "\n\n" + block("34668544", "1") + "\n\n" + block("34734080", "0")

	devices := parseDJIUSBDevices(raw)
	if len(devices) != 2 {
		t.Fatalf("parseDJIUSBDevices returned %d devices, want 2", len(devices))
	}
	if devices[0].LocationID != "0x2110000" || devices[1].LocationID != "0x2120000" {
		t.Fatalf("unexpected locations: %q, %q", devices[0].LocationID, devices[1].LocationID)
	}
	if len(devices[0].Interfaces) != 2 || len(devices[1].Interfaces) != 1 {
		t.Fatalf("interfaces were not grouped per physical module: %d, %d", len(devices[0].Interfaces), len(devices[1].Interfaces))
	}
}

func TestParseUSBLocationID(t *testing.T) {
	for _, value := range []string{"0x2110000", "2110000", "0X2110000"} {
		got, err := parseUSBLocationID(value)
		if err != nil || got != 0x02110000 {
			t.Fatalf("parseUSBLocationID(%q) = %#x, %v", value, got, err)
		}
	}
	if _, err := parseUSBLocationID("not-a-location"); err == nil {
		t.Fatal("parseUSBLocationID accepted invalid input")
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
