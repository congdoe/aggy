package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fake-keychain stands in for macOS's /usr/bin/security and Linux's
// /usr/bin/secret-tool (aggy key, T6931), so no test ever touches a real
// keychain. It takes the argument lists internal/keyring builds, in either
// tool's form, and keeps items as files in $AGGY_FAKE_KEYCHAIN, a folder the
// test makes outside its temp HOME. Each call appends its arguments (never a
// value) to calls.jsonl there.
//
// Where it differs from the real tools: security reads the value from
// /dev/tty, and this fake reads it from its stdin, which aggy gives as the
// person's terminal (the same terminal in a test's pseudo-terminal); it turns
// echo off there as both tools do. $AGGY_FAKE_KEYCHAIN_DENY=1 makes every
// read fail as a Deny in macOS's dialog would (exit 51, unverified).
//
// The item's list of trusted programs (T6931, #138 review H1) is modelled as
// a sidecar file, acl.<service>.<account>: "empty" for an item added with
// -T "", "trusted" for one added without -T (security trusts the program that
// adds it), or one a test planted. -U on an item already there keeps its old
// list, the case the review feared (unverified live), so aggy never passes
// -U. $AGGY_FAKE_KEYCHAIN_HANG=1 makes an add turn echo off and then hang,
// never restoring the terminal, as a tool killed mid-prompt leaves it.
// $AGGY_FAKE_KEYCHAIN_NODELETE=1 makes every delete (and secret-tool clear)
// fail.

// keychainCall is one line of calls.jsonl.
type keychainCall struct {
	Args  []string `json:"args"`
	Stdin bool     `json:"stdin_terminal"`
}

func runKeychain() int {
	dir := os.Getenv("AGGY_FAKE_KEYCHAIN")
	if !filepath.IsAbs(dir) {
		return fail(97, "keychain: want AGGY_FAKE_KEYCHAIN, an absolute folder")
	}
	args := os.Args[1:]
	appendJSON(filepath.Join(dir, "calls.jsonl"), keychainCall{Args: args, Stdin: isTerminal(os.Stdin)})
	if len(args) == 0 {
		return fail(2, "keychain: no command")
	}
	switch args[0] {
	case "add-generic-password", "find-generic-password", "delete-generic-password":
		return fakeSecurity(dir, args)
	case "store", "lookup", "clear":
		return fakeSecretTool(dir, args)
	}
	return fail(2, "keychain: unknown command %q", args[0])
}

// itemPath is where the fake keeps the item for service and account.
func itemPath(dir, service, account string) (string, error) {
	for _, s := range []string{service, account} {
		if s == "" || strings.ContainsAny(s, "/\x00") || s == "." || s == ".." {
			return "", fmt.Errorf("bad service or account %q", s)
		}
	}
	return filepath.Join(dir, "item."+service+"."+account), nil
}

// aclPath is the sidecar holding the item's list of trusted programs.
func aclPath(item string) string {
	return filepath.Join(filepath.Dir(item), "acl."+strings.TrimPrefix(filepath.Base(item), "item."))
}

