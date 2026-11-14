package store

import (
	"bytes"
	"crypto/sha1"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

// FsckReport summarizes a repository consistency check.
type FsckReport struct {
	Refs    int
	Objects int
}

// Fsck performs a full pure-Go repository consistency check equivalent to
// "git fsck --strict" across HEAD, refs, packfiles, loose objects, canonical
// tree ordering, and object graph reachability.
func (r *Repo) Fsck() (FsckReport, error) {
	var rep FsckReport
	if err := r.check(); err != nil {
		return rep, err
	}

	types := make(map[OID]Type)
	linksByOID := make(map[OID][]Link)
	recordObject := func(oid OID, t Type, data []byte) error {
		links, err := checkStrictObject(t, data)
		if err != nil {
			return err
		}
		if prev, seen := types[oid]; seen && prev != t {
			return pferr.Parsef("two stored copies of an object disagree on its kind")
		}
		types[oid] = t
		linksByOID[oid] = links
		return nil
	}

	// Verify every pack stream SHA-1 and every indexed packed object.
	for _, pf := range r.packs {
		if err := verifyPackStreamChecksum(pf); err != nil {
			return rep, err
		}
		for _, oid := range pf.oids {
			t, data, err := r.readFromPacks(oid, 0)
			if err != nil {
				return rep, err
			}
			if err := recordObject(oid, t, data); err != nil {
				return rep, err
			}
		}
	}

	// Verify every loose object file under objects/00..ff/*.
	objEntries, err := os.ReadDir(r.objs.dir)
	if err != nil {
		return rep, storageErr("reading objects directory", err)
	}
	for _, fan := range objEntries {
		fname := fan.Name()
		if fname == "pack" || fname == "info" {
			continue
		}
		if !fan.IsDir() || !isLowerHex(fname, 2) {
			return rep, pferr.Parsef("the objects directory holds an unexpected entry")
		}
		files, err := os.ReadDir(filepath.Join(r.objs.dir, fname))
		if err != nil {
			return rep, storageErr("reading object fanout directory", err)
		}
		for _, f := range files {
			if f.IsDir() || !isLowerHex(f.Name(), 38) {
				return rep, pferr.Parsef("an object fanout directory holds an unexpected entry")
			}
			oid, err := ParseOID(fname + f.Name())
			if err != nil {
				return rep, err
			}
			t, data, err := r.objs.read(oid)
			if err != nil {
				return rep, err
			}
			if err := recordObject(oid, t, data); err != nil {
				return rep, err
			}
		}
	}

	// Verify every link of every stored object points to an existing object of
	// the expected kind.
	for _, links := range linksByOID {
		for _, l := range links {
			got, ok := types[l.OID]
			if !ok {
				return rep, pferr.Parsef("a stored object refers to a missing object")
			}
			if got != l.Type {
				return rep, pferr.Parsef("a stored object refers to an object of the wrong kind")
			}
		}
	}

	// Verify every ref (loose and packed) points to a valid target and walks
	// cleanly through the checked object graph.
	refs, err := r.Refs()
	if err != nil {
		return rep, err
	}
	for _, ref := range refs {
		got, ok := types[ref.OID]
		if !ok {
			return rep, pferr.Parsef("a ref points to an object that is not stored")
		}
		switch {
		case strings.HasPrefix(ref.Name, "refs/heads/"):
			if got != Commit {
				return rep, pferr.Parsef("a branch does not point to a commit")
			}
		case strings.HasPrefix(ref.Name, "refs/tags/"):
			if got != Commit && got != Tag {
				return rep, pferr.Parsef("a tag does not point to a commit or tag object")
			}
			cur := ref.OID
			for depth := 0; types[cur] == Tag; depth++ {
				if depth >= 16 {
					return rep, pferr.Needsf("a tag chain is deeper than Plainforge follows")
				}
				tagLinks := linksByOID[cur]
				if len(tagLinks) != 1 {
					return rep, pferr.Parsef("a tag object is malformed")
				}
				cur = tagLinks[0].OID
			}
		}
	}

	rep.Refs = len(refs)
	rep.Objects = len(types)
	return rep, nil
}

func checkStrictObject(t Type, data []byte) ([]Link, error) {
	links, err := Links(t, data)
	if err != nil {
		return nil, err
	}
	if t == Tree {
		entries, err := ParseTree(data)
		if err != nil {
			return nil, err
		}
		canonical, err := FormatTree(entries)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(canonical, data) {
			return nil, pferr.Parsef("a tree object is not in canonical sort order")
		}
	}
	return links, nil
}

func verifyPackStreamChecksum(pf *packFile) error {
	f, err := os.Open(pf.packPath)
	if err != nil {
		return storageErr("opening pack file", err)
	}
	defer f.Close()
	if pf.packSize < 32 {
		return pferr.Parsef("a pack file in the repository is too short")
	}
	h := sha1.New()
	if _, err := io.CopyN(h, f, pf.packSize-20); err != nil {
		return pferr.Parsef("a pack file stream ended unexpectedly")
	}
	var trailer [20]byte
	if _, err := io.ReadFull(f, trailer[:]); err != nil {
		return pferr.Parsef("a pack trailer could not be read")
	}
	if !bytes.Equal(h.Sum(nil), trailer[:]) {
		return pferr.Parsef("a pack file stream checksum does not match its trailer")
	}
	return nil
}

func isLowerHex(s string, wantLen int) bool {
	if len(s) != wantLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
