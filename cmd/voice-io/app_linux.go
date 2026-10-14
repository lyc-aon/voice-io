package main

// launchedAsApp is false: desktop launchers run `voice-io start`.
func launchedAsApp([]string) bool { return false }

// logToFile is a no-op: `voice-io start` already sends a background run's
// output to the log file, and systemd sends it to the journal.
func logToFile() {}
