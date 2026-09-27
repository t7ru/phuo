package mw

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"encoding/json/jsontext"
	json "encoding/json/v2"
)

type Loaded struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Type    string `json:"type"`
}

var mwVersionRe = regexp.MustCompile(`define\(\s*'MW_VERSION',\s*'([^']+)'`)

func Version(root string) (string, error) {
	path := filepath.Join(root, "includes", "Defines.php")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := mwVersionRe.FindSubmatch(b)
	if m == nil {
		return "", fmt.Errorf("%s: MW_VERSION not found", path)
	}
	return string(m[1]), nil
}

func HasSchemaUpdates(hooks jsontext.Value) bool {
	if len(hooks) == 0 {
		return false
	}
	var p struct {
		LoadExtensionSchemaUpdates jsontext.Value `json:"LoadExtensionSchemaUpdates"`
		InstallerTasks             []any          `json:"InstallerTasks"`
		Hooks                      map[string]any `json:"Hooks"`
		HookHandlers               map[string]any `json:"HookHandlers"`
	}
	if err := json.Unmarshal(hooks, &p); err != nil {
		return false
	}
	if len(p.LoadExtensionSchemaUpdates) > 0 && string(p.LoadExtensionSchemaUpdates) != "null" {
		return true
	}
	if len(p.InstallerTasks) > 0 {
		return true
	}
	if _, ok := p.Hooks["LoadExtensionSchemaUpdates"]; ok {
		return true
	}
	for _, h := range p.HookHandlers {
		m, _ := h.(map[string]any)
		hs, _ := m["hooks"].(map[string]any)
		if _, ok := hs["LoadExtensionSchemaUpdates"]; ok {
			return true
		}
	}
	return false
}

type PHP struct {
	Bin, Root string
}

func (p PHP) bin() string {
	if p.Bin == "" {
		return "php"
	}
	return p.Bin
}

func (p PHP) Available() bool {
	if p.Root == "" {
		return false
	}
	_, err := exec.LookPath(p.bin())
	return err == nil
}

const registryEval = `echo json_encode(\MediaWiki\Registration\ExtensionRegistry::getInstance()->getAllThings());`

func (p PHP) Registry(ctx context.Context) ([]Loaded, error) {
	stdout, stderr, err := p.run(ctx, nil, strings.NewReader(registryEval), "maintenance/run.php", "eval")
	if err != nil {
		return nil, phpErr(err, stderr)
	}
	var out []Loaded
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p PHP) Config(ctx context.Context, settings ...string) (map[string]any, error) {
	stdout, stderr, err := p.run(ctx, nil, nil,
		"maintenance/run.php", "getConfiguration",
		"--format", "json",
		"--settings", strings.Join(settings, " "),
	)
	if err != nil {
		return nil, phpErr(err, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p PHP) ComposerLockUpToDate(ctx context.Context) (bool, string, error) {
	_, stderr, err := p.run(ctx, nil, nil, "maintenance/run.php", "checkComposerLockUpToDate")
	if err == nil {
		return true, "", nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, strings.TrimSpace(string(stderr)), nil
	}
	return false, "", phpErr(err, stderr)
}

func (p PHP) UpdateDB(ctx context.Context, out io.Writer) error {
	_, stderr, err := p.run(ctx, out, nil, "maintenance/run.php", "update", "--quick")
	if err != nil {
		return phpErr(err, stderr)
	}
	return nil
}

func Remote(ctx context.Context, c *http.Client, api string) (version string, exts, skins []Loaded, err error) {
	u, err := url.Parse(api)
	if err != nil {
		return "", nil, nil, err
	}
	q := u.Query()
	q.Set("action", "query")
	q.Set("meta", "siteinfo")
	q.Set("siprop", "general|extensions|skins")
	q.Set("format", "json")
	q.Set("formatversion", "2")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return "", nil, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", nil, nil, fmt.Errorf("%s: HTTP %d", api, res.StatusCode)
	}
	var body struct {
		Query struct {
			General struct {
				Generator string `json:"generator"`
			} `json:"general"`
			Extensions []Loaded `json:"extensions"`
			Skins      []Loaded `json:"skins"`
		} `json:"query"`
	}
	if err := json.UnmarshalRead(res.Body, &body); err != nil {
		return "", nil, nil, err
	}
	ver := strings.TrimSpace(strings.TrimPrefix(body.Query.General.Generator, "MediaWiki"))
	return ver, body.Query.Extensions, body.Query.Skins, nil
}

func (p PHP) run(ctx context.Context, combined io.Writer, stdin io.Reader, args ...string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, p.bin(), args...)
	cmd.Dir = p.Root
	cmd.Stdin = stdin
	var outBuf, errBuf bytes.Buffer
	if combined != nil {
		cmd.Stdout = io.MultiWriter(combined, &outBuf)
		cmd.Stderr = io.MultiWriter(combined, &errBuf)
	} else {
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
	}
	err = cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

func phpErr(err error, stderr []byte) error {
	line, _, _ := strings.Cut(string(stderr), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return err
	}
	return fmt.Errorf("%s", line)
}
