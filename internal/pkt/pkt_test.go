package pkt

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestReadFixtures(t *testing.T) {
	var cases []struct {
		Name, Input, Error string
		Packets            []string
	}
	load(t, "read.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			r := NewReader(strings.NewReader(c.Input))
			got, gotErr := []string{}, ""
			for {
				p, flush, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					gotErr = err.Error()
					break
				}
				if flush {
					got = append(got, "FLUSH")
				} else {
					got = append(got, string(p))
				}
			}
			if !reflect.DeepEqual(got, c.Packets) || gotErr != c.Error {
				t.Fatalf("got %q, error %q; want %q, error %q", got, gotErr, c.Packets, c.Error)
			}
		})
	}
}

func TestWriteFixtures(t *testing.T) {
	var cases []struct{ Name, Payload, Output, Error string }
	load(t, "write.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			gotErr := ""
			if err := WriteString(&out, c.Payload); err != nil {
				gotErr = err.Error()
			}
			if out.String() != c.Output || gotErr != c.Error {
				t.Fatalf("got %q, error %q; want %q, error %q", out.String(), gotErr, c.Output, c.Error)
			}
		})
	}
}

func TestLongestLine(t *testing.T) {
	var out bytes.Buffer
	if err := Write(&out, bytes.Repeat([]byte("a"), MaxPayload)); err != nil {
		t.Fatal(err)
	}
	if got := out.String()[:4]; got != "fff0" {
		t.Fatalf("length prefix %q, want fff0", got)
	}
	if err := Write(&out, bytes.Repeat([]byte("a"), MaxPayload+1)); err == nil {
		t.Fatal("a line one byte too long was written")
	}
	p, _, err := NewReader(&out).Next()
	if err != nil || len(p) != MaxPayload {
		t.Fatalf("read back %d bytes, error %v", len(p), err)
	}
	var flush bytes.Buffer
	Flush(&flush)
	if flush.String() != "0000" {
		t.Fatalf("flush is %q", flush.String())
	}
}
