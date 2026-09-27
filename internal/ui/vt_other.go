//go:build !windows

package ui

import "os"

func enableVT(f *os.File) {}
