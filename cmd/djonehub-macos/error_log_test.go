package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func TestErrorLogReportsOnChange(t *testing.T) {
	buf := captureLog(t)

	var l errorLog
	notConnected := errors.New("DJI USB device is not connected")
	l.report("SMS poll failed", notConnected)
	l.report("SMS poll failed", notConnected)
	l.report("SMS poll failed", errors.New("USB AT is cooling down after disconnect: module reset"))
	l.report("SMS poll failed", nil)
	l.report("SMS poll failed", notConnected)
	if got := strings.Count(buf.String(), "\n"); got != 3 {
		t.Fatalf("logged %d lines:\n%s", got, buf.String())
	}
}
