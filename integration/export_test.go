package integration

// SetBeforePublish installs fn as sync's pre-publication hook and returns a
// function restoring the previous one.
func SetBeforePublish(fn func()) (restore func()) {
	previous := beforePublish
	beforePublish = fn
	return func() { beforePublish = previous }
}
