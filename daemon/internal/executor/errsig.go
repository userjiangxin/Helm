package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	reHex    = regexp.MustCompile(`0x[0-9a-fA-F]+|\b[0-9a-f]{8,}\b`)
	rePath   = regexp.MustCompile(`(?:/[\w.\-@+]+)+/?`)
	reNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)
	reQuoted = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	reSpace  = regexp.MustCompile(`\s+`)
)

// ErrorSignature normalizes an error message so repeats of "the same" failure share one id:
// addresses, paths, numbers and quoted values are masked before hashing. Only the last
// non-empty line is used — tracebacks end with the exception line.
func ErrorSignature(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}
	s := reHex.ReplaceAllString(last, "<hex>")
	s = rePath.ReplaceAllString(s, "<path>")
	s = reQuoted.ReplaceAllString(s, "<q>")
	s = reNumber.ReplaceAllString(s, "<n>")
	s = strings.ToLower(reSpace.ReplaceAllString(strings.TrimSpace(s), " "))
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}
