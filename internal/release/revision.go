package release

import (
	"encoding"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cplieger/seadex-scout/internal/nametoken"
)

// RevisionMarker names what a revision reading rests on.
type RevisionMarker uint8

const (
	// RevisionUnknown means no revision evidence was read at all. It is the zero
	// value, so a reading nobody took never compares as an original release.
	RevisionUnknown RevisionMarker = iota
	// RevisionNone means evidence was read and carries no revision token: an
	// original release, Version 1 in the arrs' numbering.
	RevisionNone
	// RevisionVersion means an explicit vN token (v0 to v9) and nothing else.
	RevisionVersion
	// RevisionRepack means a REPACK, REPACKn, RERIP or RERIPn word, with or
	// without a vN token.
	RevisionRepack
	// RevisionProper means a PROPER word, with or without a vN token, and no
	// repack word.
	RevisionProper
)

// revisionMarkerNames is indexed by RevisionMarker and is the persisted text of
// each marker.
var revisionMarkerNames = [...]string{
	RevisionUnknown: "unknown",
	RevisionNone:    "none",
	RevisionVersion: "version",
	RevisionRepack:  "repack",
	RevisionProper:  "proper",
}

var (
	_ encoding.TextMarshaler   = RevisionMarker(0)
	_ encoding.TextUnmarshaler = (*RevisionMarker)(nil)
	_ fmt.Stringer             = Revision{}
)

// MarshalText renders the marker under its persisted name. It fails only for a
// value outside the declared constants.
func (m RevisionMarker) MarshalText() ([]byte, error) {
	if int(m) >= len(revisionMarkerNames) {
		return nil, fmt.Errorf("release: revision marker %d is not a declared marker", m)
	}
	return []byte(revisionMarkerNames[m]), nil
}

// UnmarshalText decodes a persisted marker name. An unrecognized name decodes
// to RevisionUnknown with a nil error: a decode error in a persisted snapshot
// quarantines the whole file, and an unknown reading never claims anything.
func (m *RevisionMarker) UnmarshalText(text []byte) error {
	*m = RevisionUnknown
	for marker, name := range revisionMarkerNames {
		if string(text) == name {
			*m = RevisionMarker(marker)
			break
		}
	}
	return nil
}

// Revision is one release-revision reading in the arrs' numbering (Sonarr and
// Radarr's Revision.Version): an original release is Version 1, and a vN token,
// a PROPER or a REPACK raises it. The zero value is an unknown reading.
type Revision struct {
	Version int            `json:"version,omitempty"`
	Marker  RevisionMarker `json:"marker,omitempty"`
}

// Known reports whether the reading rests on any evidence at all, including
// evidence of an original release.
func (r Revision) Known() bool { return r.Marker != RevisionUnknown }

// Explicit reports whether the reading rests on a revision token rather than
// on the absence of one.
func (r Revision) Explicit() bool {
	switch r.Marker {
	case RevisionVersion, RevisionRepack, RevisionProper:
		return true
	case RevisionUnknown, RevisionNone:
		return false
	default:
		return false
	}
}

// String renders the reading for logs and the report: "" for unknown, "v1" for
// an original release, "v2" for a version token, and the word in parentheses
// for a repack or proper ("v3 (repack)").
func (r Revision) String() string {
	version := "v" + strconv.Itoa(r.Version)
	switch r.Marker {
	case RevisionNone, RevisionVersion:
		return version
	case RevisionRepack:
		return version + " (repack)"
	case RevisionProper:
		return version + " (proper)"
	case RevisionUnknown:
		return ""
	default:
		return ""
	}
}

// maxBracketVersion is the highest version the arrs read from a "[vN]" token,
// whose grammar takes a single digit.
const maxBracketVersion = 9

// Token renders an explicit reading as one release-title token the arrs read
// back at the same Version: "REPACK" or "REPACKn" for a repack, "PROPER" for a
// proper at 2, and "[vN]" otherwise. It is "" for a reading that is not
// explicit, or whose Version no single token spells.
func (r Revision) Token() string {
	if !r.Explicit() {
		return ""
	}
	switch {
	case r.Marker == RevisionRepack && r.Version == 2:
		return "REPACK"
	case r.Marker == RevisionRepack && r.Version > 2 && r.Version <= maxBracketVersion+1:
		return "REPACK" + strconv.Itoa(r.Version-1)
	case r.Marker == RevisionProper && r.Version == 2:
		return "PROPER"
	case r.Version >= 0 && r.Version <= maxBracketVersion:
		return "[v" + strconv.Itoa(r.Version) + "]"
	default:
		return ""
	}
}

// The revision grammar is Sonarr and Radarr's QualityParser.ParseQualityModifiers
// (VersionRegex, ProperRegex, RepackRegex), restricted to the arms both arrs
// share: Sonarr's extra "1080p v2" arm is absent from Radarr. Reading a shape
// the arrs do not would make a library file that came from the very torrent
// SeaDex lists look older than it.
// https://github.com/Sonarr/Sonarr/blob/develop/src/NzbDrone.Core/Parser/QualityParser.cs
const (
	// arrWordEdge is one rune that is not a .NET regex word character, which is
	// what the arrs' \b asserts against (Unicode letters, digits, nonspacing
	// marks, connector punctuation, and ZWNJ/ZWJ).
	arrWordEdge = `[^\p{L}\p{Mn}\p{Nd}\p{Pc}\x{200C}\x{200D}]`
	revDelim    = `[-._ ]`
)

