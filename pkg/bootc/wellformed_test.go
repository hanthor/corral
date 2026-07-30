package bootc

import "encoding/xml"

// wellFormed reports whether a generated document parses as XML at all —
// libvirt rejects malformed input outright, and a stray unescaped character in
// a name or a path is the usual cause.
func wellFormed(doc string) error {
	var v any
	return xml.Unmarshal([]byte(doc), &v)
}
