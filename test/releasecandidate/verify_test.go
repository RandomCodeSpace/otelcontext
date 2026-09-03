package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTag      = "v0.4.0"
	testHostKey  = runtime.GOOS + "_" + runtime.GOARCH
	stubMainFile = `package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
)

//go:embed static/index.html static/app.js static/app.css
var ui embed.FS

var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("OtelContext version %s\n", Version)
		return
	}
	entries, _ := fs.ReadDir(ui, "static")
	fmt.Println(len(entries))
}
`
)

// stubBuild is the once-per-process set of cross-compiled stub binaries.
type stubBuild struct {
	sha          string
	sourceRoot   string
	binaries     map[string][]byte // "<goos>_<goarch>" -> binary
	wrongVersion []byte            // linux/amd64 built without -trimpath and with -X main.Version=v9.9.9
	certPEM      []byte
	err          error
}

var (
	stubOnce sync.Once
	stub     *stubBuild
)

func buildStub(t *testing.T) *stubBuild {
	t.Helper()
	stubOnce.Do(func() { stub = newStubBuild() })
	if stub.err != nil {
		t.Fatalf("build stub binaries: %v", stub.err)
	}
	return stub
}

func newStubBuild() *stubBuild {
	s := &stubBuild{binaries: map[string][]byte{}}
	if _, err := exec.LookPath("git"); err != nil {
		s.err = fmt.Errorf("git is required to stamp vcs.revision: %w", err)
		return s
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		s.err = err
		return s
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		s.err = err
		return s
	}
	s.sourceRoot = root

	work, err := os.MkdirTemp("", "releasecandidate-stub-")
	if err != nil {
		s.err = err
		return s
	}
	mod := filepath.Join(work, "stub")
	if err := os.MkdirAll(filepath.Join(mod, "static"), 0o755); err != nil {
		s.err = err
		return s
	}
	for _, name := range uiFiles {
		data, err := os.ReadFile(filepath.Join(root, "internal", "ui", "static", name))
		if err != nil {
			s.err = err
			return s
		}
		if err := os.WriteFile(filepath.Join(mod, "static", name), data, 0o644); err != nil {
			s.err = err
			return s
		}
	}
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/stub\n\ngo 1.25\n"), 0o644); err != nil {
		s.err = err
		return s
	}
	if err := os.WriteFile(filepath.Join(mod, "main.go"), []byte(stubMainFile), 0o644); err != nil {
		s.err = err
		return s
	}
	gitEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=stub", "-c", "user.email=stub@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = mod
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "stub"}} {
		if _, err := git(args...); err != nil {
			s.err = err
			return s
		}
	}
	s.sha, err = git("rev-parse", "HEAD")
	if err != nil {
		s.err = err
		return s
	}

	type target struct{ goos, goarch, version, key string }
	// The release build uses -trimpath, under which Go omits the -ldflags
	// buildinfo setting. The wrong-version stub is built without -trimpath so
	// its recorded ldflags exercise the verifier's ldflags branch.
	targets := []target{{"linux", "amd64", "v9.9.9", "wrong"}}
	for _, tg := range verifyTargets {
		targets = append(targets, target{tg[0], tg[1], testTag, tg[0] + "_" + tg[1]})
	}
	outputs := make([][]byte, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		go func(i int, tg target) {
			defer wg.Done()
			out := filepath.Join(work, "bin", tg.key, "otelcontext")
			args := []string{"build", "-buildvcs=true", "-ldflags", "-X main.Version=" + tg.version, "-o", out, "."}
			if tg.key != "wrong" {
				args = append([]string{"build", "-trimpath"}, args[1:]...)
			}
			cmd := exec.Command(goBin, args...)
			cmd.Dir = mod
			cmd.Env = append(os.Environ(), "GOOS="+tg.goos, "GOARCH="+tg.goarch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off", "GO111MODULE=on")
			if msg, err := cmd.CombinedOutput(); err != nil {
				errs[i] = fmt.Errorf("go build %s/%s: %v: %s", tg.goos, tg.goarch, err, msg)
				return
			}
			outputs[i], errs[i] = os.ReadFile(out)
		}(i, tg)
	}
	wg.Wait()
	for i, tg := range targets {
		if errs[i] != nil {
			s.err = errs[i]
			return s
		}
		if tg.key == "wrong" {
			s.wrongVersion = outputs[i]
		} else {
			s.binaries[tg.key] = outputs[i]
		}
	}
	s.certPEM, s.err = selfSignedCertPEM()
	_ = os.RemoveAll(work)
	return s
}

