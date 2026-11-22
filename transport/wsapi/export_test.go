package wsapi

// SetAfterRegisterHook installs a callback that runs after a new connection
// is registered with the broadcaster and before its snapshot is queued — the
// window in which a concurrent HandleEvent used to overtake the snapshot.
func SetAfterRegisterHook(s *Server, fn func()) {
	s.afterRegister = fn
}
