package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	assetsSchemaVersion = "otelcontext.release-assets.v1"
	oidcIssuer          = "https://token.actions.githubusercontent.com"
	defaultIdentity     = "https://github.com/RandomCodeSpace/otelcontext/.github/workflows/release.yml@refs/tags/"
	decodedCertName     = "checksums-certificate.pem"
	versionExecTimeout  = 30 * time.Second
)

var (
	verifyTargets = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
	// archiveFiles is the exact regular-file set of every release archive.
	archiveFiles = []string{
		"otelcontext",
		"README.md",
		"LICENSE.md",
		"deploy/systemd/otelcontext.service",
		"deploy/systemd/otelcontext.env.example",
	}
	uiFiles = []string{"index.html", "app.js", "app.css"}
	sha40   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type assetRecord struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type archiveRecord struct {
	Name          string   `json:"name"`
	GOOS          string   `json:"goos"`
	GOARCH        string   `json:"goarch"`
	ArchiveSHA256 string   `json:"archive_sha256"`
	BinarySHA256  string   `json:"binary_sha256"`
	SBOM          string   `json:"sbom"`
	SBOMSHA256    string   `json:"sbom_sha256"`
	Version       string   `json:"version"`
	VCSRevision   string   `json:"vcs_revision"`
	EmbeddedUI    bool     `json:"embedded_ui"`
	Executed      bool     `json:"executed"`
	Files         []string `json:"files"`

	binaryPath string
	binary     []byte
}

type checkRecord struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type assetsReport struct {
	SchemaVersion       string          `json:"schema_version"`
	Tag                 string          `json:"tag"`
	SHA                 string          `json:"sha"`
	CertificateIdentity string          `json:"certificate_identity"`
	CertificateWrapper  string          `json:"certificate_wrapper"`
	ChecksumsSHA256     string          `json:"checksums_sha256"`
	SignatureVerified   bool            `json:"signature_verified"`
	Assets              []assetRecord   `json:"assets"`
	Archives            []archiveRecord `json:"archives"`
	Checks              []checkRecord   `json:"checks"`
}

// verifier carries the parsed flags and the state shared between checks.
type verifier struct {
	tag, sha, assets, extract, sourceRoot, out, identity, cosign string
	skipExec                                                     bool

	report      assetsReport
	assetSHA    map[string]string // asset name -> sha256 of the file in --assets
	decodedCert string            // path of the unwrapped certificate PEM
}

// check appends one check result. failures empty means passed.
func (v *verifier) check(name string, failures []string, okDetail string) bool {
	if len(failures) == 0 {
		v.report.Checks = append(v.report.Checks, checkRecord{Name: name, Passed: true, Detail: okDetail})
		return true
	}
	v.report.Checks = append(v.report.Checks, checkRecord{Name: name, Passed: false, Detail: strings.Join(failures, "; ")})
	return false
}

// runVerifyAssets verifies the downloaded draft release assets and writes
// release-assets-v1.json. Every check runs; the file is written even when
// checks failed, and the returned error then lists the failed checks.
func runVerifyAssets(args []string) error {
	v := &verifier{}
	flags := flag.NewFlagSet("verify-assets", flag.ContinueOnError)
	flags.StringVar(&v.tag, "tag", "", "release tag (vX.Y.Z[-pre])")
	flags.StringVar(&v.sha, "sha", "", "40-hex commit SHA the tag points at")
	flags.StringVar(&v.assets, "assets", "", "directory holding the downloaded draft assets")
	flags.StringVar(&v.extract, "extract", "", "directory archives are extracted into")
	flags.StringVar(&v.sourceRoot, "source-root", "", "source checkout root (for internal/ui/static)")
	flags.StringVar(&v.out, "out", "", "path of release-assets-v1.json to write")
	flags.StringVar(&v.identity, "identity", "", "cosign certificate identity (default: release.yml@refs/tags/<tag>)")
	flags.StringVar(&v.cosign, "cosign", "", "cosign binary (default: $OTELCONTEXT_COSIGN, then cosign on PATH)")
	flags.BoolVar(&v.skipExec, "skip-exec", false, "do not run the host-native binary with --version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("verify-assets: unexpected arguments %q", flags.Args())
	}
	for name, val := range map[string]string{"tag": v.tag, "sha": v.sha, "assets": v.assets, "extract": v.extract, "source-root": v.sourceRoot, "out": v.out} {
		if val == "" {
			return fmt.Errorf("verify-assets: --%s is required", name)
		}
	}
	if !strings.HasPrefix(v.tag, "v") || len(v.tag) < 2 {
		return fmt.Errorf("verify-assets: --tag %q must start with v", v.tag)
	}
	if !sha40.MatchString(v.sha) {
		return errors.New("verify-assets: --sha must be 40 lowercase hex characters")
	}
	if v.identity == "" {
		v.identity = defaultIdentity + v.tag
	}
	if v.cosign == "" {
		v.cosign = os.Getenv("OTELCONTEXT_COSIGN")
	}
	if v.cosign == "" {
		v.cosign = "cosign"
	}

	v.report = assetsReport{SchemaVersion: assetsSchemaVersion, Tag: v.tag, SHA: v.sha, CertificateIdentity: v.identity}
	v.run()

	if err := v.write(); err != nil {
		return fmt.Errorf("verify-assets: write %s: %w", v.out, err)
	}
	var failed []string
	for _, c := range v.report.Checks {
		if !c.Passed {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("verify-assets: %d of %d checks failed: %s", len(failed), len(v.report.Checks), strings.Join(failed, ", "))
	}
	return nil
}

func (v *verifier) write() error {
	if err := os.MkdirAll(filepath.Dir(v.out), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v.report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(v.out, append(data, '\n'), 0o644)
}

func (v *verifier) archiveName(goos, goarch string) string {
	return fmt.Sprintf("otelcontext_%s_%s_%s.tar.gz", strings.TrimPrefix(v.tag, "v"), goos, goarch)
}

func assetKind(name string) string {
	switch {
	case name == "checksums.txt":
		return "checksums"
	case name == "checksums.txt.sig":
		return "signature"
	case name == "checksums.txt.pem":
		return "certificate"
	case strings.HasSuffix(name, ".tar.gz"):
		return "archive"
	case strings.HasSuffix(name, ".sbom.json"):
		return "sbom"
	}
	return "unknown"
}

func (v *verifier) run() {
	v.assetSHA = map[string]string{}
	for _, t := range verifyTargets {
		v.report.Archives = append(v.report.Archives, archiveRecord{
			Name:   v.archiveName(t[0], t[1]),
			GOOS:   t[0],
			GOARCH: t[1],
			SBOM:   v.archiveName(t[0], t[1]) + ".sbom.json",
			Files:  []string{},
		})
	}
	sort.Slice(v.report.Archives, func(i, j int) bool { return v.report.Archives[i].Name < v.report.Archives[j].Name })

	v.checkAssets()
	v.checkChecksums()
	certOK := v.checkCertificate()
	v.checkSignature(certOK)
	v.checkArchives()
	v.checkEmbeddedUI()
	v.checkVersion()
	v.checkSBOMs()
}

// checkAssets (1): the assets directory holds exactly the 11 expected names.
func (v *verifier) checkAssets() {
	expected := map[string]bool{"checksums.txt": true, "checksums.txt.sig": true, "checksums.txt.pem": true}
	for _, a := range v.report.Archives {
		expected[a.Name] = true
		expected[a.SBOM] = true
	}
	var failures []string
	entries, err := os.ReadDir(v.assets)
	if err != nil {
		failures = append(failures, fmt.Sprintf("read assets dir: %v", err))
	}
	present := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		present[name] = true
		if !e.Type().IsRegular() {
			failures = append(failures, fmt.Sprintf("%s is not a regular file", name))
			continue
		}
		sum, size, err := fileSHA256(filepath.Join(v.assets, name))
		if err != nil {
			failures = append(failures, fmt.Sprintf("hash %s: %v", name, err))
			continue
		}
		v.assetSHA[name] = sum
		v.report.Assets = append(v.report.Assets, assetRecord{Name: name, Kind: assetKind(name), SHA256: sum, SizeBytes: size})
		if !expected[name] {
			failures = append(failures, fmt.Sprintf("unexpected asset %s", name))
		}
	}
	for _, name := range sortedKeys(expected) {
		if !present[name] {
			failures = append(failures, fmt.Sprintf("missing asset %s", name))
		}
	}
	sort.Slice(v.report.Assets, func(i, j int) bool { return v.report.Assets[i].Name < v.report.Assets[j].Name })
	if v.report.Assets == nil {
		v.report.Assets = []assetRecord{}
	}
	for i := range v.report.Archives {
		a := &v.report.Archives[i]
		a.ArchiveSHA256 = v.assetSHA[a.Name]
		a.SBOMSHA256 = v.assetSHA[a.SBOM]
	}
	v.report.ChecksumsSHA256 = v.assetSHA["checksums.txt"]
	v.check("assets", failures, fmt.Sprintf("%d expected assets present, nothing else", len(expected)))
}

// checkChecksums (2): checksums.txt has exactly 8 entries, every entry names
// an existing archive or SBOM, and every digest matches the file.
func (v *verifier) checkChecksums() {
	var failures []string
	data, err := os.ReadFile(filepath.Join(v.assets, "checksums.txt"))
	if err != nil {
		v.check("checksums", []string{fmt.Sprintf("read checksums.txt: %v", err)}, "")
		return
	}
	expected := map[string]bool{}
	for _, a := range v.report.Archives {
		expected[a.Name] = true
		expected[a.SBOM] = true
	}
	seen := map[string]bool{}
	entries := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entries++
		fields := strings.Fields(line)
		if len(fields) != 2 {
			failures = append(failures, fmt.Sprintf("malformed checksum line %q", line))
			continue
		}
		sum, name := strings.ToLower(fields[0]), strings.TrimPrefix(fields[1], "*")
		if seen[name] {
			failures = append(failures, fmt.Sprintf("duplicate checksum entry for %s", name))
			continue
		}
		seen[name] = true
		if !expected[name] {
			failures = append(failures, fmt.Sprintf("checksum entry for unexpected file %s", name))
			continue
		}
		actual, ok := v.assetSHA[name]
		if !ok {
			failures = append(failures, fmt.Sprintf("checksum entry names missing asset %s", name))
			continue
		}
		if actual != sum {
			failures = append(failures, fmt.Sprintf("sha256 mismatch for %s: checksums.txt %s, file %s", name, sum, actual))
		}
	}
	if entries != 8 {
		failures = append(failures, fmt.Sprintf("checksums.txt has %d entries, want 8", entries))
	}
	for _, name := range sortedKeys(expected) {
		if !seen[name] {
			failures = append(failures, fmt.Sprintf("checksums.txt has no entry for %s", name))
		}
	}
	v.check("checksums", failures, "8 entries, every archive and SBOM digest matches")
}

// checkCertificate (3): checksums.txt.pem is a raw or base64-wrapped PEM
// holding one X.509 certificate. The unwrapped PEM is written next to the
// extracted archives for cosign.
func (v *verifier) checkCertificate() bool {
	raw, err := os.ReadFile(filepath.Join(v.assets, "checksums.txt.pem"))
	if err != nil {
		return v.check("certificate", []string{fmt.Sprintf("read checksums.txt.pem: %v", err)}, "")
	}
	pemBytes, wrapper, err := unwrapCertificate(raw)
	if err != nil {
		return v.check("certificate", []string{err.Error()}, "")
	}
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return v.check("certificate", []string{"decoded data is not a CERTIFICATE PEM block"}, "")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return v.check("certificate", []string{"trailing data after the certificate PEM block"}, "")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return v.check("certificate", []string{fmt.Sprintf("parse X.509 certificate: %v", err)}, "")
	}
	if err := os.MkdirAll(v.extract, 0o755); err != nil {
		return v.check("certificate", []string{fmt.Sprintf("create extract dir: %v", err)}, "")
	}
	v.decodedCert = filepath.Join(v.extract, decodedCertName)
	if err := os.WriteFile(v.decodedCert, pem.EncodeToMemory(block), 0o644); err != nil {
		return v.check("certificate", []string{fmt.Sprintf("write decoded certificate: %v", err)}, "")
	}
	v.report.CertificateWrapper = wrapper
	return v.check("certificate", nil, fmt.Sprintf("%s-wrapped X.509 certificate, subject %q, serial %s", wrapper, cert.Subject.String(), cert.SerialNumber))
}

