//go:build windows

package installer

func statOwner(string) (int, int, bool) { return 0, 0, false }

func chownTree(string, int, int) error { return nil }