func selfSignedCertPEM() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "releasecandidate test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

type tarEntry struct {
	name     string
	mode     int64
	data     []byte
	typeflag byte
	linkname string
}

// layout describes one assets directory. checksums nil means "compute from
// the archives and SBOMs".
type layout struct {
	archives  map[string][]tarEntry
	sboms     map[string][]byte
	checksums []byte
	sig       []byte
	pem       []byte
	extra     map[string][]byte
	remove    []string
}

func archiveNameFor(goos, goarch string) string {
	return fmt.Sprintf("otelcontext_%s_%s_%s.tar.gz", strings.TrimPrefix(testTag, "v"), goos, goarch)
}

func (s *stubBuild) happyLayout() *layout {
	l := &layout{archives: map[string][]tarEntry{}, sboms: map[string][]byte{}, extra: map[string][]byte{}}
	for _, tg := range verifyTargets {
		name := archiveNameFor(tg[0], tg[1])
		l.archives[name] = stubEntries(s.binaries[tg[0]+"_"+tg[1]])
		l.sboms[name+".sbom.json"] = []byte(fmt.Sprintf(`{"spdxVersion":"SPDX-2.3","name":"%s","packages":[]}`+"\n", name))
	}
	l.sig = []byte("MEUCIQDfakeSignature==\n")
	l.pem = []byte(base64.StdEncoding.EncodeToString(s.certPEM) + "\n")
	return l
}

func stubEntries(binary []byte) []tarEntry {
	return []tarEntry{
		{name: "otelcontext", mode: 0o755, data: binary, typeflag: tar.TypeReg},
		{name: "README.md", mode: 0o644, data: []byte("# OtelContext\n"), typeflag: tar.TypeReg},
		{name: "LICENSE.md", mode: 0o644, data: []byte("MIT\n"), typeflag: tar.TypeReg},
		{name: "deploy/systemd/", mode: 0o755, typeflag: tar.TypeDir},
		{name: "deploy/systemd/otelcontext.service", mode: 0o644, data: []byte("[Unit]\n"), typeflag: tar.TypeReg},
		{name: "deploy/systemd/otelcontext.env.example", mode: 0o644, data: []byte("HTTP_PORT=8080\n"), typeflag: tar.TypeReg},
	}
}

func writeTarGz(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	var buf bytes.Buffer
	// Stored blocks: deflate under the race detector would dominate the run.
	gz, err := gzip.NewWriterLevel(&buf, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typeflag, Linkname: e.linkname, ModTime: time.Unix(0, 0)}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// writeAssets materialises a layout into dir and returns the checksums.txt bytes.
func writeAssets(t *testing.T, dir string, l *layout) []byte {
	t.Helper()
	var lines []string
	names := make([]string, 0, len(l.archives))
	for name := range l.archives {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeTarGz(t, filepath.Join(dir, name), l.archives[name])
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, sha256Hex(data)+"  "+name)
	}
	sbomNames := make([]string, 0, len(l.sboms))
	for name := range l.sboms {
		sbomNames = append(sbomNames, name)
	}
	sort.Strings(sbomNames)
	for _, name := range sbomNames {
		if err := os.WriteFile(filepath.Join(dir, name), l.sboms[name], 0o644); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, sha256Hex(l.sboms[name])+"  "+name)
	}
	checksums := l.checksums
	if checksums == nil {
		checksums = []byte(strings.Join(lines, "\n") + "\n")
	}
	files := map[string][]byte{"checksums.txt": checksums, "checksums.txt.sig": l.sig, "checksums.txt.pem": l.pem}
	for name, data := range l.extra {
		files[name] = data
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range l.remove {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return checksums
}

// writeCosignStub writes a cosign stand-in that appends its argv (one per
// line) to argvFile and exits with code.
func writeCosignStub(t *testing.T, dir string, code int) (script, argvFile string) {
	t.Helper()
	argvFile = filepath.Join(dir, "cosign-argv.txt")
	script = filepath.Join(dir, "cosign")
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\nexit %d\n", argvFile, code)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, argvFile
}

type runOpts struct {
	sha        string
	sourceRoot string
	cosignExit int
	skipExec   bool
}

type runResult struct {
	err        error
	report     assetsReport
	raw        []byte
	cosignArgs []string
	assetsDir  string
	extractDir string
	checksums  []byte
}

func runVerify(t *testing.T, l *layout, opts runOpts) runResult {
	t.Helper()
	s := buildStub(t)
	base := t.TempDir()
	assetsDir := filepath.Join(base, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	checksums := writeAssets(t, assetsDir, l)
	cosign, argvFile := writeCosignStub(t, base, opts.cosignExit)
	sha := opts.sha
	if sha == "" {
		sha = s.sha
	}
	sourceRoot := opts.sourceRoot
	if sourceRoot == "" {
		sourceRoot = s.sourceRoot
	}
	extractDir := filepath.Join(base, "extract")
	out := filepath.Join(base, "out", "release-assets-v1.json")
	args := []string{
		"--tag", testTag, "--sha", sha, "--assets", assetsDir, "--extract", extractDir,
		"--source-root", sourceRoot, "--out", out, "--cosign", cosign,
	}
	if opts.skipExec {
		args = append(args, "--skip-exec")
	}
	res := runResult{assetsDir: assetsDir, extractDir: extractDir, checksums: checksums}
	res.err = runVerifyAssets(args)
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("release-assets-v1.json not written: %v (run error: %v)", err, res.err)
	}
	res.raw = raw
	if err := json.Unmarshal(raw, &res.report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, raw)
	}
	if argv, err := os.ReadFile(argvFile); err == nil {
		res.cosignArgs = strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	}
	return res
}

