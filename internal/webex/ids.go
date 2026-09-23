package webex

import (
	"encoding/base64"
	"strings"
)

// REST API ids are base64-encoded URIs of the form
// "ciscospark://<cluster>/<TYPE>/<uuid>". The websocket speaks in bare
// uuids, so the two have to be translated.

// ID is a decoded REST id.
type ID struct {
	Cluster string
	Type    string
	UUID    string
}

// DecodeID parses a REST id. ok is false if it is not in the expected form.
func DecodeID(id string) (ID, bool) {
	trimmed := strings.TrimRight(id, "=")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		raw, err := enc.DecodeString(trimmed)
		if err != nil {
			continue
		}
		rest, found := strings.CutPrefix(string(raw), "ciscospark://")
		if !found {
			continue
		}
		slash := strings.LastIndex(rest, "/")
		if slash < 0 {
			continue
		}
		uuid := rest[slash+1:]
		prefix := rest[:slash]
		typeSlash := strings.LastIndex(prefix, "/")
		if typeSlash < 0 {
			continue
		}
		return ID{Cluster: prefix[:typeSlash], Type: prefix[typeSlash+1:], UUID: uuid}, true
	}
	return ID{}, false
}

// EncodeID builds a REST id.
func EncodeID(cluster, typ, uuid string) string {
	return base64.RawStdEncoding.EncodeToString([]byte("ciscospark://" + cluster + "/" + typ + "/" + uuid))
}

// UUIDOf returns the uuid inside a REST id, or the input if it is already a
// bare uuid.
func UUIDOf(id string) string {
	if decoded, ok := DecodeID(id); ok {
		return decoded.UUID
	}
	return id
}
