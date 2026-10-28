package adapter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// VersionRange is the inclusive range of CLI versions an adapter was
// tested on.
type VersionRange struct {
	Min, Max string
}

func (r VersionRange) String() string {
	if r.Min == r.Max {
		return r.Min
	}
	return r.Min + " to " + r.Max
}

// versionRE is x.y or x.y.z, with an optional v, a semver pre-release, and
// build metadata.
var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// CompareVersions compares two versions the semver way: -1, 0, or 1. x.y
// means x.y.0. A pre-release sorts before its release, and pre-release
// identifiers compare numerically when both are numbers (beta.10 after
// beta.9), as strings otherwise, with numbers before strings and a
// shorter list first when one is a prefix of the other. Build metadata is
// ignored.
func CompareVersions(a, b string) (int, error) {
	ma, mb := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if ma == nil {
		return 0, fmt.Errorf("adapter: %q is not a version", a)
	}
	if mb == nil {
		return 0, fmt.Errorf("adapter: %q is not a version", b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.ParseUint(ma[i], 10, 64) // "" is 0
		y, _ := strconv.ParseUint(mb[i], 10, 64)
		if c := cmp(x, y); c != 0 {
			return c, nil
		}
	}
	pa, pb := ma[4], mb[4]
	switch {
	case pa == pb:
		return 0, nil
	case pa == "":
		return 1, nil
	case pb == "":
		return -1, nil
	}
	ia, ib := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(ia) && i < len(ib); i++ {
		na, ea := strconv.ParseUint(ia[i], 10, 64)
		nb, eb := strconv.ParseUint(ib[i], 10, 64)
		var c int
		switch {
		case ea == nil && eb == nil:
			c = cmp(na, nb)
		case ea == nil:
			c = -1
		case eb == nil:
			c = 1
		default:
			c = strings.Compare(ia[i], ib[i])
		}
		if c != 0 {
			return c, nil
		}
	}
	return cmp(uint64(len(ia)), uint64(len(ib))), nil
}

func cmp(x, y uint64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// Contains reports whether version is within the range.
func (r VersionRange) Contains(version string) (bool, error) {
	lo, err := CompareVersions(version, r.Min)
	if err != nil {
		return false, err
	}
	hi, err := CompareVersions(version, r.Max)
	if err != nil {
		return false, err
	}
	return lo >= 0 && hi <= 0, nil
}

// VersionWarning returns a warning when version, as a probe found it, is
// outside the versions the adapter was tested on, or empty or unreadable
// (a probe that could not read --version records ""); "" when it is
// inside. aggy doctor shows it.
func VersionWarning(a Adapter, version string) string {
	r := a.TestedVersions()
	ok, err := r.Contains(version)
	switch {
	case err != nil:
		return fmt.Sprintf("%s: cannot read the CLI version %q; aggy was tested on %s", a.Vendor(), version, r)
	case !ok:
		return fmt.Sprintf("%s %s is outside the versions aggy was tested on (%s); flags or output formats may have changed", a.Vendor(), version, r)
	}
	return ""
}