func checkByName(t *testing.T, r assetsReport, name string) checkRecord {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not in report: %+v", name, r.Checks)
	return checkRecord{}
}

var checkOrder = []string{"assets", "checksums", "certificate", "signature", "archives", "embedded_ui", "version", "sboms"}

func TestVerifyAssetsHappyPath(t *testing.T) {
	s := buildStub(t)
	res := runVerify(t, s.happyLayout(), runOpts{})
	if res.err != nil {
		t.Fatalf("verify failed: %v\n%s", res.err, res.raw)
	}
	r := res.report
	if r.SchemaVersion != "otelcontext.release-assets.v1" || r.Tag != testTag || r.SHA != s.sha {
		t.Fatalf("header mismatch: %+v", r)
	}
	wantIdentity := "https://github.com/RandomCodeSpace/otelcontext/.github/workflows/release.yml@refs/tags/" + testTag
	if r.CertificateIdentity != wantIdentity {
		t.Fatalf("identity %q, want %q", r.CertificateIdentity, wantIdentity)
	}
	if r.CertificateWrapper != "base64" {
		t.Fatalf("wrapper %q, want base64", r.CertificateWrapper)
	}
	if r.ChecksumsSHA256 != sha256Hex(res.checksums) {
		t.Fatalf("checksums_sha256 %q, want %q", r.ChecksumsSHA256, sha256Hex(res.checksums))
	}
	if !r.SignatureVerified {
		t.Fatal("signature_verified is false")
	}

	// Checks: fixed order, all passed.
	if len(r.Checks) != len(checkOrder) {
		t.Fatalf("got %d checks, want %d: %+v", len(r.Checks), len(checkOrder), r.Checks)
	}
	for i, name := range checkOrder {
		if r.Checks[i].Name != name {
			t.Fatalf("check %d is %q, want %q", i, r.Checks[i].Name, name)
		}
		if !r.Checks[i].Passed {
			t.Fatalf("check %q failed: %s", name, r.Checks[i].Detail)
		}
	}

	// Assets: 11, sorted, correct kinds and digests.
	if len(r.Assets) != 11 {
		t.Fatalf("got %d assets, want 11", len(r.Assets))
	}
	kinds := map[string]int{}
	for i, a := range r.Assets {
		if i > 0 && r.Assets[i-1].Name >= a.Name {
			t.Fatalf("assets not sorted at %q", a.Name)
		}
		data, err := os.ReadFile(filepath.Join(res.assetsDir, a.Name))
		if err != nil {
			t.Fatal(err)
		}
		if a.SHA256 != sha256Hex(data) || a.SizeBytes != int64(len(data)) {
			t.Fatalf("asset %s digest/size mismatch: %+v", a.Name, a)
		}
		kinds[a.Kind]++
	}
	if kinds["archive"] != 4 || kinds["sbom"] != 4 || kinds["checksums"] != 1 || kinds["signature"] != 1 || kinds["certificate"] != 1 {
		t.Fatalf("asset kinds %v", kinds)
	}

	// Archives: 4, sorted, buildinfo and digests recorded, only host executed.
	if len(r.Archives) != 4 {
		t.Fatalf("got %d archives, want 4", len(r.Archives))
	}
	wantFiles := []string{"LICENSE.md", "README.md", "deploy/systemd/otelcontext.env.example", "deploy/systemd/otelcontext.service", "otelcontext"}
	executed := 0
	for i, a := range r.Archives {
		if i > 0 && r.Archives[i-1].Name >= a.Name {
			t.Fatalf("archives not sorted at %q", a.Name)
		}
		key := a.GOOS + "_" + a.GOARCH
		if a.Name != archiveNameFor(a.GOOS, a.GOARCH) {
			t.Fatalf("archive %s does not match %s/%s", a.Name, a.GOOS, a.GOARCH)
		}
		if a.BinarySHA256 != sha256Hex(s.binaries[key]) {
			t.Fatalf("%s: binary_sha256 %q, want %q", a.Name, a.BinarySHA256, sha256Hex(s.binaries[key]))
		}
		archiveData, err := os.ReadFile(filepath.Join(res.assetsDir, a.Name))
		if err != nil {
			t.Fatal(err)
		}
		if a.ArchiveSHA256 != sha256Hex(archiveData) {
			t.Fatalf("%s: archive_sha256 mismatch", a.Name)
		}
		if a.SBOM != a.Name+".sbom.json" {
			t.Fatalf("%s: sbom %q", a.Name, a.SBOM)
		}
		sbomData, err := os.ReadFile(filepath.Join(res.assetsDir, a.SBOM))
		if err != nil {
			t.Fatal(err)
		}
		if a.SBOMSHA256 != sha256Hex(sbomData) {
			t.Fatalf("%s: sbom_sha256 mismatch", a.Name)
		}
		if a.VCSRevision != s.sha {
			t.Fatalf("%s: vcs_revision %q, want %q", a.Name, a.VCSRevision, s.sha)
		}
		if !a.EmbeddedUI {
			t.Fatalf("%s: embedded_ui false", a.Name)
		}
		if strings.Join(a.Files, ",") != strings.Join(wantFiles, ",") {
			t.Fatalf("%s: files %v, want %v", a.Name, a.Files, wantFiles)
		}
		if key == testHostKey {
			if !a.Executed || a.Version != "OtelContext version "+testTag {
				t.Fatalf("%s: executed=%v version=%q", a.Name, a.Executed, a.Version)
			}
			executed++
		} else if a.Executed || a.Version != "" {
			t.Fatalf("%s: non-host archive executed=%v version=%q", a.Name, a.Executed, a.Version)
		}
		if _, err := os.Stat(filepath.Join(res.extractDir, key, "otelcontext")); err != nil {
			t.Fatalf("%s: extracted binary missing: %v", a.Name, err)
		}
	}
	if executed != 1 {
		t.Fatalf("executed %d archives, want 1 (host %s)", executed, testHostKey)
	}

	// cosign argv is exactly the contract's list.
	wantArgs := []string{
		"verify-blob",
		"--certificate", filepath.Join(res.extractDir, "checksums-certificate.pem"),
		"--signature", filepath.Join(res.assetsDir, "checksums.txt.sig"),
		"--certificate-identity", wantIdentity,
		"--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
		filepath.Join(res.assetsDir, "checksums.txt"),
	}
	if strings.Join(res.cosignArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("cosign argv\n got %q\nwant %q", res.cosignArgs, wantArgs)
	}
	decoded, err := os.ReadFile(filepath.Join(res.extractDir, "checksums-certificate.pem"))
	if err != nil || !bytes.Equal(decoded, s.certPEM) {
		t.Fatalf("decoded certificate not the raw PEM (err %v)", err)
	}

	// Output format: fixed key order, 2-space indent, trailing newline.
	if !strings.HasPrefix(string(res.raw), "{\n  \"schema_version\": \"otelcontext.release-assets.v1\",\n  \"tag\":") || !strings.HasSuffix(string(res.raw), "}\n") {
		t.Fatalf("unexpected JSON framing:\n%s", res.raw[:min(len(res.raw), 200)])
	}
}

