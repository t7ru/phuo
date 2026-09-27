package patch

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func Apply(ctx context.Context, patchFile, dir string, check bool) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is required")
	}
	if check {
		if err := runApply(ctx, patchFile, dir, true); err != nil {
			return err
		}
	}
	return runApply(ctx, patchFile, dir, false)
}

func Check(ctx context.Context, patchFile, dir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is required")
	}
	return runApply(ctx, patchFile, dir, true)
}

func runApply(ctx context.Context, patchFile, dir string, check bool) error {
	args := []string{"apply"}
	if check {
		args = append(args, "--check")
	}
	args = append(args, "--", patchFile)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func RewriteDiffPrefixes(diff, aPrefix, bPrefix string) string {
	aPrefix = strings.TrimRight(strings.ReplaceAll(aPrefix, "\\", "/"), "/")
	bPrefix = strings.TrimRight(strings.ReplaceAll(bPrefix, "\\", "/"), "/")
	lines := strings.Split(diff, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "--- a/"):
			lines[i] = "--- a/" + trimPrefixPath(line[len("--- a/"):], aPrefix)
		case strings.HasPrefix(line, "+++ b/"):
			lines[i] = "+++ b/" + trimPrefixPath(line[len("+++ b/"):], bPrefix)
		case strings.HasPrefix(line, "diff --git "):
			lines[i] = rewriteDiffGit(line, aPrefix, bPrefix)
		}
	}
	return strings.Join(lines, "\n")
}

func trimPrefixPath(path, prefix string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if rest, ok := strings.CutPrefix(path, prefix+"/"); ok {
		return rest
	}
	if path == prefix {
		return ""
	}
	if i := strings.Index(path, "/"+prefix+"/"); i >= 0 {
		return path[i+len(prefix)+2:]
	}
	return path
}

func rewriteDiffGit(line, aPrefix, bPrefix string) string {
	rest, ok := strings.CutPrefix(line, "diff --git ")
	if !ok {
		return line
	}
	aPart, bPart, ok := strings.Cut(rest, " b/")
	if !ok {
		return line
	}
	aPath, ok := strings.CutPrefix(aPart, "a/")
	if !ok {
		return line
	}
	return "diff --git a/" + trimPrefixPath(aPath, aPrefix) + " b/" + trimPrefixPath(bPart, bPrefix)
}
