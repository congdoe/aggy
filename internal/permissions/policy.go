package permissions

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

// The safety policy's rule groups (internal/parity renders their rules). On
// screen each group asks by default; a person moves one to deny, or turns it
// off with allow (T6979), for this repo or everywhere, with aggy permissions
// policy, kept under "policy" in permissions.json, aggy's own file: a repo's
// files never set one. A borrow child, a headless run, and print mode get
// every group as deny, whatever is set.
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

// StrictGroup says a group's prompt must not be answered for good: OpenCode,
// whose Always allow could, refuses it, and suggest never proposes it.
func StrictGroup(group string) bool {
	return group == PolicyKeys || group == PolicyForceMain
}

// CheckPolicySetting refuses a group aggy does not know or a setting other than
// ask, deny, or allow. Allow turns the group off on screen (T6979): its rules
// render as nothing there, and it never reaches a borrow, headless, or print
// render, which deny every group.
func CheckPolicySetting(group, setting string) error {
	if !slices.Contains(PolicyGroups, group) {
		return fmt.Errorf("%q is not a policy group (one of %v)", group, PolicyGroups)
	}
	if setting != string(Ask) && setting != string(Deny) && setting != string(Allow) {
		return fmt.Errorf("a policy group is ask, deny, or allow, not %q", setting)
	}
	return nil
}

// policyRank orders settings by strictness: deny, then ask, then allow.
var policyRank = []Kind{Deny, Ask, Allow}

// OffGroups lists the groups set to allow, in PolicyGroups' order.
func (s Set) OffGroups() []string {
	var out []string
	for _, g := range PolicyGroups {
		if s.Policy[g] == Allow {
			out = append(out, g)
		}
	}
	return out
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
		// Two keys of the file may name one repo (spelled two ways): of those,
		// the stricter setting wins, not the one that sorts last.
		if from == FromAggyRepo && s.PolicyFrom[g] == FromAggyRepo && slices.Index(policyRank, s.Policy[g]) < slices.Index(policyRank, Kind(m[g])) {
			continue
		}
		s.Policy[g], s.PolicyFrom[g] = Kind(m[g]), from
	}
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