func TestVerifyAssetsRawPEMAndSkipExec(t *testing.T) {
	s := buildStub(t)
	l := s.happyLayout()
	l.pem = s.certPEM
	res := runVerify(t, l, runOpts{skipExec: true})
	if res.err != nil {
		t.Fatalf("verify failed: %v\n%s", res.err, res.raw)
	}
	if res.report.CertificateWrapper != "pem" {
		t.Fatalf("wrapper %q, want pem", res.report.CertificateWrapper)
	}
	for _, a := range res.report.Archives {
		if a.Executed {
			t.Fatalf("%s executed despite --skip-exec", a.Name)
		}
	}
	if c := checkByName(t, res.report, "version"); !c.Passed || !strings.Contains(c.Detail, "skip") {
		t.Fatalf("version check %+v", c)
	}
}

func TestVerifyAssetsFailures(t *testing.T) {
	s := buildStub(t)
	hostArchive := archiveNameFor(runtime.GOOS, runtime.GOARCH)
	linuxAmd64 := archiveNameFor("linux", "amd64")

	brokenSourceRoot := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "src")
		static := filepath.Join(dir, "internal", "ui", "static")
		if err := os.MkdirAll(static, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range uiFiles {
			data, err := os.ReadFile(filepath.Join(s.sourceRoot, "internal", "ui", "static", name))
			if err != nil {
				t.Fatal(err)
			}
			if name == "index.html" {
				data = append(data, []byte("<!-- not in the binary -->\n")...)
			}
			if err := os.WriteFile(filepath.Join(static, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	cases := []struct {
		name      string
		mutate    func(t *testing.T, l *layout, o *runOpts)
		wantCheck string
	}{
		{"missing asset", func(t *testing.T, l *layout, o *runOpts) {
			l.remove = []string{linuxAmd64 + ".sbom.json"}
		}, "assets"},
		{"extra asset", func(t *testing.T, l *layout, o *runOpts) {
			l.extra["otelcontext_0.4.0_windows_amd64.zip"] = []byte("nope")
		}, "assets"},
		{"bad checksum", func(t *testing.T, l *layout, o *runOpts) {
			l.sboms[linuxAmd64+".sbom.json"] = []byte(`{"spdxVersion":"SPDX-2.3","tampered":true}`)
			l.checksums = []byte(strings.Repeat("0", 64) + "  " + linuxAmd64 + ".sbom.json\n")
		}, "checksums"},
		{"malformed pem", func(t *testing.T, l *layout, o *runOpts) {
			l.pem = []byte(base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")) + "\n")
		}, "certificate"},
		{"non-pem garbage", func(t *testing.T, l *layout, o *runOpts) {
			l.pem = []byte("this is not a certificate\n")
		}, "certificate"},
		{"extra executable in archive", func(t *testing.T, l *layout, o *runOpts) {
			for i, e := range l.archives[linuxAmd64] {
				if e.name == "README.md" {
					l.archives[linuxAmd64][i].mode = 0o755
				}
			}
		}, "archives"},
		{"symlink in archive", func(t *testing.T, l *layout, o *runOpts) {
			l.archives[linuxAmd64] = append(l.archives[linuxAmd64], tarEntry{name: "link", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"})
		}, "archives"},
		{"wrong version ldflag", func(t *testing.T, l *layout, o *runOpts) {
			l.archives[linuxAmd64] = stubEntries(s.wrongVersion)
		}, "archives"},
		{"missing UI bytes", func(t *testing.T, l *layout, o *runOpts) {
			o.sourceRoot = brokenSourceRoot(t)
		}, "embedded_ui"},
		{"wrong vcs.revision", func(t *testing.T, l *layout, o *runOpts) {
			o.sha = strings.Repeat("a", 40)
		}, "archives"},
		{"cosign exit 1", func(t *testing.T, l *layout, o *runOpts) {
			o.cosignExit = 1
		}, "signature"},
		{"invalid sbom json", func(t *testing.T, l *layout, o *runOpts) {
			l.sboms[linuxAmd64+".sbom.json"] = []byte("{not json")
		}, "sboms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := s.happyLayout()
			opts := runOpts{}
			tc.mutate(t, l, &opts)
			res := runVerify(t, l, opts)
			if res.err == nil {
				t.Fatalf("expected failure\n%s", res.raw)
			}
			if !strings.Contains(res.err.Error(), tc.wantCheck) {
				t.Fatalf("error %q does not name check %q", res.err, tc.wantCheck)
			}
			if c := checkByName(t, res.report, tc.wantCheck); c.Passed {
				t.Fatalf("check %q passed, want failure\n%s", tc.wantCheck, res.raw)
			}
			if len(res.report.Checks) != len(checkOrder) {
				t.Fatalf("not every check ran: %+v", res.report.Checks)
			}
			if tc.wantCheck == "signature" && res.report.SignatureVerified {
				t.Fatal("signature_verified true after cosign failure")
			}
			if tc.name == "wrong version ldflag" {
				c := checkByName(t, res.report, "archives")
				if !strings.Contains(c.Detail, "-X main.Version=v9.9.9") {
					t.Fatalf("archives detail does not quote the recorded ldflags: %s", c.Detail)
				}
				if v := checkByName(t, res.report, "version"); linuxAmd64 == hostArchive && v.Passed {
					t.Fatalf("host-native --version passed with the wrong stamp: %s", v.Detail)
				}
			}
		})
	}
}