// fakeSecurity follows security(1)'s generic-password commands, with -s, -a,
// -l, -U, -T, and -w.
func fakeSecurity(dir string, args []string) int {
	opts := map[string]string{}
	flags := map[string]bool{}
	for i := 1; i < len(args); i++ {
		switch a := args[i]; a {
		case "-s", "-a", "-l", "-T":
			if i+1 >= len(args) {
				return fail(2, "security: option %s needs a value", a)
			}
			opts[a] = args[i+1]
			i++
		case "-U", "-g":
			flags[a] = true
		case "-w":
			// A value after -w would put the key in the argument list:
			// aggy never does that, and the fake refuses it.
			if args[0] == "add-generic-password" && i != len(args)-1 {
				return fail(2, "security: fake refuses -w with a value: the key would be in argv")
			}
			flags[a] = true
		default:
			return fail(2, "security: unknown option %q", a)
		}
	}
	p, err := itemPath(dir, opts["-s"], opts["-a"])
	if err != nil {
		return fail(2, "security: %v", err)
	}
	_, statErr := os.Stat(p)
	exists := statErr == nil
	switch args[0] {
	case "add-generic-password":
		if exists && !flags["-U"] {
			fmt.Fprintln(os.Stderr, "security: SecKeychainItemCreateFromContent (<default>): The specified item already exists in the keychain.")
			return 45
		}
		if !flags["-w"] {
			return fail(2, "security: fake wants -w last, to ask for the value")
		}
		if os.Getenv("AGGY_FAKE_KEYCHAIN_HANG") == "1" {
			if _, err := noEcho(0); err != nil {
				return fail(1, "security: %v", err)
			}
			fmt.Fprint(os.Stderr, "password data for new item: ")
			time.Sleep(time.Hour)
			return 1
		}
		acl := "trusted"
		if t, ok := opts["-T"]; ok && t == "" {
			acl = "empty"
		}
		if exists {
			// -U: the item's old list stays.
			if b, err := os.ReadFile(aclPath(p)); err == nil {
				acl = string(b)
			}
		}
		first, err := askValue("password data for new item: ")
		if err != nil {
			return fail(1, "security: %v", err)
		}
		again, err := askValue("retype password for new item: ")
		if err != nil || again != first {
			fmt.Fprintln(os.Stderr, "security: passwords don't match")
			return 1
		}
		if err := os.WriteFile(p, []byte(first), 0o600); err != nil {
			return fail(1, "security: %v", err)
		}
		if err := os.WriteFile(aclPath(p), []byte(acl), 0o600); err != nil {
			return fail(1, "security: %v", err)
		}
		return 0
	case "find-generic-password":
		if !exists {
			fmt.Fprintln(os.Stderr, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
			return 44
		}
		if !flags["-w"] {
			fmt.Printf("keychain: \"fake\"\nattributes:\n    \"svce\"<blob>=%q\n    \"acct\"<blob>=%q\n", opts["-s"], opts["-a"])
			return 0
		}
		if os.Getenv("AGGY_FAKE_KEYCHAIN_DENY") == "1" {
			fmt.Fprintln(os.Stderr, "security: SecKeychainItemCopyAccess: User interaction is not allowed.")
			return 51
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fail(1, "security: %v", err)
		}
		fmt.Printf("%s\n", b)
		return 0
	case "delete-generic-password":
		if os.Getenv("AGGY_FAKE_KEYCHAIN_NODELETE") == "1" {
			// As a Deny in a dialog for deleting an item that trusts no
			// program might (unverified).
			fmt.Fprintln(os.Stderr, "security: SecKeychainItemDelete: User interaction is not allowed.")
			return 51
		}
		if !exists {
			fmt.Fprintln(os.Stderr, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
			return 44
		}
		if err := os.Remove(p); err != nil {
			return fail(1, "security: %v", err)
		}
		_ = os.Remove(aclPath(p))
		return 0
	}
	return 2
}

// fakeSecretTool follows secret-tool(1): store [--label=L] attr value...,
// lookup attr value..., clear attr value..., with the service and account
// attributes.
func fakeSecretTool(dir string, args []string) int {
	attrs := map[string]string{}
	rest := args[1:]
	if args[0] == "store" && len(rest) > 0 && strings.HasPrefix(rest[0], "--label=") {
		rest = rest[1:]
	}
	if len(rest)%2 != 0 {
		return fail(2, "secret-tool: attributes come in pairs")
	}
	for i := 0; i < len(rest); i += 2 {
		attrs[rest[i]] = rest[i+1]
	}
	p, err := itemPath(dir, attrs["service"], attrs["account"])
	if err != nil {
		return fail(2, "secret-tool: %v", err)
	}
	switch args[0] {
	case "store":
		var v string
		if isTerminal(os.Stdin) {
			if v, err = askValue("Password: "); err != nil {
				return fail(1, "secret-tool: %v", err)
			}
		} else {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<16))
			if err != nil {
				return fail(1, "secret-tool: %v", err)
			}
			v = string(b)
		}
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			return fail(1, "secret-tool: %v", err)
		}
		return 0
	case "lookup":
		if os.Getenv("AGGY_FAKE_KEYCHAIN_DENY") == "1" {
			return 1
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return 1
		}
		_, _ = os.Stdout.Write(b)
		return 0
	case "clear":
		if os.Getenv("AGGY_FAKE_KEYCHAIN_NODELETE") == "1" {
			return 1
		}
		_ = os.Remove(p)
		return 0
	}
	return 2
}

// keychainIn is stdin, read through one buffer, so the retype prompt reads
// the line after the first.
var keychainIn = bufio.NewReader(os.Stdin)

// askValue prompts on stderr and reads one line from stdin, with echo off
// when stdin is a terminal.
func askValue(prompt string) (string, error) {
	// Echo goes off before the prompt shows, so nothing typed after it is
	// echoed.
	if isTerminal(os.Stdin) {
		if st, err := noEcho(0); err == nil {
			defer func() { _ = restoreTerm(0, st) }()
		}
	}
	fmt.Fprint(os.Stderr, prompt)
	line, err := keychainIn.ReadString('\n')
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return "", fmt.Errorf("no value: %v", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
