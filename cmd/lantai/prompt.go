package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// stdin 是交互输入；测试替换为脚本化输入。
var stdin io.Reader = os.Stdin

// prompter 从终端读取输入：口令与动态码在终端上不回显；输入不是终端时逐行
// 读取（用于脚本化测试），并提醒调用者这不是交互终端。
type prompter struct {
	in     *bufio.Reader
	file   *os.File
	tty    bool
	out    io.Writer
	warned bool
}

func newPrompter(out io.Writer) *prompter {
	p := &prompter{in: bufio.NewReader(stdin), out: out}
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		p.file, p.tty = f, true
	}
	return p
}

var errNoInput = errors.New("no more input")

func (p *prompter) line(prompt string) (string, error) {
	fmt.Fprint(p.out, prompt)
	s, err := p.in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		fmt.Fprintln(p.out)
		if err == io.EOF {
			return "", errNoInput
		}
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// secret 读取不回显的输入。
func (p *prompter) secret(prompt string) (string, error) {
	if !p.tty {
		if !p.warned {
			fmt.Fprintln(p.out, "warning: input is not a terminal; secrets will be read without masking")
			p.warned = true
		}
		return p.line(prompt)
	}
	fmt.Fprint(p.out, prompt)
	b, err := term.ReadPassword(int(p.file.Fd()))
	fmt.Fprintln(p.out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// newPassword 读取两次并确认一致。
func (p *prompter) newPassword(what string) (string, error) {
	for range 3 {
		a, err := p.secret("New password for " + what + ": ")
		if err != nil {
			return "", err
		}
		b, err := p.secret("Repeat the password: ")
		if err != nil {
			return "", err
		}
		if a == b {
			return a, nil
		}
		fmt.Fprintln(p.out, "the passwords do not match; try again")
	}
	return "", errors.New("passwords did not match")
}
