package pack

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ClearedStack/plainforge/internal/store"
)

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadJSON(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(readFile(t, name), v); err != nil {
		t.Fatal(err)
	}
}

// memSink keeps objects in memory.
type memSink struct {
	kinds map[store.OID]store.Type
	data  map[store.OID][]byte
	order []store.OID
}

func newSink() *memSink {
	return &memSink{kinds: map[store.OID]store.Type{}, data: map[store.OID][]byte{}}
}

func (m *memSink) Write(t store.Type, data []byte) (store.OID, error) {
	oid := store.Hash(t, data)
	if _, ok := m.kinds[oid]; !ok {
		m.kinds[oid], m.data[oid] = t, data
		m.order = append(m.order, oid)
	}
	return oid, nil
}

func (m *memSink) Read(o store.OID) (store.Type, []byte, bool, error) {
	t, ok := m.kinds[o]
	return t, m.data[o], ok, nil
}

func (m *memSink) get(o store.OID) (store.Type, []byte, error) { return m.kinds[o], m.data[o], nil }

func (m *memSink) listing() string {
	var lines []string
	for oid, k := range m.kinds {
		lines = append(lines, oid.String()+" "+k.String())
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// Each saved pack, read from scratch, gives exactly the saved list of
// objects, and is stored the way its name says.
func TestReadSavedPacks(t *testing.T) {
	first := string(readFile(t, "first.objects"))
	cases := []struct {
		pack, base, want string
		stats            Stats
	}{
		{"first-ofs.pack", "", first, Stats{Whole: 11, OfsDeltas: 1}},
		{"first-ref.pack", "", first, Stats{Whole: 11, RefDeltas: 1}},
		{"second-thin.pack", "first-ofs.pack", first + string(readFile(t, "second.objects")), Stats{Whole: 3, RefDeltas: 2}},
		{"tag.pack", "", string(readFile(t, "tag.objects")), Stats{Whole: 1}},
		{"side.pack", "", string(readFile(t, "side.objects")), Stats{Whole: 3}},
	}
	for _, c := range cases {
		t.Run(c.pack, func(t *testing.T) {
			sink := newSink()
			if c.base != "" {
				if _, err := Read(bytes.NewReader(readFile(t, c.base)), sink); err != nil {
					t.Fatal(err)
				}
			}
			stats, err := Read(bytes.NewReader(readFile(t, c.pack)), sink)
			if err != nil {
				t.Fatal(err)
			}
			if stats != c.stats {
				t.Errorf("entries %+v, want %+v", stats, c.stats)
			}
			want := strings.Split(strings.TrimSpace(c.want), "\n")
			sort.Strings(want)
			if got := sink.listing(); got != strings.Join(want, "\n")+"\n" {
				t.Errorf("objects:\n%swant:\n%s", got, strings.Join(want, "\n"))
			}
		})
	}
}

func TestBrokenPackFixtures(t *testing.T) {
	var cases []struct {
		Name, Pack, Change, Hex, Error string
		At                             int
	}
	loadJSON(t, "broken.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			data := readFile(t, c.Pack)
			at := c.At
			if at < 0 {
				at += len(data)
			}
			patch, _ := hex.DecodeString(c.Hex)
			switch c.Change {
			case "set":
				copy(data[at:], patch)
			case "flip":
				data[at] ^= 1
			case "truncate":
				data = data[:at]
			case "append":
				data = append(data, patch...)
			case "none":
			default:
				t.Fatalf("unknown change %q", c.Change)
			}
			sink := newSink()
			if _, err := Read(bytes.NewReader(data), sink); errText(err) != c.Error {
				t.Fatalf("got %q, want %q", errText(err), c.Error)
			}
		})
	}
}

func TestDeltaFixtures(t *testing.T) {
	var cases []struct {
		Name, Base, Error string
		DeltaHex          string `json:"delta_hex"`
		Result            *string
	}
	loadJSON(t, "deltas.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			delta, err := hex.DecodeString(strings.ReplaceAll(c.DeltaHex, " ", ""))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ApplyDelta([]byte(c.Base), delta, 1<<20)
			if errText(err) != c.Error {
				t.Fatalf("error %q, want %q", errText(err), c.Error)
			}
			if c.Result != nil && string(got) != *c.Result {
				t.Fatalf("result %q, want %q", got, *c.Result)
			}
		})
	}
}

// A pack with no objects has one fixed form.
func TestWriteEmptyPack(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, nil, nil); err != nil {
		t.Fatal(err)
	}
	const want = "5041434b0000000200000000029d08823bd8a8eab510ad6ac75c823cfd3ed31e"
	if got := hex.EncodeToString(out.Bytes()); got != want {
		t.Fatalf("empty pack is %s, want %s", got, want)
	}
	if st, err := Read(&out, newSink()); err != nil || st != (Stats{}) {
		t.Fatalf("reading it back: %+v %v", st, err)
	}
}

// What Write produces, Read takes back unchanged and in the same order.
func TestWriteThenRead(t *testing.T) {
	src := newSink()
	if _, err := Read(bytes.NewReader(readFile(t, "first-ofs.pack")), src); err != nil {
		t.Fatal(err)
	}
	big, _ := src.Write(store.Blob, bytes.Repeat([]byte("made-up filler "), 5000))
	_ = big
	var out bytes.Buffer
	if err := Write(&out, src.order, src.get); err != nil {
		t.Fatal(err)
	}
	dst := newSink()
	st, err := Read(bytes.NewReader(out.Bytes()), dst)
	if err != nil {
		t.Fatal(err)
	}
	if st != (Stats{Whole: len(src.order)}) {
		t.Fatalf("entries %+v", st)
	}
	if !reflect.DeepEqual(dst.order, src.order) || !reflect.DeepEqual(dst.data, src.data) {
		t.Fatal("the objects read back differ from the objects written")
	}
}

func TestObjectTooLarge(t *testing.T) {
	// A blob entry that states a size one byte over the limit.
	size := store.MaxObjectSize + 1
	entry := []byte{byte(store.Blob)<<4 | byte(size&15) | 0x80}
	for size >>= 4; ; size >>= 7 {
		if size < 0x80 {
			entry = append(entry, byte(size))
			break
		}
		entry = append(entry, byte(size&0x7f)|0x80)
	}
	data := append([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01"), entry...)
	_, err := Read(bytes.NewReader(data), newSink())
	if errText(err) != "E_NEEDS_SUBTOOL: an object in the pack is larger than Plainforge handles" {
		t.Fatalf("got %v", err)
	}
}
