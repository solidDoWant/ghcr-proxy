package config

import (
	"fmt"
	"regexp"
	"strings"
)

type rawGrant struct {
	Namespaces   string   `json:"namespaces"`
	Repositories []string `json:"repositories"`
	Actions      []string `json:"actions"`
}

// ExpandedGrant is one grant applied to one namespace: concrete repository
// glob patterns and the actions allowed on them. Actions include the ones
// implied by push and delete (pull).
type ExpandedGrant struct {
	Patterns []string
	Actions  []Action
}

type grant struct {
	namespaces   *regexp.Regexp
	repositories []string
	actions      []Action
}

// Grants are the compiled grants from the config file.
type Grants struct {
	grants []grant
}

var (
	captureRef    = regexp.MustCompile(`\$\{([^}]*)\}`)
	captureName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	captureValue  = regexp.MustCompile(`^[a-z0-9._-]+$`)
	segmentRegexp = regexp.MustCompile(`^[a-z0-9._*-]+$`)
)

// validGlob reports whether pattern satisfies the repository glob grammar:
// non-empty segments separated by "/", each exactly "**" or a run of
// [a-z0-9._-] and single "*".
func validGlob(pattern string) bool {
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "**" {
			continue
		}

		if !segmentRegexp.MatchString(segment) || strings.Contains(segment, "**") {
			return false
		}
	}

	return true
}

func compileGrants(raw []rawGrant) (Grants, []error) {
	var (
		errs []error
		out  Grants
	)

	for i, rg := range raw {
		prefix := fmt.Sprintf("grants[%d]", i)
		g := grant{}

		re, err := regexp.Compile("^(?:" + rg.Namespaces + ")$")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.namespaces: invalid regular expression: %w", prefix, err))
		} else {
			g.namespaces = re
		}

		if rg.Namespaces == "" {
			errs = append(errs, fmt.Errorf("%s.namespaces: must not be empty", prefix))
		}

		if len(rg.Repositories) == 0 {
			errs = append(errs, fmt.Errorf("%s.repositories: must not be empty", prefix))
		}

		for j, pattern := range rg.Repositories {
			ok := true

			for _, m := range captureRef.FindAllStringSubmatch(pattern, -1) {
				if !captureName.MatchString(m[1]) || re == nil || re.SubexpIndex(m[1]) < 0 {
					errs = append(errs, fmt.Errorf("%s.repositories[%d]: unknown capture name %q", prefix, j, m[1]))
					ok = false
				}
			}

			if ok && !validGlob(captureRef.ReplaceAllString(pattern, "x")) {
				errs = append(errs, fmt.Errorf("%s.repositories[%d]: %q is not a valid repository pattern", prefix, j, pattern))
			}

			g.repositories = append(g.repositories, pattern)
		}

		if len(rg.Actions) == 0 {
			errs = append(errs, fmt.Errorf("%s.actions: must not be empty", prefix))
		}

		seen := map[Action]bool{}

		for _, a := range rg.Actions {
			switch Action(a) {
			case ActionPull, ActionPush, ActionDelete:
				seen[Action(a)] = true
			default:
				errs = append(errs, fmt.Errorf("%s.actions: unknown action %q", prefix, a))
			}
		}

		if len(seen) > 0 {
			seen[ActionPull] = true
		}

		for _, a := range []Action{ActionPull, ActionPush, ActionDelete} {
			if seen[a] {
				g.actions = append(g.actions, a)
			}
		}

		out.grants = append(out.grants, g)
	}

	return out, errs
}

// For returns the grants that apply to namespace, with captures expanded. A
// grant that matches the namespace but yields an empty, unmatched or unsafe
// capture value, or an invalid expanded pattern, is left out.
func (g Grants) For(namespace string) []ExpandedGrant {
	var out []ExpandedGrant

	for _, gr := range g.grants {
		if gr.namespaces == nil {
			continue
		}

		match := gr.namespaces.FindStringSubmatchIndex(namespace)
		if match == nil {
			continue
		}

		patterns, ok := expandPatterns(gr, namespace, match)
		if !ok {
			continue
		}

		out = append(out, ExpandedGrant{Patterns: patterns, Actions: append([]Action(nil), gr.actions...)})
	}

	return out
}

func expandPatterns(gr grant, namespace string, match []int) ([]string, bool) {
	patterns := make([]string, 0, len(gr.repositories))
	for _, pattern := range gr.repositories {
		ok := true

		expanded := captureRef.ReplaceAllStringFunc(pattern, func(ref string) string {
			idx := gr.namespaces.SubexpIndex(ref[2 : len(ref)-1])
			if idx < 0 || match[2*idx] < 0 {
				ok = false
				return ""
			}

			value := strings.ToLower(namespace[match[2*idx]:match[2*idx+1]])
			if !captureValue.MatchString(value) {
				ok = false
			}

			return value
		})
		if !ok || !validGlob(expanded) {
			return nil, false
		}

		patterns = append(patterns, expanded)
	}

	return patterns, true
}
