//go:build !darwin

package main

// logToFile is a no-op: `voice-io start` already sends a background run's
// output to the log file, and systemd sends it to the journal.
func logToFile() {}
