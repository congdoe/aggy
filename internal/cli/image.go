package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/termsafe"
)

const imageUsage = `Usage: aggy image "<brief>" --out PATH [--vendor codex|grok] [--ref IMG]... [--n N]

Makes an image in another vendor's CLI, read-only in an empty temp dir,
and copies it to --out. The brief says the purpose, size, exact text,
and style.

  --out PATH       where the image goes, inside this repo (.png, .jpg, .jpeg)
  --ref IMG        a reference image, PNG or JPEG (repeatable; Codex only)
  --n N            ask for N variants, 1 to 4: PATH, then PATH-2, ...
  --vendor V       codex or grok, skipping routing
  --allow-untested run a CLI whose version is outside the versions the
                   borrow lockdown was verified on
`

// mediaArgs is a parsed aggy image or aggy video command line.
type mediaArgs struct {
	brief         string
	out           string
	refs          []string
	n             int
	vendor        string
	allowUntested bool
	help          bool
}

// parseMediaArgs reads flags anywhere on the line; "--" ends them. The
// one positional is the brief. flags are the value flags this command
// takes.
func parseMediaArgs(args []string, flags []string) (mediaArgs, error) {
	var m mediaArgs
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
			continue
		case a == "-h" || a == "--help":
			m.help = true
			continue
		case a == "--allow-untested":
			m.allowUntested = true
			continue
		case !strings.HasPrefix(a, "-") || a == "-":
			pos = append(pos, a)
			continue
		}
		name, val, has := strings.Cut(a, "=")
		if !slices.Contains(flags, name) {
			return m, fmt.Errorf("unknown flag %s", name)
		}
		if !has {
			if i+1 >= len(args) {
				return m, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		switch name {
		case "--out":
			m.out = val
		case "--ref":
			m.refs = append(m.refs, val)
		case "--vendor":
			m.vendor = val
		case "--n":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 || n > borrow.MaxImages {
				return m, fmt.Errorf("--n must be a number from 1 to %d", borrow.MaxImages)
			}
			m.n = n
		}
	}
	if m.help {
		return m, nil
	}
	if len(pos) != 1 {
		return m, errors.New("want one quoted brief")
	}
	m.brief = pos[0]
	return m, nil
}

func runImage(args []string, stdout, stderr io.Writer) int {
	return mediaWith(adapter.Image, args, defaultBorrowEnv(), stdout, stderr)
}

// mediaWith runs aggy image or aggy video: a borrow of kind with file
// arguments.
func mediaWith(kind adapter.Kind, args []string, env borrowEnv, stdout, stderr io.Writer) int {
	name, usage, flags := "aggy image", imageUsage, []string{"--out", "--ref", "--n", "--vendor"}
	ma, err := parseMediaArgs(args, flags)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s\n\n%s", name, termsafe.Field(err.Error()), usage)
		return 2
	}
	if ma.help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if ma.out == "" {
		fmt.Fprintf(stderr, "%s: --out is required\n\n%s", name, usage)
		return 2
	}
	cwd, err := env.cwd()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	media := &borrow.Media{Out: absIn(cwd, ma.out), N: ma.n}
	for _, r := range ma.refs {
		media.Refs = append(media.Refs, absIn(cwd, r))
	}
	bargs := []string{string(kind)}
	if ma.vendor != "" {
		bargs = append(bargs, "--vendor", ma.vendor)
	}
	if ma.allowUntested {
		bargs = append(bargs, "--allow-untested")
	}
	return borrowMedia(append(bargs, "--", ma.brief), media, env, stdout, stderr)
}

// absIn is p made absolute against dir, and cleaned.
func absIn(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.Clean(p)
}
