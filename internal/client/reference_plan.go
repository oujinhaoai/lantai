package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"golang.org/x/text/unicode/norm"
)

const (
	ReferenceInputSchema    = "lantai.reference-input/v1"
	ReferencePlanSchema     = "lantai.reference-plan/v1"
	MaxReferenceNotes       = 1000
	MaxReferenceNoteBytes   = 1 << 20
	MaxReferenceSourceBytes = 2 << 20
	MaxReferenceEdits       = 10000
)

type ReferenceMapping struct {
	OldTarget string `json:"old_target"`
	FixedRef  string `json:"fixed_ref"`
}
type ReferencePreserve struct {
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}
type ReferenceInput struct {
	Schema   string              `json:"schema"`
	Notes    []string            `json:"notes"`
	Mappings []ReferenceMapping  `json:"mappings"`
	Preserve []ReferencePreserve `json:"preserve,omitempty"`
}
type ReferenceEdit struct {
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Target   string `json:"target"`
	FixedRef string `json:"fixed_ref"`
	Syntax   string `json:"syntax"`
}
type ReferenceIssue struct {
	Note   string `json:"note"`
	Offset int    `json:"offset"`
	Target string `json:"target,omitempty"`
	Reason string `json:"reason"`
}
type ReferenceNotePlan struct {
	Path         string          `json:"path"`
	BeforeSHA256 string          `json:"before_sha256"`
	AfterSHA256  string          `json:"after_sha256"`
	Before       string          `json:"before"`
	After        string          `json:"after"`
	Edits        []ReferenceEdit `json:"edits"`
	Diff         string          `json:"diff"`
}
type ReferencePlan struct {
	Schema     string              `json:"schema"`
	Digest     string              `json:"digest"`
	SourceRoot string              `json:"source_root"`
	Input      ReferenceInput      `json:"input"`
	Complete   bool                `json:"complete"`
	Scope      string              `json:"scope"`
	Notes      []ReferenceNotePlan `json:"notes"`
	Unresolved []ReferenceIssue    `json:"unresolved"`
	Skipped    []ReferenceIssue    `json:"skipped"`
}

func (v *ReferenceInput) UnmarshalJSON(b []byte) error {
	if _, err := canonjson.Canonicalize(b); err != nil {
		return err
	}
	type plain ReferenceInput
	var x plain
	if e := importStrictJSON(b, &x); e != nil {
		return e
	}
	*v = ReferenceInput(x)
	return nil
}
func (v *ReferencePlan) UnmarshalJSON(b []byte) error {
	if _, err := canonjson.Canonicalize(b); err != nil {
		return err
	}
	type plain ReferencePlan
	var x plain
	if e := importStrictJSON(b, &x); e != nil {
		return e
	}
	*v = ReferencePlan(x)
	return nil
}

