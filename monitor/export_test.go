package monitor

// TrackedSizes reports how many cursor and removed-ID entries m holds.
func TrackedSizes(m *Monitor) (cursors, removed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.cursors), len(m.removed)
}
