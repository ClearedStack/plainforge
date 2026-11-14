// Package store keeps one bare Git repository on disk in the standard
// layout: loose objects, one file per ref and a HEAD file. Ordinary Git
// tools can check the result (for example with "git fsck").
package store

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

// OID is a SHA-1 object name.
type OID [20]byte

// IsZero reports whether the name is all zeroes, which Git uses for "none".
func (o OID) IsZero() bool { return o == OID{} }

func (o OID) String() string { return hex.EncodeToString(o[:]) }

// ParseOID reads 40 lower-case hexadecimal digits.
func ParseOID(s string) (OID, error) {
	var o OID
	if len(s) != 40 || strings.ToLower(s) != s {
		return o, pferr.Parsef("an object name was not 40 lower-case hexadecimal digits")
	}
	if _, err := hex.Decode(o[:], []byte(s)); err != nil {
		return o, pferr.Parsef("an object name was not 40 lower-case hexadecimal digits")
	}
	return o, nil
}

// Type is the kind of a Git object. The numbers are the ones packs use.
type Type uint8

const (
	Commit Type = 1
	Tree   Type = 2
	Blob   Type = 3
	Tag    Type = 4
)

func (t Type) String() string {
	switch t {
	case Commit:
		return "commit"
	case Tree:
		return "tree"
	case Blob:
		return "blob"
	case Tag:
		return "tag"
	}
	return "unknown"
}

// ParseType reads an object kind by its Git name.
func ParseType(s string) (Type, bool) {
	switch s {
	case "commit":
		return Commit, true
	case "tree":
		return Tree, true
	case "blob":
		return Blob, true
	case "tag":
		return Tag, true
	}
	return 0, false
}

// MaxObjectSize is the largest object Plainforge accepts or serves. Objects
// are handled in memory one at a time, and the whole service has to fit in
// 64 MiB, so a larger object is refused instead of risking the budget.
const MaxObjectSize = 8 << 20

func header(t Type, size int) []byte {
	return []byte(t.String() + " " + strconv.Itoa(size) + "\x00")
}

// Hash returns the object name of an object.
func Hash(t Type, data []byte) OID {
	h := sha1.New()
	h.Write(header(t, len(data)))
	h.Write(data)
	var o OID
	h.Sum(o[:0])
	return o
}

// Link is a reference from one object to another, with the kind the target
// has to be.
type Link struct {
	OID  OID
	Type Type
}

// CommitInfo is the part of a commit Plainforge reads.
type CommitInfo struct {
	Tree    OID
	Parents []OID
	Message []byte
}

// ParseCommit reads the tree, the parents and the message of a commit.
func ParseCommit(data []byte) (CommitInfo, error) {
	var c CommitInfo
	head, msg, found := bytes.Cut(data, []byte("\n\n"))
	if !found {
		head = bytes.TrimSuffix(data, []byte("\n"))
		msg = nil
	}
	c.Message = msg
	lines := strings.Split(string(head), "\n")
	rest, ok := strings.CutPrefix(lines[0], "tree ")
	if !ok {
		return c, pferr.Parsef("a commit did not start with its tree")
	}
	tree, err := ParseOID(rest)
	if err != nil {
		return c, pferr.Parsef("a commit named its tree badly")
	}
	c.Tree = tree
	for _, line := range lines[1:] {
		rest, ok := strings.CutPrefix(line, "parent ")
		if !ok {
			break
		}
		p, err := ParseOID(rest)
		if err != nil {
			return c, pferr.Parsef("a commit named a parent badly")
		}
		c.Parents = append(c.Parents, p)
	}
	return c, nil
}

// ParseTag reads what an annotated tag points at.
func ParseTag(data []byte) (Link, error) {
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 3 {
		return Link{}, pferr.Parsef("a tag object was too short")
	}
	obj, ok1 := strings.CutPrefix(lines[0], "object ")
	typ, ok2 := strings.CutPrefix(lines[1], "type ")
	if !ok1 || !ok2 {
		return Link{}, pferr.Parsef("a tag object did not start with its object and type")
	}
	oid, err := ParseOID(obj)
	if err != nil {
		return Link{}, pferr.Parsef("a tag object named its target badly")
	}
	t, ok := ParseType(typ)
	if !ok {
		return Link{}, pferr.Parsef("a tag object named an unknown kind of target")
	}
	return Link{OID: oid, Type: t}, nil
}

// parseTree reads the entries of a tree. Submodule entries point outside
// the repository and are not returned.
func parseTree(data []byte) ([]Link, error) {
	var links []Link
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 1 {
			return nil, pferr.Parsef("a tree entry had no mode")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			return nil, pferr.Parsef("a tree entry was cut short")
		}
		name := string(data[:nul])
		if name == "" || name == "." || name == ".." || strings.EqualFold(name, ".git") || strings.Contains(name, "/") {
			return nil, pferr.Parsef("a tree entry had a name that is not allowed")
		}
		var oid OID
		copy(oid[:], data[nul+1:nul+21])
		data = data[nul+21:]
		switch mode {
		case "40000":
			links = append(links, Link{OID: oid, Type: Tree})
		case "100644", "100755", "120000":
			links = append(links, Link{OID: oid, Type: Blob})
		case "160000":
			// A submodule: the commit lives in another repository.
		default:
			return nil, pferr.Parsef("a tree entry had an unknown mode")
		}
	}
	return links, nil
}

// Links returns every object that the given object refers to.
func Links(t Type, data []byte) ([]Link, error) {
	switch t {
	case Blob:
		return nil, nil
	case Tree:
		return parseTree(data)
	case Commit:
		c, err := ParseCommit(data)
		if err != nil {
			return nil, err
		}
		links := []Link{{OID: c.Tree, Type: Tree}}
		for _, p := range c.Parents {
			links = append(links, Link{OID: p, Type: Commit})
		}
		return links, nil
	case Tag:
		l, err := ParseTag(data)
		if err != nil {
			return nil, err
		}
		return []Link{l}, nil
	}
	return nil, pferr.Parsef("an object had an unknown kind")
}
