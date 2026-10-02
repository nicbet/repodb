package engine

// SetMaxValidatedNodeLinks sets the validated-node cache bound and returns a
// function restoring it.
func SetMaxValidatedNodeLinks(limit int) (restore func()) {
	validatedNodes.Lock()
	previous := maxValidatedNodeLinks
	maxValidatedNodeLinks = limit
	validatedNodes.Unlock()
	return func() {
		validatedNodes.Lock()
		maxValidatedNodeLinks = previous
		validatedNodes.Unlock()
	}
}