var (
	// arrRerip spells "rerip" without U+0130: .NET folds it onto "i" only under
	// a Turkic culture, where nametoken.Literal follows strings.ToLower.
	arrRerip  = nametoken.Literal("rer") + `[iI]` + nametoken.Literal("p")
	arrRepack = nametoken.Literal("repack")
	arrV      = nametoken.Literal("v")
	reVersion = regexp.MustCompile(`\p{Nd}` + revDelim + `?` + arrV + `([0-9])` + revDelim + `|\[` + arrV + `([0-9])\]|` + arrRepack + `([0-9])|` + arrRerip + `([0-9])`)
	reProper  = regexp.MustCompile(`(?:^|` + arrWordEdge + `)` + nametoken.Literal("proper") + `(?:$|` + arrWordEdge + `)`)
	reRepack  = regexp.MustCompile(`(?:^|` + arrWordEdge + `)(?:` + arrRepack + `|` + arrRerip + `)\p{Nd}?(?:$|` + arrWordEdge + `)`)
)

// ParseRevision reads the revision of one release or file name exactly as the
// arrs do: the first version token sets the version, a PROPER word then sets
// it to one more (or 2), and a repack word likewise and marks it a repack, so
// "REPACK2" is Version 3. Only the base name after the last '/' or '\' is read.
// A blank base name is an unknown reading; any other name with no token is an
// original release, Version 1.
func ParseRevision(name string) Revision {
	return ParseTitleRevision(name[strings.LastIndexAny(name, `/\`)+1:])
}

// ParseTitleRevision is ParseRevision over a whole release title: the arrs read
// a title as one string, so a '/' inside it ("Fate/Zero") is not a path
// separator there.
func ParseTitleRevision(title string) Revision {
	normalized := strings.TrimSpace(strings.ReplaceAll(title, "_", " "))
	if normalized == "" {
		return Revision{}
	}
	rev := Revision{Version: 1, Marker: RevisionNone}
	tokenVersion, hasToken := versionToken(normalized)
	bumped := 2
	if hasToken {
		rev = Revision{Version: tokenVersion, Marker: RevisionVersion}
		bumped = tokenVersion + 1
	}
	if reProper.MatchString(normalized) {
		rev = Revision{Version: bumped, Marker: RevisionProper}
	}
	if reRepack.MatchString(normalized) {
		rev = Revision{Version: bumped, Marker: RevisionRepack}
	}
	return rev
}

// versionToken returns the digit of the leftmost version token, from whichever
// alternation arm matched.
func versionToken(name string) (int, bool) {
	match := reVersion.FindStringSubmatch(name)
	if match == nil {
		return 0, false
	}
	for _, digit := range match[1:] {
		if digit != "" {
			return int(digit[0] - '0'), true
		}
	}
	return 0, false
}

// ArrRevision converts the revision an arr recorded for a file (Sonarr and
// Radarr's Revision.Version and IsRepack) into a reading. A negative version is
// not a revision the arrs produce and reads unknown.
func ArrRevision(version int, isRepack bool) Revision {
	switch {
	case version < 0:
		return Revision{}
	case isRepack:
		return Revision{Version: version, Marker: RevisionRepack}
	case version == 1:
		return Revision{Version: 1, Marker: RevisionNone}
	default:
		return Revision{Version: version, Marker: RevisionVersion}
	}
}

// ExplicitRevision returns the newest explicit reading among names, or an
// unknown reading when none carries a token. It is the fallback for a library
// file whose arr recorded no revision: a renamed file loses its token, so a
// name without one is missing evidence there, not proof of an original.
func ExplicitRevision(names ...string) Revision {
	var newest Revision
	for _, name := range names {
		if rev := ParseRevision(name); rev.Explicit() && (!newest.Known() || newer(rev, newest)) {
			newest = rev
		}
	}
	return newest
}

// NewestRevision returns the highest Version among revs, ties going to the
// higher marker. It is unknown when revs is empty or any reading is unknown,
// because the unread one could be newer.
func NewestRevision(revs ...Revision) Revision {
	if len(revs) == 0 {
		return Revision{}
	}
	newest := revs[0]
	for _, rev := range revs {
		if !rev.Known() {
			return Revision{}
		}
		if newer(rev, newest) {
			newest = rev
		}
	}
	return newest
}

// RevisionBehind reports whether a held reading is provably older than a listed
// one. It is the only way a revision can change a verdict: missing evidence on
// either side, or a listing that carries no token, never claims.
func RevisionBehind(held, listed Revision) bool {
	return held.Known() && listed.Explicit() && held.Version < listed.Version
}

// newer orders two readings by Version, then by marker.
func newer(a, b Revision) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	return a.Marker > b.Marker
}
