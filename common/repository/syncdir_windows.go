package repository

// syncDir is a no-op on Windows, where directory handles cannot be flushed;
// NTFS journals the rename itself.
func syncDir(string) error { return nil }
