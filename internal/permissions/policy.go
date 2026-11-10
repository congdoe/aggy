package permissions

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// The safety policy's rule groups (internal/parity renders their rules). On
// screen each group asks by default; a person moves one to deny, for this repo
// or everywhere, with aggy permissions policy, kept under "policy" in
// permissions.json. A borrow child gets every group as deny, whatever is set.
const (
	PolicyEnv       = "env-read"        // reading .env files
	PolicyKeys      = "key-read"        // reading private key files: strict
	PolicyPushMain  = "push-main"       // a push to main
	PolicyForce     = "force-push"      // a force push to any branch
	PolicyForceMain = "force-push-main" // a force push to main or the default branch: strict
	PolicyRmRf      = "rm-rf"           // a recursive forced delete
	PolicyReset     = "reset-hard"      // git reset --hard
	PolicyClean     = "git-clean"       // git clean
)

// PolicyGroups lists them in the order aggy shows them.
var PolicyGroups = []string{PolicyEnv, PolicyKeys, PolicyPushMain, PolicyForce, PolicyForceMain, PolicyRmRf, PolicyReset, PolicyClean}

// StrictGroup says a group's prompt must not be answered for good: only an
// allow in aggy's own file that names one of its rules pre-approves it.
func StrictGroup(group string) bool {
	return group == PolicyKeys || group == PolicyForceMain
}

// CheckPolicySetting refuses a group aggy does not know or a setting other than
// ask or deny: a policy rule can never be set to allow.
func CheckPolicySetting(group, setting string) error {
	if !slices.Contains(PolicyGroups, group) {
		return fmt.Errorf("%q is not a policy group (one of %v)", group, PolicyGroups)
	}
	if setting != string(Ask) && setting != string(Deny) {
		return fmt.Errorf("a policy group is ask or deny, not %q", setting)
	}
	return nil
}

// loadPolicy keeps valid settings. A mistake is a FileErr, as a deny the person
// meant would otherwise be lost.
func (s *Set) loadPolicy(m map[string]string, from Origin, path string) {
	for _, g := range slices.Sorted(maps.Keys(m)) {
		if err := CheckPolicySetting(g, m[g]); err != nil {
			if s.FileErr == nil {
				s.FileErr = &FileError{path, 0, "policy: " + err.Error()}
			}
			continue
		}
		if s.Policy == nil {
			s.Policy = map[string]Kind{}
			s.PolicyFrom = map[string]Origin{}
		}
		s.Policy[g], s.PolicyFrom[g] = Kind(m[g]), from
	}
}

// PolicyAllows are the allows that may pre-approve a policy rule
// (parity.PolicyPlan): those that apply here, and those in aggy's own file
// held only for trust, since the person wrote them and a repo cannot.
func (s Set) PolicyAllows() []Rule {
	var out []Rule
	for _, r := range s.Rules {
		if r.Kind == Allow {
			out = append(out, r)
		}
	}
	for _, k := range s.Skipped {
		if k.Kind != Allow || (k.Origin != FromAggy && k.Origin != FromAggyRepo) {
			continue
		}
		if class, _ := CheckAllow(k.Text); class != Drop && !slices.Contains(out, k.Rule) {
			out = append(out, k.Rule)
		}
	}
	return out
}

// PolicyEdit sets one group's setting in aggy's own file. RepoKey "" means
// every repo.
type PolicyEdit struct {
	Group, Setting, RepoKey string
}

// Apply sets the group, deterministically, as internal/patch needs.
func (e PolicyEdit) Apply(src []byte) ([]byte, error) {
	if err := CheckPolicySetting(e.Group, e.Setting); err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if len(src) > 0 {
		if _, err := ParseAggyFile(src); err != nil {
			return nil, fmt.Errorf("%s is not valid: %w; aggy doctor says where", FileName, err)
		}
		if err := json.Unmarshal(src, &doc); err != nil {
			return nil, err
		}
	}
	section := func(m map[string]any, key string) map[string]any {
		if sub, ok := m[key].(map[string]any); ok {
			return sub
		}
		sub := map[string]any{}
		m[key] = sub
		return sub
	}
	object := doc
	if e.RepoKey != "" {
		repos := section(doc, "repos")
		object = section(repos, matchingKey(slices.Collect(maps.Keys(repos)), e.RepoKey))
	}
	section(object, "policy")[e.Group] = e.Setting
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// PolicySetting returns the group's setting in the file at that scope, or "".
func PolicySetting(src []byte, group, repoKey string) (string, error) {
	if len(src) == 0 {
		return "", nil
	}
	f, err := ParseAggyFile(src)
	if err != nil {
		return "", err
	}
	if repoKey == "" {
		return f.Policy[group], nil
	}
	return f.Repos[matchingKey(slices.Collect(maps.Keys(f.Repos)), repoKey)].Policy[group], nil
}
