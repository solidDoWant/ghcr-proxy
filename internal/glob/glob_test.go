package glob_test

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/registrygate/internal/glob"
)

func mustParse(t *testing.T, pattern string) glob.Glob {
	t.Helper()

	parsed, err := glob.Parse(pattern)
	require.NoError(t, err, "pattern %q", pattern)

	return parsed
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{pattern: "soliddowant/foo/*", name: "soliddowant/foo/a", want: true},
		{pattern: "soliddowant/foo/*", name: "soliddowant/foo/a/b", want: false},
		{pattern: "soliddowant/foo/*", name: "soliddowant/foo", want: false},
		{pattern: "soliddowant/foo/**", name: "soliddowant/foo/a", want: true},
		{pattern: "soliddowant/foo/**", name: "soliddowant/foo/a/b", want: true},
		{pattern: "soliddowant/foo/**", name: "soliddowant/foo", want: true},
		{pattern: "soliddowant/foo/**", name: "soliddowant/foobar", want: false},
		{pattern: "soliddowant/foo", name: "soliddowant/foo", want: true},
		{pattern: "soliddowant/foo", name: "soliddowant/foo/a", want: false},
		{pattern: "soliddowant/app-*", name: "soliddowant/app-web", want: true},
		{pattern: "soliddowant/app-*", name: "soliddowant/app-", want: true},
		{pattern: "soliddowant/app-*", name: "soliddowant/api", want: false},
		{pattern: "soliddowant/**/cache", name: "soliddowant/cache", want: true},
		{pattern: "soliddowant/**/cache", name: "soliddowant/a/b/cache", want: true},
		{pattern: "soliddowant/**/cache", name: "soliddowant/a/b/cache2", want: false},
		{pattern: "**", name: "soliddowant/a", want: true},
		{pattern: "soliddowant/*", name: "SolidDoWant/a", want: false},
		{pattern: "soliddowant/*", name: "soliddowant//", want: false},
		{pattern: "**", name: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.pattern+" "+test.name, func(t *testing.T) {
			assert.Equal(t, test.want, mustParse(t, test.pattern).Match(test.name))
		})
	}
}

func TestContains(t *testing.T) {
	tests := []struct {
		outer string
		inner string
		want  bool
	}{
		{outer: "soliddowant/foo/*", inner: "soliddowant/foo/**", want: false},
		{outer: "soliddowant/foo/**", inner: "soliddowant/foo/*", want: true},
		{outer: "soliddowant/**", inner: "soliddowant/foo", want: true},
		{outer: "a/**/b", inner: "a/**", want: false},
		{outer: "**/b", inner: "**", want: false},
		{outer: "**", inner: "a/**/b", want: true},
		{outer: "a/*", inner: "a/b", want: true},
		{outer: "a/b", inner: "a/*", want: false},
		{outer: "a/*", inner: "a/x*", want: true},
		{outer: "a/x*", inner: "a/x*", want: true},
		{outer: "a/x*", inner: "a/*", want: false},
		{outer: "a/x*", inner: "a/xy", want: true},
		{outer: "a/x*", inner: "a/yx", want: false},
		{outer: "a/**", inner: "a", want: true},
		{outer: "a/**", inner: "b/**", want: false},
		{outer: "**", inner: "**", want: true},
		{outer: "a/**/**", inner: "a/**", want: true},
		{outer: "a/b", inner: "a/b", want: true},
		{outer: "a/b", inner: "a/b/c", want: false},
	}
	for _, test := range tests {
		t.Run(test.outer+" contains "+test.inner, func(t *testing.T) {
			assert.Equal(t, test.want, glob.Contains(mustParse(t, test.outer), mustParse(t, test.inner)))
		})
	}
}

// enumerateNames returns every name of one to four segments over a small
// segment alphabet. Patterns built from the same letters can be told apart
// by at least one of these names whenever they differ.
func enumerateNames() []string {
	segments := []string{"a", "b", "ab", "ba", "aa", "aba"}
	names := []string{}
	current := []string{""}

	for depth := 1; depth <= 4; depth++ {
		next := []string{}

		for _, prefix := range current {
			for _, segment := range segments {
				name := segment
				if prefix != "" {
					name = prefix + "/" + segment
				}

				next = append(next, name)
			}
		}

		names = append(names, next...)
		current = next
	}

	return names
}

// counterexample returns a name that matches inner but not outer, if the
// name set holds one.
func counterexample(outer, inner glob.Glob, names []string) (string, bool) {
	for _, name := range names {
		if inner.Match(name) && !outer.Match(name) {
			return name, true
		}
	}

	return "", false
}

