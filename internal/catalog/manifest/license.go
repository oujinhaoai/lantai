package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// CheckLicense 校验 SPDX 许可表达式的语法（SPDX 规范附录 D 的子集）：
//
//	expr   := term (("AND" | "OR") term)*
//	term   := "(" expr ")" | id ["+"] ["WITH" id]
//	id     := [A-Za-z0-9][A-Za-z0-9.-]*（含 LicenseRef-… 与 DocumentRef-…:LicenseRef-…）
//
// 运算符区分大小写。只核对语法，不核对许可列表，也不推导法律兼容性；表达式
// 是声明与证据索引，许可是否被接受由 provenance 与人审决定。
func CheckLicense(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("license expression is empty")
	}
	if len(expr) > 256 {
		return errors.New("license expression is longer than 256 characters")
	}
	toks, err := tokenize(expr)
	if err != nil {
		return err
	}
	p := &licenseParser{toks: toks}
	if err := p.expr(0); err != nil {
		return err
	}
	if p.pos != len(p.toks) {
		return fmt.Errorf("unexpected %q in license expression", p.toks[p.pos])
	}
	return nil
}

var licenseIDRE = regexp.MustCompile(`^(DocumentRef-[A-Za-z0-9.-]+:)?[A-Za-z0-9][A-Za-z0-9.-]*\+?$`)

func tokenize(s string) ([]string, error) {
	var out []string
	for _, field := range strings.Fields(s) {
		for field != "" {
			switch field[0] {
			case '(', ')':
				out = append(out, field[:1])
				field = field[1:]
				continue
			}
			i := strings.IndexAny(field, "()")
			if i < 0 {
				i = len(field)
			}
			tok := field[:i]
			switch tok {
			case "AND", "OR", "WITH":
			default:
				if !licenseIDRE.MatchString(tok) {
					return nil, fmt.Errorf("%q is not a license identifier", tok)
				}
			}
			out = append(out, tok)
			field = field[i:]
		}
	}
	return out, nil
}

type licenseParser struct {
	toks []string
	pos  int
}

func (p *licenseParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *licenseParser) expr(depth int) error {
	if depth > 16 {
		return errors.New("license expression is nested too deeply")
	}
	if err := p.term(depth); err != nil {
		return err
	}
	for {
		switch p.peek() {
		case "AND", "OR":
			p.pos++
			if err := p.term(depth); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (p *licenseParser) term(depth int) error {
	switch tok := p.peek(); tok {
	case "":
		return errors.New("license expression ends unexpectedly")
	case "(":
		p.pos++
		if err := p.expr(depth + 1); err != nil {
			return err
		}
		if p.peek() != ")" {
			return errors.New("missing ) in license expression")
		}
		p.pos++
		return nil
	case ")", "AND", "OR", "WITH":
		return fmt.Errorf("unexpected %q in license expression", tok)
	}
	p.pos++
	if p.peek() == "WITH" {
		p.pos++
		switch exc := p.peek(); exc {
		case "", "(", ")", "AND", "OR", "WITH":
			return errors.New("WITH needs an exception identifier")
		}
		p.pos++
	}
	return nil
}