// unwrapCertificate returns the PEM bytes and the wrapper kind ("pem" or
// "base64"). Anything that is neither a PEM header nor valid base64 fails.
func unwrapCertificate(raw []byte) ([]byte, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, "", errors.New("checksums.txt.pem is empty")
	}
	if bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
		return trimmed, "pem", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(string(trimmed))
	if err != nil {
		return nil, "", fmt.Errorf("checksums.txt.pem is neither PEM nor base64: %v", err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(decoded), []byte("-----BEGIN ")) {
		return nil, "", errors.New("base64 payload of checksums.txt.pem is not PEM")
	}
	return decoded, "base64", nil
}

// checkSignature (4): cosign verify-blob against the unwrapped certificate
// and the expected tag-bound identity.
func (v *verifier) checkSignature(certOK bool) {
	if !certOK {
		v.check("signature", []string{"certificate check failed, signature not verified"}, "")
		return
	}
	cosignPath, err := exec.LookPath(v.cosign)
	if err != nil {
		v.check("signature", []string{fmt.Sprintf("cosign not found: %v", err)}, "")
		return
	}
	args := []string{
		"verify-blob",
		"--certificate", v.decodedCert,
		"--signature", filepath.Join(v.assets, "checksums.txt.sig"),
		"--certificate-identity", v.identity,
		"--certificate-oidc-issuer", oidcIssuer,
		filepath.Join(v.assets, "checksums.txt"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cosignPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		v.check("signature", []string{fmt.Sprintf("cosign verify-blob failed: %v: %s", err, strings.TrimSpace(string(output)))}, "")
		return
	}
	v.report.SignatureVerified = true
	v.check("signature", nil, fmt.Sprintf("cosign verify-blob accepted checksums.txt for identity %s", v.identity))
}

// checkArchives (5): each archive extracts safely into <extract>/<goos>_<goarch>,
// carries exactly the expected file set with otelcontext as the only
// executable, and its buildinfo matches the target, SHA, and tag.
func (v *verifier) checkArchives() {
	var failures []string
	for i := range v.report.Archives {
		a := &v.report.Archives[i]
		archivePath := filepath.Join(v.assets, a.Name)
		if _, ok := v.assetSHA[a.Name]; !ok {
			failures = append(failures, fmt.Sprintf("%s: archive missing", a.Name))
			continue
		}
		dest := filepath.Join(v.extract, a.GOOS+"_"+a.GOARCH)
		files, execs, err := extractArchive(archivePath, dest)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", a.Name, err))
			continue
		}
		a.Files = files
		if strings.Join(files, "\n") != strings.Join(sortedStrings(archiveFiles), "\n") {
			failures = append(failures, fmt.Sprintf("%s: file set %v, want %v", a.Name, files, sortedStrings(archiveFiles)))
		}
		if strings.Join(execs, ",") != "otelcontext" {
			failures = append(failures, fmt.Sprintf("%s: executables %v, want [otelcontext]", a.Name, execs))
		}
		binPath := filepath.Join(dest, "otelcontext")
		info, err := os.Lstat(binPath)
		if err != nil || !info.Mode().IsRegular() {
			failures = append(failures, fmt.Sprintf("%s: otelcontext is not a regular file", a.Name))
			continue
		}
		bin, err := os.ReadFile(binPath)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: read otelcontext: %v", a.Name, err))
			continue
		}
		a.binaryPath = binPath
		a.binary = bin
		a.BinarySHA256 = hexSHA256(bin)

		bi, err := buildinfo.ReadFile(binPath)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: read buildinfo: %v", a.Name, err))
			continue
		}
		settings := map[string]string{}
		for _, s := range bi.Settings {
			settings[s.Key] = s.Value
		}
		a.VCSRevision = settings["vcs.revision"]
		if settings["GOOS"] != a.GOOS || settings["GOARCH"] != a.GOARCH {
			failures = append(failures, fmt.Sprintf("%s: buildinfo target %s/%s, want %s/%s", a.Name, settings["GOOS"], settings["GOARCH"], a.GOOS, a.GOARCH))
		}
		if a.VCSRevision != v.sha {
			failures = append(failures, fmt.Sprintf("%s: vcs.revision %q, want %s", a.Name, a.VCSRevision, v.sha))
		}
		if settings["vcs.modified"] != "false" {
			failures = append(failures, fmt.Sprintf("%s: vcs.modified %q, want \"false\"", a.Name, settings["vcs.modified"]))
		}
		// Go redacts the -ldflags setting from buildinfo when -trimpath is set
		// (go.dev/issue/52372), and the release build uses -trimpath. When the
		// setting is present it must carry the tag; when it is absent the
		// binary must at least prove it was a -trimpath build, and the version
		// stamp is proven by executing the host-native binary (check 7).
		ldflags, hasLdflags := settings["-ldflags"]
		switch {
		case hasLdflags:
			if want := "-X main.Version=" + v.tag; !strings.Contains(ldflags, want) {
				failures = append(failures, fmt.Sprintf("%s: ldflags %q do not contain %q", a.Name, ldflags, want))
			}
		case settings["-trimpath"] != "true":
			failures = append(failures, fmt.Sprintf("%s: buildinfo records neither -ldflags nor -trimpath=true", a.Name))
		}
	}
	v.check("archives", failures, "4 archives extracted with the expected file set, single executable, and matching buildinfo (target, vcs.revision, vcs.modified, version stamp or -trimpath)")
}

