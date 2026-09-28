package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

var stamp string

func String() string {
	if stamp != "" {
		return stamp
	}
	if v := moduleVersion(); v != "" {
		return v
	}
	return "seven"
}

func Module() string {
	if stamp != "" {
		return ""
	}
	return moduleVersion()
}

func Devel() bool {
	if stamp != "" {
		return false
	}
	if v := moduleVersion(); v == "" || strings.Contains(v, "+dirty") {
		return true
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return true
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.modified" && s.Value == "true" {
			return true
		}
	}
	return false
}

func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return ""
	}
	return bi.Main.Version
}

func Compare(a, b string) int {
	an, apre, aok := splitVer(a)
	bn, bpre, bok := splitVer(b)
	switch {
	case !aok && !bok:
		return strings.Compare(a, b)
	case !aok:
		return -1
	case !bok:
		return 1
	}
	n := max(len(an), len(bn))
	for i := range n {
		av, bv := 0, 0
		if i < len(an) {
			av = an[i]
		}
		if i < len(bn) {
			bv = bn[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

func splitVer(v string) (nums []int, pre string, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	base, pre, _ := strings.Cut(v, "-")
	parts := strings.Split(base, ".")
	nums = make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, "", false
		}
		nums[i] = n
	}
	return nums, pre, true
}

func UserAgent() string {
	return "phuo/" + String() + " (+https://github.com/t7ru/phuo)"
}
