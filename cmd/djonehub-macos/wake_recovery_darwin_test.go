//go:build darwin

package main

import (
	"testing"
	"time"
)

func TestReadSystemWakeTime(t *testing.T) {
	wake, err := readSystemWakeTime()
	if err != nil {
		t.Fatal(err)
	}
	if !wake.IsZero() && (wake.Before(time.Unix(1_000_000_000, 0)) || wake.After(time.Now().Add(time.Minute))) {
		t.Fatalf("implausible kern.waketime %s", wake)
	}
}