// extractArchive unpacks a tar.gz into dest, refusing absolute paths, parent
// references, symlinks, hardlinks, and any entry that is not a regular file
// or directory. It returns the sorted regular-file paths and the sorted
// subset of those carrying an executable bit.
func extractArchive(archivePath, dest string) (files, execs []string, err error) {
	if err := os.RemoveAll(dest); err != nil {
		return nil, nil, fmt.Errorf("clear %s: %w", dest, err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files, execs = []string{}, []string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("tar: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || path.Clean(name) != name {
			return nil, nil, fmt.Errorf("unsafe tar path %q", hdr.Name)
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == ".." || seg == "." {
				return nil, nil, fmt.Errorf("unsafe tar path %q", hdr.Name)
			}
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, nil, err
			}
			mode := fs.FileMode(hdr.Mode).Perm()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode|0o600)
			if err != nil {
				return nil, nil, fmt.Errorf("create %s: %w", name, err)
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return nil, nil, fmt.Errorf("write %s: %w", name, copyErr)
			}
			if closeErr != nil {
				return nil, nil, closeErr
			}
			files = append(files, name)
			if mode&0o111 != 0 {
				execs = append(execs, name)
			}
		default:
			return nil, nil, fmt.Errorf("tar entry %q has unsupported type %q (only regular files and directories are allowed)", hdr.Name, hdr.Typeflag)
		}
	}
	sort.Strings(files)
	sort.Strings(execs)
	return files, execs, nil
}

