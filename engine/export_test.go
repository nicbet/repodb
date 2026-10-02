package engine

// SetMaxValidatedNodeLinks sets the validated-node cache bound and returns a
// function restoring it.
func SetMaxValidatedNodeLinks(limit int) (restore func()) {
	processValidation.nodesMu.Lock()
	previous := maxValidatedNodeLinks
	maxValidatedNodeLinks = limit
	processValidation.nodesMu.Unlock()
	return func() {
		processValidation.nodesMu.Lock()
		maxValidatedNodeLinks = previous
		processValidation.nodesMu.Unlock()
	}
}