// PrepareReferencePlan is offline and read-only. Complete covers only explicit
// mappings in selected notes; it never means every vault reference was scanned.
func PrepareReferencePlan(ctx context.Context, input ReferenceInput, sourceRoot string) (ReferencePlan, error) {
	p := ReferencePlan{Schema: ReferencePlanSchema, Input: input, Scope: "explicit_mappings_in_selected_notes_only", Notes: []ReferenceNotePlan{}, Unresolved: []ReferenceIssue{}, Skipped: []ReferenceIssue{}}
	raw, err := json.Marshal(input)
	if err != nil || int64(len(raw)) > MaxJSONBytes {
		return p, errors.New("client: reference input exceeds local document limit")
	}
	if err = json.Unmarshal(raw, &p.Input); err != nil {
		return p, err
	}
	mappings, preserve, err := referenceSelection(p.Input)
	if err != nil {
		return p, err
	}
	rootName, err := importAbsolute(sourceRoot)
	if err != nil {
		return p, err
	}
	p.SourceRoot = rootName
	root, err := openWorksetRoot(rootName, false)
	if err != nil {
		return p, err
	}
	if root == nil {
		return p, os.ErrNotExist
	}
	defer root.Close()
	matched := map[string]int{}
	total := 0
	for _, name := range p.Input.Notes {
		if err = ctx.Err(); err != nil {
			return p, err
		}
		if !strings.HasSuffix(name, ".md") {
			return p, errors.New("client: reference source selection must contain only .md notes")
		}
		before, err := readReferenceNote(ctx, rootName, root, name)
		if err != nil {
			return p, err
		}
		total += len(before)
		if total > MaxReferenceSourceBytes {
			return p, errors.New("client: reference selected source bytes exceed 2 MiB; split the batch")
		}
		note, unresolved, skipped, counts := planReferenceNote(name, before, mappings, preserve)
		if err = ctx.Err(); err != nil {
			return p, err
		}
		p.Notes = append(p.Notes, note)
		p.Unresolved = append(p.Unresolved, unresolved...)
		p.Skipped = append(p.Skipped, skipped...)
		for target, n := range counts {
			matched[target] += n
		}
	}
	return finishReferencePlan(p, matched)
}
func referenceSelection(input ReferenceInput) (map[string]string, map[string]ReferencePreserve, error) {
	var err error
	if input.Schema != ReferenceInputSchema || len(input.Notes) == 0 || len(input.Notes) > MaxReferenceNotes || len(input.Mappings) == 0 || len(input.Mappings) > 10000 {
		return nil, nil, errors.New("client: invalid reference schema, selected notes or mappings count")
	}
	if err = ValidatePaths(input.Notes); err != nil {
		return nil, nil, err
	}
	mappings := map[string]string{}
	preserve := map[string]ReferencePreserve{}
	for _, m := range input.Mappings {
		if !referenceTargetValid(m.OldTarget) || referenceProtectedTarget(m.OldTarget) {
			return nil, nil, errors.New("client: invalid or protected reference mapping target")
		}
		if _, err = ids.ParseURI(m.FixedRef); err != nil {
			return nil, nil, err
		}
		if _, exists := mappings[m.OldTarget]; exists {
			return nil, nil, errors.New("client: duplicate or ambiguous old reference target")
		}
		mappings[m.OldTarget] = m.FixedRef
	}
	for _, x := range input.Preserve {
		if !referenceTargetValid(x.Target) || strings.TrimSpace(x.Reason) == "" || len(x.Reason) > 2000 {
			return nil, nil, errors.New("client: preserved references require exact target and bounded reason")
		}
		switch x.Kind {
		case "note", "model", "tool", "intermediate", "external", "other":
		default:
			return nil, nil, errors.New("client: unknown preserved reference kind")
		}
		if _, ok := mappings[x.Target]; ok {
			return nil, nil, errors.New("client: a reference cannot be both mapped and protected")
		}
		if _, ok := preserve[x.Target]; ok {
			return nil, nil, errors.New("client: duplicate preserved reference")
		}
		preserve[x.Target] = x
	}
	if worksetConflicts(input.Notes) {
		return nil, nil, errors.New("client: reference note paths collide after Unicode/case folding")
	}
	for _, name := range input.Notes {
		if !strings.HasSuffix(name, ".md") {
			return nil, nil, errors.New("client: reference selection must contain only .md notes")
		}
	}
	return mappings, preserve, nil
}
func ValidateReferencePlan(ctx context.Context, p ReferencePlan, sourceRoot string) error {
	if err := validateReferenceSnapshot(p); err != nil {
		return err
	}
	fresh, err := PrepareReferencePlan(ctx, p.Input, sourceRoot)
	if err != nil {
		return err
	}
	if fresh.Digest != p.Digest {
		return errors.New("client: reference source or plan changed; review a new plan")
	}
	return nil
}
func validateReferenceSnapshot(p ReferencePlan) error {
	if p.Schema != ReferencePlanSchema || p.Scope != "explicit_mappings_in_selected_notes_only" {
		return errors.New("client: invalid reference plan schema or scope")
	}
	d, err := referencePlanDigest(p)
	if err != nil || d != p.Digest {
		return errors.New("client: reference plan digest mismatch")
	}
	if !p.Complete || len(p.Unresolved) > 0 || len(p.Notes) != len(p.Input.Notes) {
		return errors.New("client: unresolved reference plan cannot be copied")
	}
	mappings, preserve, err := referenceSelection(p.Input)
	if err != nil {
		return err
	}
	rebuilt := ReferencePlan{Schema: ReferencePlanSchema, SourceRoot: p.SourceRoot, Input: p.Input, Scope: p.Scope, Notes: []ReferenceNotePlan{}, Unresolved: []ReferenceIssue{}, Skipped: []ReferenceIssue{}}
	matched := map[string]int{}
	total := 0
	for i, n := range p.Notes {
		if n.Path != p.Input.Notes[i] || len(n.Before) > MaxReferenceNoteBytes || !utf8.ValidString(n.Before) || strings.ContainsRune(n.Before, 0) {
			return errors.New("client: invalid reference snapshot note")
		}
		total += len(n.Before)
		if total > MaxReferenceSourceBytes {
			return errors.New("client: reference snapshot exceeds source byte limit")
		}
		note, unresolved, skipped, counts := planReferenceNote(n.Path, []byte(n.Before), mappings, preserve)
		rebuilt.Notes = append(rebuilt.Notes, note)
		rebuilt.Unresolved = append(rebuilt.Unresolved, unresolved...)
		rebuilt.Skipped = append(rebuilt.Skipped, skipped...)
		for target, count := range counts {
			matched[target] += count
		}
	}
	rebuilt, err = finishReferencePlan(rebuilt, matched)
	if err != nil {
		return err
	}
	if rebuilt.Digest != p.Digest {
		return errors.New("client: reference plan does not match pure regeneration from input and before snapshots")
	}
	return nil
}
func finishReferencePlan(p ReferencePlan, matched map[string]int) (ReferencePlan, error) {
	edits := 0
	for _, n := range p.Notes {
		if len(n.After) > MaxReferenceNoteBytes {
			return p, errors.New("client: transformed reference note exceeds 1 MiB; narrow the batch")
		}
		edits += len(n.Edits)
	}
	if edits > MaxReferenceEdits {
		return p, errors.New("client: reference edit count exceeds 10000; split the batch")
	}
	for _, m := range p.Input.Mappings {
		if matched[m.OldTarget] == 0 {
			p.Unresolved = append(p.Unresolved, ReferenceIssue{Offset: -1, Target: m.OldTarget, Reason: "mapping_has_no_supported_occurrence"})
		}
	}
	less := func(a, b ReferenceIssue) bool {
		if a.Note != b.Note {
			return a.Note < b.Note
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Reason < b.Reason
	}
	sort.Slice(p.Unresolved, func(i, j int) bool { return less(p.Unresolved[i], p.Unresolved[j]) })
	sort.Slice(p.Skipped, func(i, j int) bool { return less(p.Skipped[i], p.Skipped[j]) })
	p.Complete = len(p.Unresolved) == 0
	var err error
	p.Digest, err = referencePlanDigest(p)
	return p, err
}
func referencePlanDigest(p ReferencePlan) (string, error) {
	p.Digest = ""
	b, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	if int64(len(b)) > MaxJSONBytes {
		return "", errors.New("client: reference plan exceeds 8 MiB; split the batch")
	}
	return importSHA(b), nil
}
func referenceTargetValid(s string) bool {
	if s == "" || len(s) > 8192 || !utf8.ValidString(s) || !norm.NFC.IsNormalString(s) || strings.TrimSpace(s) != s || strings.Contains(s, "\\") {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func referenceProtectedTarget(s string) bool {
	lower := strings.ToLower(s)
	for _, suffix := range []string{".safetensors", ".ckpt", ".pt", ".pth", ".onnx", ".gguf"} {
		if strings.HasSuffix(strings.SplitN(strings.SplitN(lower, "#", 2)[0], "?", 2)[0], suffix) {
			return true
		}
	}
	for _, part := range strings.Split(lower, "/") {
		switch part {
		case "models", "checkpoints", "loras", "controlnet", "tools", "tool", "cache", "intermediate", "tmp", "temp", "venv", ".venv", "node_modules", "site-packages":
			return true
		}
	}
	return false
}
func readReferenceNote(ctx context.Context, rootName string, root *os.Root, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := SafeRelative(name); err != nil {
		return nil, err
	}
	if err := checkWorksetRoot(rootName, root); err != nil {
		return nil, err
	}
	if err := checkWorksetTarget(root, name, false); err != nil {
		return nil, err
	}
	st, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > MaxReferenceNoteBytes {
		return nil, errors.New("client: reference note must be a regular file no larger than 1 MiB")
	}
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, errors.New("client: reference note changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, MaxReferenceNoteBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, e := root.Lstat(filepath.FromSlash(name))
	if e != nil || !os.SameFile(st, current) || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) || int64(len(b)) != st.Size() {
		return nil, errors.New("client: reference note changed while reading")
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, errors.New("client: reference notes must be UTF-8 Markdown without NUL")
	}
	return b, nil
}

func planReferenceNote(name string, b []byte, mappings map[string]string, preserve map[string]ReferencePreserve) (ReferenceNotePlan, []ReferenceIssue, []ReferenceIssue, map[string]int) {
	n := ReferenceNotePlan{Path: name, Before: string(b), BeforeSHA256: importSHA(b), Edits: []ReferenceEdit{}}
	unresolved := []ReferenceIssue{}
	skipped := []ReferenceIssue{}
	matched := map[string]int{}
	mask, regions := referenceMask(b)
	targets := make([]string, 0, len(mappings))
	for target := range mappings {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, r := range regions {
		skipped = append(skipped, ReferenceIssue{Note: name, Offset: r.start, Reason: r.reason})
	}
	visit := func(start, end, targetStart, targetEnd int, target, syntax string, replacement string, supported bool) {
		fixed, selected := mappings[target]
		if !selected {
			reason := "not_selected"
			if x, ok := preserve[target]; ok {
				reason = "preserved_" + x.Kind
			}
			skipped = append(skipped, ReferenceIssue{Note: name, Offset: start, Target: target, Reason: reason})
			return
		}
		if !supported {
			unresolved = append(unresolved, ReferenceIssue{Note: name, Offset: start, Target: target, Reason: "unsupported_or_ambiguous_" + syntax})
			return
		}
		if replacement == "" {
			replacement = fixed
			start = targetStart
			end = targetEnd
		}
		n.Edits = append(n.Edits, ReferenceEdit{Start: start, End: end, Before: string(b[start:end]), After: replacement, Target: target, FixedRef: fixed, Syntax: syntax})
		matched[target]++
	}
	lineEnd := -1
	work := 0
	budget := 8*len(b) + 100000
	unsupportedLine := func(start, end int, reason string) {
		found := false
		for _, target := range targets {
			work += end - start + len(target)
			if work > budget {
				unresolved = append(unresolved, ReferenceIssue{Note: name, Offset: start, Reason: "scan_budget_exceeded"})
				return
			}
			if bytes.Contains(b[start:end], []byte(target)) {
				found = true
				unresolved = append(unresolved, ReferenceIssue{Note: name, Offset: start, Target: target, Reason: reason})
			}
		}
		if !found {
			skipped = append(skipped, ReferenceIssue{Note: name, Offset: start, Reason: "unsupported_not_selected"})
		}
	}
scan:
	for i := 0; i < len(b); {
		if work > budget {
			break
		}
		if i >= lineEnd {
			if b[i] == '\n' {
				i++
				continue
			}
			e := bytes.IndexByte(b[i:], '\n')
			lineEnd = len(b)
			if e >= 0 {
				lineEnd = i + e
			}
		}
		if len(n.Edits) >= MaxReferenceEdits {
			unresolved = append(unresolved, ReferenceIssue{Note: name, Offset: i, Reason: "edit_limit_exceeded"})
			break
		}
		if mask[i] {
			i++
			continue
		}
		if (b[i] == '[' || b[i] == '!' || b[i] == '<') && referenceEscaped(b, i) {
			i++
			continue
		}
		start := i
		image := b[i] == '!' && i+1 < len(b) && b[i+1] == '['
		j := i
		if image {
			j++
		}
		if j+1 < len(b) && b[j] == '[' && b[j+1] == '[' {
			end := bytes.Index(b[j+2:lineEnd], []byte("]]"))
			if end < 0 {
				spanEnd := referenceUnsupportedLabelSpan(b, j, mask)
				unsupportedLine(start, spanEnd, "unsupported_unclosed_wikilink")
				i = spanEnd
				continue
			}
			end += j + 2
			if bytes.IndexByte(b[j+2:end], '\n') >= 0 || referenceMasked(mask, start, end+2) {
				i = end + 2
				continue
			}
			raw := string(b[j+2 : end])
			if strings.ContainsAny(raw, "[]") {
				unsupportedLine(j+2, end, "unsupported_nested_wikilink")
			}
			parts := strings.Split(raw, "|")
			target := parts[0]
			alias := target
			if len(parts) == 2 {
				alias = parts[1]
			}
			supported := len(parts) <= 2 && target != "" && !strings.ContainsAny(raw, "\\[]\r\n") && !strings.ContainsAny(alias, "[]")
			replacement := ""
			if fixed, ok := mappings[target]; ok {
				label := alias
				replacement = "[" + label + "](" + fixed + ")"
				if image {
					replacement = "!" + replacement
				}
			}
			visit(start, end+2, j+2, j+2+len(target), target, "wikilink", replacement, supported)
			i = end + 2
			continue
		}
		if j < len(b) && b[j] == '[' {
			close := referenceLabelEnd(b, j, mask)
			if close < 0 {
				spanEnd := referenceUnsupportedLabelSpan(b, j, mask)
				unsupportedLine(start, spanEnd, "unsupported_unclosed_markdown_label")
				i = spanEnd
				continue
			}
			// Advancing past an outer label also skips any inner image/link.
			// Until nested labels are parsed explicitly, a mapped target inside
			// one must remain unresolved even if another occurrence was edited.
			// Keep the existing bounded comparison budget; unrelated labels do
			// not expand this batch's selected mapping scope.
			if bytes.ContainsAny(b[j+1:close], "[]") {
				unsupportedLine(j+1, close, "unsupported_nested_markdown_label")
			}
			if close > j && close+1 < len(b) && b[close+1] == '(' {
				ts, te, end, ok := referenceMarkdownDestination(b, close+2)
				if te > ts && !referenceMasked(mask, start, end) {
					visit(start, end, ts, te, string(b[ts:te]), "markdown", "", ok)
				}
				if end > i {
					i = end
					continue
				}
			}
			// Reference definitions are intentionally unsupported, rather than
			// changing a definition shared by links outside this selected batch.
			if close > j && close+1 < len(b) && b[close+1] == ':' {
				end := bytes.IndexByte(b[close+2:], '\n')
				if end < 0 {
					end = len(b)
				} else {
					end += close + 2
				}
				target := strings.TrimSpace(string(b[close+2 : end]))
				if target == "" && end < len(b) {
					// A reference definition can place its destination on the
					// next line. It remains unsupported, but must not be hidden
					// merely because the first line after ':' was empty.
					next := bytes.IndexByte(b[end+1:], '\n')
					spanEnd := len(b)
					if next >= 0 {
						spanEnd = end + 1 + next
					}
					unsupportedLine(start, spanEnd, "unsupported_multiline_reference_definition")
					i = spanEnd
					continue
				}
				if strings.HasPrefix(target, "<") {
					if e := strings.IndexByte(target, '>'); e >= 0 {
						target = target[1:e]
					}
				} else if e := strings.IndexAny(target, " \t"); e >= 0 {
					target = target[:e]
				}
				visit(start, end, 0, 0, target, "reference_definition", "", false)
				i = end
				continue
			}
			if close > j {
				i = close + 1
				continue
			}
		}
		if b[i] == '<' {
			if end := referenceUnsupportedHTMLEnd(b, i); end > 0 {
				token := string(b[i+1 : end-1])
				if _, ok := mappings[token]; ok {
					visit(i, end, 0, 0, token, "autolink", "", false)
				} else {
					for _, target := range targets {
						work += len(token) + len(target)
						if work > budget {
							unresolved = append(unresolved, ReferenceIssue{Note: name, Offset: i, Reason: "scan_budget_exceeded"})
							break scan
						}
						if strings.Contains(token, target) {
							visit(i, end, 0, 0, target, "html", "", false)
						}
					}
				}
				i = end
				continue
			}
			unsupportedLine(i, len(b), "unsupported_unclosed_html_or_autolink")
			i = len(b)
			continue
		}
		i++
	}
	sort.Slice(n.Edits, func(i, j int) bool { return n.Edits[i].Start < n.Edits[j].Start })
	var after strings.Builder
	cursor := 0
	var diff strings.Builder
	fmt.Fprintf(&diff, "--- %s (before)\n+++ %s (copy)\n", name, name)
	for _, e := range n.Edits {
		after.Write(b[cursor:e.Start])
		after.WriteString(e.After)
		cursor = e.End
		fmt.Fprintf(&diff, "@@ bytes %d:%d @@\n-%s\n+%s\n", e.Start, e.End, e.Before, e.After)
	}
	after.Write(b[cursor:])
	n.After = after.String()
	n.AfterSHA256 = importSHA([]byte(n.After))
	n.Diff = diff.String()
	return n, unresolved, skipped, matched
}

type referenceRegion struct {
	start, end int
	reason     string
}

func referenceMask(b []byte) ([]bool, []referenceRegion) {
	mask := make([]bool, len(b))
	regions := []referenceRegion{}
	mark := func(start, end int, reason string) {
		for i := start; i < end; i++ {
			mask[i] = true
		}
		regions = append(regions, referenceRegion{start, end, reason})
	}
	fenceStart := -1
	var fence byte
	fenceLen := 0
	front := -1
	for start := 0; start < len(b); {
		end := bytes.IndexByte(b[start:], '\n')
		if end < 0 {
			end = len(b)
		} else {
			end += start + 1
		}
		line := strings.TrimRight(string(b[start:end]), "\r\n")
		if start == 0 && line == "---" {
			front = 0
			start = end
			continue
		}
		if front >= 0 {
			if line == "---" || line == "..." {
				mark(front, end, "frontmatter")
				front = -1
			}
			start = end
			continue
		}
		ch, run, rest := referenceFenceLine(line)
		if fenceStart >= 0 {
			if ch == fence && run >= fenceLen && strings.TrimSpace(rest) == "" {
				mark(fenceStart, end, "code_fence")
				fenceStart = -1
			}
			start = end
			continue
		}
		if run >= 3 && !(ch == '`' && strings.ContainsRune(rest, '`')) {
			fenceStart = start
			fence = ch
			fenceLen = run
			start = end
			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			mark(start, end, "indented_code")
		}
		start = end
	}
	if front >= 0 {
		mark(front, len(b), "frontmatter")
	}
	if fenceStart >= 0 {
		mark(fenceStart, len(b), "code_fence")
	}
	for i := 0; i < len(b); {
		if mask[i] {
			i++
			continue
		}
		open, close, reason := "", "", ""
		if bytes.HasPrefix(b[i:], []byte("<!--")) {
			open = "<!--"
			close = "-->"
			reason = "html_comment"
		} else if bytes.HasPrefix(b[i:], []byte("%%")) {
			open = "%%"
			close = "%%"
			reason = "obsidian_comment"
		}
		if open != "" {
			end := bytes.Index(b[i+len(open):], []byte(close))
			if end < 0 {
				end = len(b)
			} else {
				end += i + len(open) + len(close)
			}
			mark(i, end, reason)
			i = end
			continue
		}
		if b[i] == '`' {
			begin := i
			closed := false
			run := 1
			for i+run < len(b) && b[i+run] == '`' {
				run++
			}
			for j := i + run; j < len(b); j++ {
				if mask[j] {
					continue
				}
				if b[j] == '`' {
					n := 1
					for j+n < len(b) && b[j+n] == '`' {
						n++
					}
					if n == run {
						mark(i, j+n, "inline_code")
						i = j + n
						closed = true
						break
					}
					j += n - 1
				}
			}
			if !closed {
				i = begin + run
			}
			continue
		}
		i++
	}
	return mask, regions
}

// A fence starts only at the beginning of a line after at most three spaces.
// Backtick info strings cannot contain backticks; a closer contains no info.
// Other backtick runs are handled by the inline-code pass, never as fences.
func referenceFenceLine(line string) (byte, int, string) {
	i := 0
	for i < len(line) && line[i] == ' ' && i < 4 {
		i++
	}
	if i >= len(line) || i > 3 || (line[i] != '`' && line[i] != '~') {
		return 0, 0, ""
	}
	ch := line[i]
	run := 1
	for i+run < len(line) && line[i+run] == ch {
		run++
	}
	if run < 3 {
		return 0, 0, ""
	}
	return ch, run, line[i+run:]
}
func referenceMasked(mask []bool, start, end int) bool {
	if start < 0 || end > len(mask) || end < start {
		return true
	}
	for _, v := range mask[start:end] {
		if v {
			return true
		}
	}
	return false
}
func referenceEscaped(b []byte, i int) bool {
	n := 0
	for i--; i >= 0 && b[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}
func referenceLabelEnd(b []byte, start int, mask []bool) int {
	depth := 1
	for i := start + 1; i < len(b); i++ {
		if mask[i] || b[i] == '\n' || b[i] == '\r' {
			return -1
		}
		if (b[i] == '[' || b[i] == ']') && referenceEscaped(b, i) {
			continue
		}
		if b[i] == '[' {
			depth++
		}
		if b[i] == ']' {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// For an unsupported label, inspect its whole balanced structure, including a
// following destination, rather than just its first line. No edits are made.
// The caller advances to the returned end, so malformed multiline input is
// consumed once. If no safe closing boundary exists, conservatively inspect
// the remaining bounded note and leave selected targets unresolved.
func referenceUnsupportedLabelSpan(b []byte, start int, mask []bool) int {
	depth := 1
	for i := start + 1; i < len(b); i++ {
		if mask[i] {
			continue
		}
		if (b[i] == '[' || b[i] == ']') && referenceEscaped(b, i) {
			continue
		}
		if b[i] == '[' {
			depth++
		}
		if b[i] != ']' {
			continue
		}
		depth--
		if depth != 0 {
			continue
		}
		end := i + 1
		if end >= len(b) || b[end] != '(' {
			return end
		}
		parentheses := 1
		var quote byte
		for j := end + 1; j < len(b); j++ {
			if mask[j] {
				continue
			}
			if b[j] == '\\' {
				j++
				continue
			}
			if quote != 0 {
				if b[j] == quote {
					quote = 0
				}
				continue
			}
			if b[j] == '"' || b[j] == '\'' {
				quote = b[j]
				continue
			}
			if b[j] == '(' {
				parentheses++
			}
			if b[j] == ')' {
				parentheses--
				if parentheses == 0 {
					return j + 1
				}
			}
		}
		return len(b)
	}
	return len(b)
}

// A quoted attribute may contain '>'; it cannot terminate the unsupported tag
// before a later mapped attribute. An unterminated quote has no safe boundary.
func referenceUnsupportedHTMLEnd(b []byte, start int) int {
	var quote byte
	for i := start + 1; i < len(b); i++ {
		if quote != 0 {
			if b[i] == quote {
				quote = 0
			}
			continue
		}
		if b[i] == '\'' || b[i] == '"' {
			quote = b[i]
			continue
		}
		if b[i] == '>' {
			return i + 1
		}
	}
	return -1
}
func referenceMarkdownDestination(b []byte, start int) (ts, te, end int, ok bool) {
	i := start
	multiline := false
	space := func() {
		for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
			if b[i] == '\r' || b[i] == '\n' {
				multiline = true
			}
			i++
		}
	}
	space()
	ts = i
	if i >= len(b) {
		return ts, ts, i, false
	}
	if b[i] == '<' {
		ts = i + 1
		i++
		for i < len(b) && b[i] != '>' && b[i] != '\r' && b[i] != '\n' {
			i++
		}
		te = i
		if i >= len(b) || b[i] != '>' {
			return ts, te, i, false
		}
		i++
	} else {
		depth := 0
		for i < len(b) {
			ch := b[i]
			if ch == '\\' {
				i += 2
				continue
			}
			if ch == '(' {
				depth++
			}
			if ch == ')' {
				if depth == 0 {
					break
				}
				depth--
			}
			if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
				break
			}
			i++
		}
		te = min(i, len(b))
	}
	space()
	if i < len(b) && (b[i] == '"' || b[i] == '\'') {
		quote := b[i]
		i++
		for i < len(b) && (b[i] != quote || referenceEscaped(b, i)) && b[i] != '\r' && b[i] != '\n' {
			i++
		}
		if i >= len(b) || b[i] != quote {
			return ts, te, i, false
		}
		i++
		space()
	}
	if i < len(b) && b[i] == ')' {
		return ts, te, i + 1, !multiline && ts < te && !bytes.ContainsRune(b[ts:te], '\\')
	}
	return ts, te, min(i+1, len(b)), false
}
