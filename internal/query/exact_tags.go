package query

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// The limits match the existing client-side exact tag intersection. This is
// a bounded query field, never a SQL expression or an authorization policy.
func cleanExactTags(tags []string) ([]string, error) {
	if len(tags) > 64 {
		return nil, errcode.New(errcode.SchemaInvalid, "at most 64 exact tags are accepted")
	}
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		if !utf8.ValidString(raw) {
			return nil, errcode.New(errcode.SchemaInvalid, "exact tag must be valid Unicode")
		}
		tag := norm.NFC.String(strings.TrimSpace(raw))
		if tag == "" || utf8.RuneCountInString(tag) > 64 || strings.ContainsFunc(tag, unicode.IsControl) {
			return nil, errcode.New(errcode.SchemaInvalid, "exact tag is empty, too long or contains a control character")
		}
		out = append(out, tag)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func hasExactTags(have, wanted []string) bool {
	for _, tag := range wanted {
		if !slices.Contains(have, tag) {
			return false
		}
	}
	return true
}
