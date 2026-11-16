// Package secrets keeps the Kubernetes Secrets that registrygate's objects
// reference. Secrets in the admin namespaces come from one namespace-scoped
// informer each. Every other referenced Secret gets its own watch, narrowed
// to that one name, so a tenant only has to grant the proxy a Role with
// resourceNames for that Secret.
package secrets

import (
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// Key names a Secret.
type Key struct {
	Namespace string
	Name      string
}

// String returns namespace/name.
func (k Key) String() string { return k.Namespace + "/" + k.Name }

// State says what the store knows about a Secret.
type State int

const (
	// Pending means the watch started but its first list has not returned.
	Pending State = iota
	// Present means the Secret exists and its data is known.
	Present
	// NotFound means the Secret does not exist.
	NotFound
	// Forbidden means the API server refused to let the proxy read the
	// Secret.
	Forbidden
)

// String names the state.
func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Present:
		return "present"
	case NotFound:
		return "notFound"
	case Forbidden:
		return "forbidden"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Data holds a Secret's values. It hides them from fmt and slog so they
// can't reach a log line, an error or a status message by accident.
type Data map[string][]byte

// String hides the values.
func (Data) String() string { return redacted }

// GoString hides the values from %#v.
func (Data) GoString() string { return redacted }

// LogValue hides the values from slog.
func (Data) LogValue() slog.Value { return slog.StringValue(redacted) }

// Format hides the values from every fmt verb, including %x and %q, which
// would otherwise print the map's bytes.
func (Data) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(redacted)) }

// Secret is the store's view of one Secret. Data and ResourceVersion are set
// only when State is Present.
type Secret struct {
	State           State
	Data            Data
	ResourceVersion string
}

// String shows the state and resource version, never the data.
func (s Secret) String() string {
	return fmt.Sprintf("{State:%s ResourceVersion:%s Data:%s}", s.State, s.ResourceVersion, redacted)
}

// GoString hides the data from %#v.
func (s Secret) GoString() string { return s.String() }

// LogValue hides the data from slog.
func (s Secret) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("state", s.State.String()),
		slog.String("resourceVersion", s.ResourceVersion),
	)
}

// Format hides the data from every fmt verb.
func (s Secret) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(s.String())) }
