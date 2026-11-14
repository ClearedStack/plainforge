package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ClearedStack/plainforge/internal/pkt"
	"github.com/ClearedStack/plainforge/internal/store"
)

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func oid(t *testing.T, s string) store.OID {
	t.Helper()
	o, err := store.ParseOID(s)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return o
}

// pktLines turns fixture lines into the bytes a client would send.
func pktLines(lines []string) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		if l == "FLUSH" {
			pkt.Flush(&b)
		} else {
			pkt.WriteString(&b, l)
		}
	}
	return b.Bytes()
}

func TestUpdateRuleFixtures(t *testing.T) {
	var cases []struct {
		Name, Ref, Old, New, Current, Error string
		NewKind                             string `json:"new_kind"`
		FastForward                         bool   `json:"fast_forward"`
	}
	loadJSON(t, "update_rules.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			kind, _ := store.ParseType(c.NewKind) // "missing" gives zero
			err := Decide(Update{
				Cmd:         Command{Old: oid(t, c.Old), New: oid(t, c.New), Ref: c.Ref},
				Current:     oid(t, c.Current),
				NewKind:     kind,
				FastForward: func() (bool, error) { return c.FastForward, nil },
			})
			if errText(err) != c.Error {
				t.Fatalf("got %q, want %q", errText(err), c.Error)
			}
		})
	}
}

func TestRequestFixtures(t *testing.T) {
	var cases []struct {
		Name, Kind, Parsed, Error string
		Lines                     []string
	}
	loadJSON(t, "requests.json", &cases)
	for _, c := range cases {
		t.Run(c.Kind+"/"+c.Name, func(t *testing.T) {
			in := bytes.NewReader(pktLines(c.Lines))
			var got string
			var err error
			switch c.Kind {
			case "fetch":
				var req *UploadRequest
				if req, err = ParseUploadRequest(in); err == nil {
					got = fmt.Sprintf("wants=%v caps=%v haves=%v done=%v", req.Wants, req.Caps, req.Haves, req.Done)
				}
			case "push":
				var cmds []Command
				var caps []string
				if cmds, caps, err = ParseCommands(pkt.NewReader(in)); err == nil {
					parts := []string{}
					for _, cmd := range cmds {
						parts = append(parts, cmd.Old.String()+" "+cmd.New.String()+" "+cmd.Ref)
					}
					got = strings.TrimSpace(fmt.Sprintf("caps=%v %s", caps, strings.Join(parts, " | ")))
				}
			default:
				t.Fatalf("unknown kind %q", c.Kind)
			}
			if errText(err) != c.Error || got != c.Parsed {
				t.Fatalf("got %q, error %q; want %q, error %q", got, errText(err), c.Parsed, c.Error)
			}
		})
	}
}
