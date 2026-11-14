package plainforge_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNoEmDashesAnywhere(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".pack") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(b) {
			return nil
		}
		if strings.ContainsRune(string(b), '\u2014') {
			t.Errorf("%s contains an em dash (U+2014)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStandardLibraryOnlyAndModulePath(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "module github.com/ClearedStack/plainforge\n") {
		t.Fatalf("unexpected module path in go.mod:\n%s", text)
	}
	if strings.Contains(text, "require") {
		t.Fatalf("external dependency found in go.mod:\n%s", text)
	}
	for _, forbidden := range []string{"Dockerfile", "Containerfile", "go.sum"} {
		if _, err := os.Stat(forbidden); err == nil {
			t.Errorf("%s must not exist", forbidden)
		}
	}
}

func TestDeployManifestsAndKoConfig(t *testing.T) {
	ko, err := os.ReadFile(".ko.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ko), "defaultBaseImage: gcr.io/distroless/static@sha256:") {
		t.Fatalf(".ko.yaml base image is not pinned by digest:\n%s", ko)
	}

	pf, err := os.ReadFile("deploy/plainforge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pfText := string(pf)
	requiredInPlainforge := []string{
		"namespace: clearedstack",
		"image: lazarette.clearedstack.svc.cluster.local:5000/plainforge@sha256:",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"key: kubernetes.io/arch",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		`operator: NotIn`,
		`values: ["cm3588-plus"]`,
		"memory: 64Mi",
		"audience: keelworks",
		"storageClassName: longhorn",
	}
	for _, want := range requiredInPlainforge {
		if !strings.Contains(pfText, want) {
			t.Errorf("deploy/plainforge.yaml is missing %q", want)
		}
	}
	if strings.Contains(pfText, "kind: Secret") {
		t.Error("deploy/plainforge.yaml must not contain inline Secret manifests")
	}

	np, err := os.ReadFile("deploy/networkpolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	npText := string(np)
	requiredInNetPol := []string{
		"name: default-deny",
		"name: plainforge",
		"namespace: clearedstack",
		"- Ingress",
		"- Egress",
		"app.kubernetes.io/name: keelworks",
		"app.kubernetes.io/name: shipkeeper",
		"app.kubernetes.io/name: bellbuoy",
		"port: 443",
	}
	for _, want := range requiredInNetPol {
		if !strings.Contains(npText, want) {
			t.Errorf("deploy/networkpolicy.yaml is missing %q", want)
		}
	}
}
