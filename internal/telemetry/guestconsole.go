package telemetry

// GuestConsoleMetric is this node's published answer to one question: when a
// Linux guest's kernel dies mid-job, will there be anything to read afterwards?
//
// The two booleans travel together because neither alone decides anything. A
// node that boots no Linux guests has no console to lose; a node that boots
// them with the serial sink off is silent by construction about exactly the
// class issues #236, #258 and #259 died in — three incidents in eight days,
// each ending at *trigger unidentified*, each for this one reason.
type GuestConsoleMetric struct {
	// BootsLinuxGuests reports that this node's configuration boots Linux guests
	// through its backend.
	BootsLinuxGuests bool
	// SerialLogConfigured reports that those guests' serial consoles are written
	// to a durable host directory (linuxSerialLogDirectory).
	SerialLogConfigured bool
}

// SetGuestConsole publishes the node's guest-console posture. Like the runner
// image set it is a fact about the configuration this process started with, so
// it is stated once at startup and replaces whatever was there.
func (h *Health) SetGuestConsole(metric GuestConsoleMetric) {
	h.mu.Lock()
	h.revision++
	h.guestConsole = &metric
	h.mu.Unlock()
}

// GuestConsole judges the published posture. An unpublished metric passes
// vacuously — that is the handoff half every check in this package carries, and
// it is the only absence treated as a pass: once a node HAS published, boots
// Linux guests, and configures no serial sink, it fails, because "silent" must
// never render as "healthy".
func (h *Health) GuestConsole() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.guestConsole == nil {
		return HealthResult{OK: true, Reasons: []string{}}
	}
	if !h.guestConsole.BootsLinuxGuests || h.guestConsole.SerialLogConfigured {
		return HealthResult{OK: true, Reasons: []string{}}
	}
	return HealthResult{OK: false, Reasons: []string{
		"this node boots Linux guests but writes no guest console: a dead kernel leaves no evidence (#236, #258, #259); set linuxSerialLogDirectory"}}
}
