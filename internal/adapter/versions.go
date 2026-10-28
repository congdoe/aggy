package adapter

import (
	"fmt"
	"regexp"
	"strconv"
)

// VersionRange is the inclusive range of CLI versions an adapter was
// tested on. Versions are x.y.z with an optional pre-release suffix,
// which sorts before its release.
type VersionRange struct {
	Min, Max string
}

func (r VersionRange) String() string {
	if r.Min == r.Max {
		return r.Min
	}
	return r.Min + " to " + r.Max
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// CompareVersions compares two x.y.z versions numerically: -1, 0, or 1. A
// pre-release sorts before its release; two pre-releases of one release
// compare as strings. Build metadata is ignored.
func CompareVersions(a, b string) (int, error) {
	ma, mb := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if ma == nil {
		return 0, fmt.Errorf("adapter: %q is not an x.y.z version", a)
	}
	if mb == nil {
		return 0, fmt.Errorf("adapter: %q is not an x.y.z version", b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.ParseUint(ma[i], 10, 64)
		y, _ := strconv.ParseUint(mb[i], 10, 64)
		switch {
		case x < y:
			return -1, nil
		case x > y:
			return 1, nil
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
	case pa < pb:
		return -1, nil
	}
	return 1, nil
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
// outside the versions the adapter was tested on, or cannot be read; ""
// when it is inside. aggy doctor shows it.
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
