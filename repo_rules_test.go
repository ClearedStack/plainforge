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
	koText := string(ko)
	for _, want := range []string{
		"defaultBaseImage: gcr.io/distroless/static@sha256:",
		"main: ./cmd/plainforge\n",
		"main: ./cmd/plainforge-backup\n",
	} {
		if !strings.Contains(koText, want) {
			t.Fatalf(".ko.yaml is missing %q:\n%s", want, koText)
		}
	}

	kust, err := os.ReadFile("deploy/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	kustText := string(kust)
	for _, want := range []string{
		"- priorityclass.yaml\n",
		"- networkpolicy.yaml\n",
		"- plainforge.yaml\n",
		"- backup-cronjob.yaml\n",
	} {
		if !strings.Contains(kustText, want) {
			t.Errorf("deploy/kustomization.yaml is missing %q", want)
		}
	}

	appYAML, err := os.ReadFile("app.yaml")
	if err != nil {
		t.Fatal(err)
	}
	appText := string(appYAML)
	for _, want := range []string{
		"name: plainforge\n",
		"namespace: clearedstack\n",
		"image: plainforge\n",
		"digest: sha256:",
		"replicas: 1\n",
		"port: 8080\n",
		"memory: 64Mi\n",
		"- clearedstack/keelworks\n",
		"- clearedstack/keelworks-build\n",
		"- cs-workers/bellbuoy\n",
	} {
		if !strings.Contains(appText, want) {
			t.Errorf("app.yaml is missing %q", want)
		}
	}

	pc, err := os.ReadFile("deploy/priorityclass.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pc), "name: clearedstack-high") {
		t.Errorf("deploy/priorityclass.yaml is missing clearedstack-high")
	}

	pf, err := os.ReadFile("deploy/plainforge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pfText := string(pf)
	requiredInPlainforge := []string{
		"namespace: clearedstack",
		"port: 8080",
		"priorityClassName: clearedstack-high",
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
		"-notify-url=http://keelworks.clearedstack.svc.cluster.local:8080/webhook",
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

	backup, err := os.ReadFile("deploy/backup-cronjob.yaml")
	if err != nil {
		t.Fatal(err)
	}
	backupText := string(backup)
	for _, want := range []string{
		"name: plainforge-backup",
		"name: plainforge-restore-drill",
		"image: lazarette.clearedstack.svc.cluster.local:5000/plainforge-backup@sha256:",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		`drop: ["ALL"]`,
	} {
		if !strings.Contains(backupText, want) {
			t.Errorf("deploy/backup-cronjob.yaml is missing %q", want)
		}
	}
	if strings.Contains(backupText, "kind: Secret") {
		t.Error("deploy/backup-cronjob.yaml must not contain inline Secret manifests")
	}

	np, err := os.ReadFile("deploy/networkpolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	npText := string(np)
	requiredInNetPol := []string{
		"name: default-deny",
		"name: plainforge",
		"name: plainforge-backup",
		"namespace: clearedstack",
		"- Ingress",
		"- Egress",
		"app.kubernetes.io/name: keelworks",
		"app.kubernetes.io/name: keelworks-build",
		"app.kubernetes.io/name: shipkeeper",
		"app.kubernetes.io/name: bellbuoy",
		"port: 443",
	}
	for _, want := range requiredInNetPol {
		if !strings.Contains(npText, want) {
			t.Errorf("deploy/networkpolicy.yaml is missing %q", want)
		}
	}

	for _, contractFile := range []string{
		"contract/v1/forge.md",
		"contract/v1/examples/forge_exchange.json",
	} {
		if _, err := os.Stat(contractFile); err != nil {
			t.Errorf("missing required contract file %s: %v", contractFile, err)
		}
	}
}
