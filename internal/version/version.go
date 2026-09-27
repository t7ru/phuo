package version

import "runtime/debug"

var stamp string

func String() string {
	if stamp != "" {
		return stamp
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return "seven"
	}
	return bi.Main.Version
}

func UserAgent() string {
	return "phuo/" + String() + " (+https://github.com/t7ru/phuo)"
}