// checkEmbeddedUI (6): every binary contains the full bytes of the three UI
// source files from --source-root.
func (v *verifier) checkEmbeddedUI() {
	var failures []string
	ui := map[string][]byte{}
	for _, name := range uiFiles {
		data, err := os.ReadFile(filepath.Join(v.sourceRoot, "internal", "ui", "static", name))
		if err != nil {
			failures = append(failures, fmt.Sprintf("read UI source %s: %v", name, err))
			continue
		}
		if len(data) == 0 {
			failures = append(failures, fmt.Sprintf("UI source %s is empty", name))
			continue
		}
		ui[name] = data
	}
	for i := range v.report.Archives {
		a := &v.report.Archives[i]
		if a.binary == nil {
			failures = append(failures, fmt.Sprintf("%s: no extracted binary to inspect", a.Name))
			continue
		}
		embedded := len(ui) == len(uiFiles)
		for _, name := range uiFiles {
			data, ok := ui[name]
			if ok && !bytes.Contains(a.binary, data) {
				embedded = false
				failures = append(failures, fmt.Sprintf("%s: binary does not contain %s", a.Name, name))
			}
		}
		a.EmbeddedUI = embedded
	}
	v.check("embedded_ui", failures, "index.html, app.js and app.css bytes present in every binary")
}

