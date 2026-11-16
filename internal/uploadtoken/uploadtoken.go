// Package uploadtoken turns an upstream's upload session location into an opaque, signed token for the client,
// and a token back into the upstream location. The token binds the session to the upstream and the repository it
// was issued for, so a client can't steer an upload session request to another repository or another path on
// the upstream.
package uploadtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
)

// MinKeyLength is the shortest signing key accepted, in bytes.
const MinKeyLength = 32

const (
	maxPayloadLength = 2800
	macLength        = 43
	// MaxLength is the longest token the grammar accepts.
	MaxLength = maxPayloadLength + 1 + macLength
)

// encoding is unpadded base64url; Strict refuses non-zero trailing bits, so each value has one encoding.
var encoding = base64.RawURLEncoding.Strict()

var (
	// ErrUnknown is a token that is malformed, wasn't issued with this key for this upstream and repository, or
	// decodes to an unusable location. The client gets 404 BLOB_UPLOAD_UNKNOWN and nothing is sent upstream.
	ErrUnknown = errors.New("blob upload unknown")
	// ErrUnusableLocation is an upstream upload location that can't be given to the client: on another scheme or
	// host, or outside the repository's blobs. The client gets 502.
	ErrUnusableLocation = errors.New("upstream returned an unusable upload location")
	// ErrQuery is a client query on an upload session request other than one digest on PUT that the upstream's
	// location doesn't already carry. The client gets 400.
	ErrQuery = errors.New("query parameter not allowed on an upload session request")
)

// Codec issues and opens tokens with one signing key.
type Codec struct {
	key []byte
}

// New returns a codec signing with key.
func New(key []byte) (*Codec, error) {
	if len(key) < MinKeyLength {
		return nil, errors.New("upload signing key is shorter than 32 bytes")
	}

	return &Codec{key: append([]byte(nil), key...)}, nil
}

// Valid reports whether token has the token grammar, [A-Za-z0-9_-]{1,2800}\.[A-Za-z0-9_-]{43}: an unpadded
// base64url payload and an unpadded base64url HMAC-SHA-256. It doesn't check the signature. (Go's regexp caps
// repeat counts at 1000, so the grammar is checked by hand.)
func Valid(token string) bool {
	payload, mac, found := strings.Cut(token, ".")

	return found && len(payload) >= 1 && len(payload) <= maxPayloadLength && len(mac) == macLength &&
		base64URLAlphabet(payload) && base64URLAlphabet(mac)
}

func base64URLAlphabet(value string) bool {
	for _, char := range []byte(value) {
		switch {
		case 'A' <= char && char <= 'Z', 'a' <= char && char <= 'z', '0' <= char && char <= '9', char == '_', char == '-':
		default:
			return false
		}
	}

	return true
}

// Issue returns the client's location for an upstream upload location. location is resolved against upstream,
// the URL of the upstream request it answered; it must be on that URL's scheme and host, and its path must lie
// under /v2/<name>/blobs/.
func (c *Codec) Issue(upstreamUID, name string, upstream *url.URL, location string) (string, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return "", ErrUnusableLocation
	}

	resolved := upstream.ResolveReference(parsed)
	if !strings.EqualFold(resolved.Scheme, upstream.Scheme) || !strings.EqualFold(resolved.Host, upstream.Host) ||
		resolved.User != nil {
		return "", ErrUnusableLocation
	}

	path, found := strings.CutPrefix(resolved.EscapedPath(), blobsPrefix(name))
	if !found || !canonicalSuffixPath(path) {
		return "", ErrUnusableLocation
	}

	suffix := path
	if resolved.RawQuery != "" {
		suffix += "?" + resolved.RawQuery
	}

	payload := encoding.EncodeToString([]byte(suffix))
	if len(payload) > maxPayloadLength || strings.Contains(suffix, "#") {
		return "", ErrUnusableLocation
	}

	return blobsPrefix(name) + "uploads/" + payload + "." + c.sign(upstreamUID, name, suffix), nil
}

// Open checks token for the upstream and repository of the request it came on, and returns the upstream path and
// query it stands for.
func (c *Codec) Open(upstreamUID, name, token string) (*url.URL, error) {
	if !Valid(token) {
		return nil, ErrUnknown
	}

	payload, mac, _ := strings.Cut(token, ".")

	decoded, err := encoding.DecodeString(payload)
	if err != nil {
		return nil, ErrUnknown
	}

	suffix := string(decoded)

	want, err := encoding.DecodeString(c.sign(upstreamUID, name, suffix))
	if err != nil {
		return nil, ErrUnknown
	}

	got, err := encoding.DecodeString(mac)
	if err != nil || !hmac.Equal(got, want) {
		return nil, ErrUnknown
	}

	// The signature already binds the suffix to what the upstream sent; these checks are a second layer.
	path, _, _ := strings.Cut(suffix, "?")
	if strings.Contains(suffix, "#") || !canonicalSuffixPath(path) {
		return nil, ErrUnknown
	}

	upstreamURL, err := url.Parse(blobsPrefix(name) + suffix)
	if err != nil || upstreamURL.IsAbs() || upstreamURL.Host != "" {
		return nil, ErrUnknown
	}

	return upstreamURL, nil
}

// AddClientQuery adds the client's query to an opened upstream URL. The client may send only one digest
// parameter, only on PUT, and only when the upstream's location doesn't already carry one.
func AddClientQuery(upstreamURL *url.URL, method, rawQuery string) error {
	if rawQuery == "" {
		return nil
	}

	values, err := url.ParseQuery(rawQuery)
	if err != nil || method != "PUT" || len(values) != 1 || len(values["digest"]) != 1 {
		return ErrQuery
	}

	upstreamValues, err := url.ParseQuery(upstreamURL.RawQuery)
	if err != nil {
		return ErrQuery
	}

	if _, present := upstreamValues["digest"]; present {
		return ErrQuery
	}

	added := url.Values{"digest": values["digest"]}.Encode()
	if upstreamURL.RawQuery == "" {
		upstreamURL.RawQuery = added
	} else {
		// The upstream's own query goes back byte for byte; only the digest is appended.
		upstreamURL.RawQuery += "&" + added
	}

	return nil
}

func (c *Codec) sign(upstreamUID, name, suffix string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(upstreamUID + "\n" + name + "\n" + suffix))

	return encoding.EncodeToString(mac.Sum(nil))
}

func blobsPrefix(name string) string {
	return "/v2/" + name + "/blobs/"
}

// canonicalSuffixPath reports whether the path after /v2/<name>/blobs/ has no empty, "." or ".." segment and
// no percent-encoding (which an upstream could decode into one), so the upstream URL stays inside the
// repository's blobs whatever the upstream does with dot segments.
func canonicalSuffixPath(path string) bool {
	if strings.Contains(path, "%") {
		return false
	}

	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}

	return true
}
