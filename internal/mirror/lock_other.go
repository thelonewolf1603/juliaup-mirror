//go:build !unix

package mirror

// lock is a no-op where flock is unavailable; do not run two syncs at once.
func lock(string) (func(), error) { return func() {}, nil }
