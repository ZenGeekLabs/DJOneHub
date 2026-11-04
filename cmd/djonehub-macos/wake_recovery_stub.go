//go:build !darwin

package main

import "context"

// startWakeMonitor is a no-op: wake recovery reads macOS sleep state.
func startWakeMonitor(context.Context, *app) {}