func TestContainsIsSoundOverPatternTable(t *testing.T) {
	pairs := [][2]string{
		{"**", "a"},
		{"**", "**"},
		{"**", "a/**/b"},
		{"**", "*/a*"},
		{"**/a", "**"},
		{"**/a", "a"},
		{"**/a", "b/a"},
		{"**/a", "**/a"},
		{"**/a", "**/*a"},
		{"**/*a", "**/a"},
		{"**/a", "*/**/a"},
		{"*/**/a", "**/a"},
		{"**/b", "a/**/b"},
		{"a/**/b", "a/b"},
		{"a/**/b", "a/*/b"},
		{"a/**/b", "a/**/b"},
		{"a/**/b", "a/**/a/b"},
		{"a/**/b", "a/**"},
		{"a/**/b", "**/b"},
		{"a/*/b", "a/**/b"},
		{"a/**", "a/*"},
		{"a/**", "a/**"},
		{"a/**", "a/b/**"},
		{"a/**", "a"},
		{"a/**", "**"},
		{"a/*", "a/**"},
		{"a/*", "a/a*"},
		{"a/a*", "a/*"},
		{"a/*a", "a/a*"},
		{"a/a*", "a/a*b"},
		{"a/a*b", "a/a*b"},
		{"a/a*b", "a/ab"},
		{"a/*b*", "a/b"},
		{"*/*", "a/b"},
		{"*", "**"},
		{"a/**/**/b", "a/**/b"},
		{"**/a/**", "a"},
		{"**/a/**", "b/a/b"},
		{"**/a/**", "**/a/b"},
	}
	names := enumerateNames()

	for _, pair := range pairs {
		t.Run(pair[0]+" contains "+pair[1], func(t *testing.T) {
			outer, inner := mustParse(t, pair[0]), mustParse(t, pair[1])
			if !glob.Contains(outer, inner) {
				return
			}

			name, found := counterexample(outer, inner, names)
			assert.False(t, found, "%q matches %q but not %q", name, pair[1], pair[0])
		})
	}
}

func TestContainsIsSoundOverRandomPatterns(t *testing.T) {
	segments := []string{"a", "b", "ab", "*", "a*", "*a", "a*b", "*b*", "**"}
	random := rand.New(rand.NewPCG(1, 2))
	randomPattern := func() glob.Glob {
		parts := make([]string, 1+random.IntN(3))
		for index := range parts {
			parts[index] = segments[random.IntN(len(segments))]
		}

		return mustParse(t, strings.Join(parts, "/"))
	}
	names := enumerateNames()
	contained := 0

	for range 5000 {
		outer, inner := randomPattern(), randomPattern()
		if !glob.Contains(outer, inner) {
			continue
		}

		contained++
		name, found := counterexample(outer, inner, names)
		require.False(t, found, "%q matches %q but not %q", name, inner, outer)
	}
	// The check is only meaningful if containment holds for a good share of pairs.
	require.Greater(t, contained, 200)
}

func TestParseRejectsPatternsOutsideTheGrammar(t *testing.T) {
	patterns := []string{
		"",
		"Soliddowant/foo",
		"soliddowant/FOO",
		"soliddowant/foo**",
		"soliddowant/**foo",
		"soliddowant/a**b",
		"soliddowant/***",
		"soliddowant//foo",
		"/soliddowant/foo",
		"soliddowant/foo/",
		"/",
		"soliddowant/fo?",
		"soliddowant/[ab]",
		"soliddowant/{a,b}",
		"soliddowant/a\\*",
		"soliddowant/!foo",
		"soliddowant/fo o",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			_, err := glob.Parse(pattern)
			require.Error(t, err)
		})
	}
}

func TestParseAcceptsPatternsInTheGrammar(t *testing.T) {
	patterns := []string{"**", "*", "a", "soliddowant/foo", "soliddowant/**", "**/cache", "a/**/b", "a/*x*y*", "a.b_c-d/0-9", "a/**/**"}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			parsed := mustParse(t, pattern)
			assert.Equal(t, pattern, parsed.String())
		})
	}
}

// referenceMatch is the matching rule written out segment by segment: `*`
// matches any run of the name alphabet within a segment, `**` matches zero
// or more whole segments.
var segmentRegexps = map[string]*regexp.Regexp{}

func referenceMatch(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}

	if pattern[0] == "**" {
		for skip := 0; skip <= len(name); skip++ {
			if referenceMatch(pattern[1:], name[skip:]) {
				return true
			}
		}

		return false
	}

	if len(name) == 0 {
		return false
	}

	segment, compiled := segmentRegexps[pattern[0]]
	if !compiled {
		parts := strings.Split(pattern[0], "*")
		for index := range parts {
			parts[index] = regexp.QuoteMeta(parts[index])
		}

		segment = regexp.MustCompile("^" + strings.Join(parts, "[a-z0-9._-]*") + "$")
		segmentRegexps[pattern[0]] = segment
	}

	return segment.MatchString(name[0]) && referenceMatch(pattern[1:], name[1:])
}

func TestMatchAgreesWithSegmentRule(t *testing.T) {
	segments := []string{"a", "b", "ab", "a.b", "*", "a*", "*a", "a*b", "*.*", "**"}
	random := rand.New(rand.NewPCG(3, 4))
	names := enumerateNames()
	names = append(names, "a.b", "a/a.b", "a.b/b", "a-b/_b")

	for range 2000 {
		parts := make([]string, 1+random.IntN(4))
		for index := range parts {
			parts[index] = segments[random.IntN(len(segments))]
		}

		pattern := strings.Join(parts, "/")

		parsed := mustParse(t, pattern)
		for _, name := range names {
			require.Equal(t, referenceMatch(parts, strings.Split(name, "/")), parsed.Match(name), "pattern %q name %q", pattern, name)
		}
	}
}
