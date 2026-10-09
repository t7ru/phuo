package gitproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

var ErrAuth = errors.New("authentication required")

func LsRefs(ctx context.Context, c *http.Client, repo string, prefixes ...string) (map[string]string, error) {
	var body bytes.Buffer
	pkt(&body, "command=ls-refs")
	pkt(&body, "agent=phuo")
	body.WriteString("0001")
	pkt(&body, "peel")
	pkt(&body, "symrefs")
	for _, p := range prefixes {
		pkt(&body, "ref-prefix "+p)
	}
	body.WriteString("0000")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(repo, "/")+"/git-upload-pack", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("%s: %w (ls-refs HTTP %d)", repo, ErrAuth, res.StatusCode)
		}
		return nil, fmt.Errorf("%s: ls-refs HTTP %d", repo, res.StatusCode)
	}

	refs := map[string]string{}
	rd := bufio.NewReader(res.Body)
	var hdr [4]byte
	var line []byte
	for {
		if _, err := io.ReadFull(rd, hdr[:]); err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(string(hdr[:]), 16, 16)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return refs, nil
		}
		if n <= 4 {
			continue
		}
		need := int(n - 4)
		if cap(line) < need {
			line = make([]byte, need)
		} else {
			line = line[:need]
		}
		if _, err := io.ReadFull(rd, line); err != nil {
			return nil, err
		}
		sha, rest, _ := strings.Cut(strings.TrimSuffix(string(line), "\n"), " ")
		ref, attrs, _ := strings.Cut(rest, " ")
		var symTarget string
		for attr := range strings.FieldsSeq(attrs) {
			if p, ok := strings.CutPrefix(attr, "peeled:"); ok {
				sha = p
			}
			if t, ok := strings.CutPrefix(attr, "symref-target:"); ok {
				symTarget = t
			}
		}
		refs[ref] = sha
		if symTarget != "" {
			refs[symTarget] = sha
		}
	}
}

func pkt(w *bytes.Buffer, s string) {
	n := len(s) + 5
	var hdr [4]byte
	hdr[0] = youHexedHerDidntYou[n>>12]
	hdr[1] = youHexedHerDidntYou[n>>8&0xf]
	hdr[2] = youHexedHerDidntYou[n>>4&0xf]
	hdr[3] = youHexedHerDidntYou[n&0xf]
	w.Write(hdr[:])
	w.WriteString(s)
	w.WriteByte('\n')
}

const youHexedHerDidntYou = "0123456789abcdef"
