// Package notify provides event notification for mount-manager, delivered
// via a pluggable Notifier interface (currently a JSON webhook or a no-op).
package notify

import "time"

// Event kind constants.
const (
	EventStartup           = "startup"
	EventWaiting           = "waiting"
	EventStackUp           = "stack_up"
	EventStackStopped      = "stack_stopped"
	EventFailureDetected   = "failure_detected"
	EventRecoveryStarted   = "recovery_started"
	EventRecoverySucceeded = "recovery_succeeded"
	EventRecoveryFailed    = "recovery_failed"
	EventStaleMountCleaned = "stale_mount_cleaned"
	EventShutdown          = "shutdown"
)

// Event describes a single notification.
type Event struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	Host      string    `json:"host"`
	MountPath string    `json:"mount_path"`
	Detail    string    `json:"detail"`
	Attempt   int       `json:"attempt"`
}

// Notifier delivers events.
type Notifier interface {
	Notify(Event)
}

// Nop is a Notifier that discards all events.
type Nop struct{}

// Notify implements Notifier.
func (Nop) Notify(Event) {}
