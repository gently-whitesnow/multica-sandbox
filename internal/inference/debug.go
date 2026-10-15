package inference

// Debug receives diagnostic lines when the controller runs with debug. Lines name
// sources, workspaces, gateways and refusal reasons, never keys.
type Debug func(format string, args ...any)

// SetDebug enables diagnostic lines; nil disables them.
func (s *Service) SetDebug(d Debug) { s.debug = d }

func (d Debug) printf(format string, args ...any) {
	if d != nil {
		d(format, args...)
	}
}