// checkVersion (7): the host-native binary prints exactly
// "OtelContext version <tag>" for --version.
func (v *verifier) checkVersion() {
	if v.skipExec {
		v.check("version", nil, "skipped (--skip-exec)")
		return
	}
	want := "OtelContext version " + v.tag
	for i := range v.report.Archives {
		a := &v.report.Archives[i]
		if a.GOOS != runtime.GOOS || a.GOARCH != runtime.GOARCH {
			continue
		}
		if a.binaryPath == "" {
			v.check("version", []string{fmt.Sprintf("%s: no extracted binary to execute", a.Name)}, "")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), versionExecTimeout)
		defer cancel()
		output, err := exec.CommandContext(ctx, a.binaryPath, "--version").CombinedOutput()
		a.Executed = true
		a.Version = strings.TrimSpace(string(output))
		if err != nil {
			v.check("version", []string{fmt.Sprintf("%s: --version failed: %v: %s", a.Name, err, a.Version)}, "")
			return
		}
		if a.Version != want {
			v.check("version", []string{fmt.Sprintf("%s: --version printed %q, want %q", a.Name, a.Version, want)}, "")
			return
		}
		v.check("version", nil, fmt.Sprintf("%s printed %q", a.Name, want))
		return
	}
	v.check("version", []string{fmt.Sprintf("no archive targets the host %s/%s", runtime.GOOS, runtime.GOARCH)}, "")
}

// checkSBOMs (8): every archive has a matching SBOM that is valid JSON.
func (v *verifier) checkSBOMs() {
	var failures []string
	for _, a := range v.report.Archives {
		data, err := os.ReadFile(filepath.Join(v.assets, a.SBOM))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: read SBOM: %v", a.Name, err))
			continue
		}
		if !json.Valid(data) {
			failures = append(failures, fmt.Sprintf("%s: %s is not valid JSON", a.Name, a.SBOM))
		}
	}
	v.check("sboms", failures, "4 SBOMs present and valid JSON")
}

func fileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
