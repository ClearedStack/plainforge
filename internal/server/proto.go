package server

import (
	"bytes"
	"io"
	"strings"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/pkt"
	"github.com/ClearedStack/plainforge/internal/store"
)

// Version is the version in component.json.
const Version = "v0.1.0-draft"

const agent = "agent=plainforge/" + Version

const (
	serviceUpload  = "git-upload-pack"
	serviceReceive = "git-receive-pack"
)

// The capabilities Plainforge offers. A request that asks for anything
// else is refused.
var (
	uploadCaps  = []string{"multi_ack_detailed"}
	receiveCaps = []string{"report-status", "ofs-delta"}
)

// Peeled is the object an annotated tag finally points at.
type Peeled struct {
	Ref string
	OID store.OID
}

// Advertise builds the exact answer to "info/refs" for a service.
// headTarget is the branch HEAD names. peeled lists, for annotated tags,
// the object behind the tag; it is only used for fetching.
func Advertise(service, headTarget string, refs []store.Ref, peeled []Peeled) []byte {
	var out bytes.Buffer
	pkt.WriteString(&out, "# service="+service+"\n")
	pkt.Flush(&out)

	caps := strings.Join(receiveCaps, " ") + " " + agent
	var lines []string
	if service == serviceUpload {
		caps = strings.Join(uploadCaps, " ") + " symref=HEAD:" + headTarget + " " + agent
		for _, r := range refs {
			if r.Name == headTarget {
				lines = append(lines, r.OID.String()+" HEAD")
			}
		}
	}
	behind := map[string]store.OID{}
	for _, p := range peeled {
		behind[p.Ref] = p.OID
	}
	for _, r := range refs {
		lines = append(lines, r.OID.String()+" "+r.Name)
		if p, ok := behind[r.Name]; ok && service == serviceUpload {
			lines = append(lines, p.String()+" "+r.Name+"^{}")
		}
	}
	if len(lines) == 0 {
		lines = []string{store.OID{}.String() + " capabilities^{}"}
	}
	for i, l := range lines {
		if i == 0 {
			l += "\x00" + caps
		}
		pkt.WriteString(&out, l+"\n")
	}
	pkt.Flush(&out)
	return out.Bytes()
}

// checkCaps refuses a capability that was not offered.
func checkCaps(asked, offered []string) error {
	for _, c := range asked {
		ok := strings.HasPrefix(c, "agent=")
		for _, o := range offered {
			ok = ok || c == o
		}
		if !ok {
			return pferr.Needsf("the client asked for a capability Plainforge does not offer")
		}
	}
	return nil
}

func hasCap(caps []string, c string) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// UploadRequest is a parsed fetch request.
type UploadRequest struct {
	Wants []store.OID
	Caps  []string
	Haves []store.OID
	Done  bool
}

// ParseUploadRequest reads the body of a "git-upload-pack" request.
func ParseUploadRequest(r io.Reader) (*UploadRequest, error) {
	pr := pkt.NewReader(r)
	req := &UploadRequest{}
	for first := true; ; first = false {
		line, flush, err := pr.Next()
		if err == io.EOF {
			return nil, pferr.Parsef("the fetch request ended before its list of wanted objects did")
		}
		if err != nil {
			return nil, err
		}
		if flush {
			break
		}
		text := strings.TrimSuffix(string(line), "\n")
		word, rest, _ := strings.Cut(text, " ")
		switch word {
		case "want":
		case "shallow", "deepen", "deepen-since", "deepen-not", "filter":
			return nil, pferr.Needsf("shallow and filtered fetches are not supported")
		default:
			return nil, pferr.Parsef("a line in the fetch request is not a want")
		}
		name, caps, hasCaps := strings.Cut(rest, " ")
		if hasCaps {
			if !first {
				return nil, pferr.Parsef("capabilities may only follow the first want")
			}
			req.Caps = strings.Fields(caps)
		}
		oid, err := store.ParseOID(name)
		if err != nil {
			return nil, err
		}
		req.Wants = append(req.Wants, oid)
	}
	if len(req.Wants) == 0 {
		return nil, pferr.Parsef("the fetch request wants nothing")
	}
	if err := checkCaps(req.Caps, uploadCaps); err != nil {
		return nil, err
	}
	for {
		line, flush, err := pr.Next()
		if err == io.EOF {
			return req, nil
		}
		if err != nil {
			return nil, err
		}
		if flush {
			continue
		}
		if req.Done {
			return nil, pferr.Parsef("the fetch request goes on after it said it was done")
		}
		text := strings.TrimSuffix(string(line), "\n")
		if text == "done" {
			req.Done = true
			continue
		}
		name, ok := strings.CutPrefix(text, "have ")
		if !ok {
			return nil, pferr.Parsef("a line in the fetch request is neither a have nor done")
		}
		oid, err := store.ParseOID(name)
		if err != nil {
			return nil, err
		}
		req.Haves = append(req.Haves, oid)
	}
}

