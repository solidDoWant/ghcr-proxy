// Package glob parses repository glob patterns, matches repository names
// against them and decides whether one pattern contains another.
//
// A pattern is one or more segments separated by "/", with no empty segment.
// A segment is either exactly "**", or a non-empty run of [a-z0-9._-] and
// "*" in which no two "*" are adjacent. In a segment, "*" matches any run
// (possibly empty) of [a-z0-9._-]; "**" matches zero or more whole segments.
package glob

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

const (
	separator = "/"
	star      = "*"
	starStar  = "**"
)

// Glob is a parsed repository pattern. The zero value matches nothing.
type Glob struct {
	pattern  string
	segments []string
	// matchPattern is the pattern with runs of "**" segments collapsed to
	// one and any trailing "/**" removed, so that doublestar decides only
	// cases where its answer is the grammar's.
	matchPattern string
	// trailingStarStar records that a trailing "/**" was removed.
	trailingStarStar bool
}

// Parse checks pattern against the glob grammar and returns it parsed.
func Parse(pattern string) (Glob, error) {
	if pattern == "" {
		return Glob{}, errors.New("glob: the pattern is empty")
	}

	segments := strings.Split(pattern, separator)
	for index, segment := range segments {
		if err := validateSegment(segment); err != nil {
			return Glob{}, fmt.Errorf("glob: pattern %q, segment %d: %w", pattern, index+1, err)
		}
	}

	parsed := Glob{pattern: pattern, segments: segments}

	collapsed := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == starStar && len(collapsed) > 0 && collapsed[len(collapsed)-1] == starStar {
			continue
		}

		collapsed = append(collapsed, segment)
	}

	if len(collapsed) > 1 && collapsed[len(collapsed)-1] == starStar {
		collapsed = collapsed[:len(collapsed)-1]
		parsed.trailingStarStar = true
	}

	parsed.matchPattern = strings.Join(collapsed, separator)

	return parsed, nil
}

func validateSegment(segment string) error {
	if segment == "" {
		return errors.New("the segment is empty")
	}

	if segment == starStar {
		return nil
	}

	if strings.Contains(segment, starStar) {
		return errors.New(`"**" must be a whole segment`)
	}

	for _, character := range segment {
		if character != '*' && !isNameCharacter(character) {
			return fmt.Errorf("the character %q is not allowed", character)
		}
	}

	return nil
}

func isNameCharacter(character rune) bool {
	return (character >= 'a' && character <= 'z') ||
		(character >= '0' && character <= '9') ||
		character == '.' || character == '_' || character == '-'
}

// validName reports whether name is made of non-empty segments of the
// pattern alphabet. Matching outside that alphabet is not defined by the
// grammar, so such names never match.
func validName(name string) bool {
	if name == "" {
		return false
	}

	for segment := range strings.SplitSeq(name, separator) {
		if segment == "" {
			return false
		}

		for _, character := range segment {
			if !isNameCharacter(character) {
				return false
			}
		}
	}

	return true
}

// String returns the pattern as written.
func (g Glob) String() string {
	return g.pattern
}

// Match reports whether the repository name matches the pattern.
func (g Glob) Match(name string) bool {
	if len(g.segments) == 0 || !validName(name) {
		return false
	}
	// A trailing "/**" matches zero or more segments. doublestar misses
	// some of the zero-segment matches, so the pattern without it is tried
	// on the name itself and with "**" on the name's prefixes.
	if matchDoublestar(g.matchPattern, name) {
		return true
	}

	return g.trailingStarStar && matchDoublestar(g.matchPattern+separator+starStar, name)
}

func matchDoublestar(pattern, name string) bool {
	matched, err := doublestar.Match(pattern, name)
	return err == nil && matched
}

// Contains reports whether every name matching inner also matches outer. It
// is conservative: when containment can't be proved from the segments it
// returns false.
func Contains(outer, inner Glob) bool {
	if len(outer.segments) == 0 || len(inner.segments) == 0 {
		return false
	}

	return containsSegments(outer.segments, inner.segments)
}

func containsSegments(outer, inner []string) bool {
	if len(outer) == 0 {
		return len(inner) == 0
	}

	if outer[0] == starStar {
		// The outer "**" absorbs zero or more inner segments; the rest of
		// outer must then contain the rest of inner.
		for skip := 0; skip <= len(inner); skip++ {
			if containsSegments(outer[1:], inner[skip:]) {
				return true
			}
		}

		return false
	}

	if len(inner) == 0 {
		return false
	}
	// Only an outer "**" can absorb an inner "**".
	if inner[0] == starStar {
		return false
	}

	return segmentContains(outer[0], inner[0]) && containsSegments(outer[1:], inner[1:])
}

// segmentContains reports whether every segment matching inner matches
// outer. Neither is "**".
func segmentContains(outer, inner string) bool {
	if !strings.Contains(inner, star) {
		matched, err := doublestar.Match(outer, inner)
		return err == nil && matched
	}

	if outer == star {
		return true
	}
	// Proving containment between two wildcard segments in general needs
	// more than this; equal segments are the only other case accepted.
	return outer == inner
}