// Command is one ref update asked for by a push.
type Command struct {
	Old, New store.OID
	Ref      string
}

// ParseCommands reads the list of ref updates at the start of a
// "git-receive-pack" request, up to and including its flush packet.
func ParseCommands(pr *pkt.Reader) (cmds []Command, caps []string, err error) {
	for first := true; ; first = false {
		line, flush, err := pr.Next()
		if err == io.EOF {
			if first {
				return nil, nil, nil
			}
			return nil, nil, pferr.Parsef("the push ended inside its list of ref updates")
		}
		if err != nil {
			return nil, nil, err
		}
		if flush {
			break
		}
		text, capText, hasCaps := strings.Cut(string(line), "\x00")
		if hasCaps {
			if !first {
				return nil, nil, pferr.Parsef("capabilities may only follow the first ref update")
			}
			caps = strings.Fields(capText)
		}
		text = strings.TrimSuffix(text, "\n")
		if strings.HasPrefix(text, "shallow ") || strings.HasPrefix(text, "push-cert") {
			return nil, nil, pferr.Needsf("shallow and signed pushes are not supported")
		}
		parts := strings.Split(text, " ")
		if len(parts) != 3 {
			return nil, nil, pferr.Parsef("a ref update is not an old value, a new value and a name")
		}
		var c Command
		if c.Old, err = store.ParseOID(parts[0]); err != nil {
			return nil, nil, err
		}
		if c.New, err = store.ParseOID(parts[1]); err != nil {
			return nil, nil, err
		}
		c.Ref = parts[2]
		cmds = append(cmds, c)
	}
	if err := checkCaps(caps, receiveCaps); err != nil {
		return nil, nil, err
	}
	return cmds, caps, nil
}

// Update is everything the update rule looks at for one ref update.
type Update struct {
	Cmd Command
	// Current is the value the ref has now; zero when there is no such ref.
	Current store.OID
	// NewKind is the kind of the object the ref would point at; zero when
	// that object is not stored.
	NewKind store.Type
	// FastForward says whether the current value is an ancestor of the new
	// one. It is only asked for when a branch that exists is moved.
	FastForward func() (bool, error)
}

// Decide applies the update rules to one ref update. A nil result accepts
// the update. The rules, in order:
//
//  1. Only branches and tags with plain names are accepted.
//  2. The old value sent must be the value the ref has now.
//  3. Refs cannot be deleted.
//  4. An update must change the ref.
//  5. The new object must be stored.
//  6. A tag can be created, never moved, and names a commit or a tag object.
//  7. A branch names a commit and only moves forward.
func Decide(u Update) error {
	c := u.Cmd
	if err := store.CheckRefName(c.Ref); err != nil {
		return err
	}
	if c.Old != u.Current {
		return pferr.Needsf("the ref changed since it was read; fetch and try again")
	}
	if c.New.IsZero() {
		return pferr.Needsf("deleting a ref is not supported")
	}
	if c.New == c.Old {
		return pferr.Parsef("the update does not change the ref")
	}
	if u.NewKind == 0 {
		return pferr.Parsef("the new value names an object that is not stored")
	}
	if strings.HasPrefix(c.Ref, "refs/tags/") {
		if !c.Old.IsZero() {
			return pferr.Needsf("moving an existing tag is not supported")
		}
		if u.NewKind != store.Commit && u.NewKind != store.Tag {
			return pferr.Needsf("a tag that names a tree or a blob is not supported")
		}
		return nil
	}
	if u.NewKind != store.Commit {
		return pferr.Parsef("a branch must name a commit")
	}
	if !c.Old.IsZero() {
		ff, err := u.FastForward()
		if err != nil {
			return err
		}
		if !ff {
			return pferr.Needsf("the update is not a fast-forward")
		}
	}
	return nil
}
