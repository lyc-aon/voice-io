package models

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// server is an offline downloader over fixed URL contents.
type server struct {
	mu      sync.Mutex
	files   map[string][]byte
	fail    map[string]error
	fetched []string
}

func (s *server) download(ctx context.Context, url string, dst io.Writer, onBytes func(int64)) error {
	s.mu.Lock()
	s.fetched = append(s.fetched, url)
	data, ok := s.files[url]
	err := s.fail[url]
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if !ok {
		return &HTTPError{Code: 404}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	n, err := dst.Write(data)
	onBytes(int64(n))
	return err
}

func useCatalog(t *testing.T, models ...Model) {
	t.Helper()
	saved := Catalog
	Catalog = models
	t.Cleanup(func() { Catalog = saved })
}

func plainModel(s *server) Model {
	tokens, encoder := []byte("tokens"), []byte("encoder weights")
	s.files["https://example/final/tokens.txt"] = tokens
	s.files["https://example/final/encoder.onnx"] = encoder
	return Model{ID: "final", Group: Core, Relative: "final", Required: []string{"encoder.onnx", "tokens.txt"},
		Artifacts: []Artifact{
			{URL: "https://example/final/encoder.onnx", SHA256: sum(encoder), Size: int64(len(encoder)), File: "encoder.onnx"},
			{URL: "https://example/final/tokens.txt", SHA256: sum(tokens), Size: int64(len(tokens)), File: "tokens.txt"},
		}}
}

func singleFileModel(s *server) Model {
	weights := []byte("speaker weights")
	s.files["https://example/embed.onnx"] = weights
	return Model{ID: "embed", Group: Meeting, Relative: "embed.onnx", Required: []string{"embed.onnx"},
		Artifacts: []Artifact{{URL: "https://example/embed.onnx", SHA256: sum(weights), Size: int64(len(weights)), File: "embed.onnx"}}}
}

func leftovers(t *testing.T, root string) []string {
	t.Helper()
	staged, _ := filepath.Glob(filepath.Join(root, ".voice-io-model-*"))
	return staged
}

func TestInstallIsVerifiedIdempotentAndScopedToGroups(t *testing.T) {
	s := &server{files: map[string][]byte{}}
	useCatalog(t, plainModel(s), singleFileModel(s))
	root := t.TempDir()
	var phases []string
	in := &Installer{Root: root, Download: s.download, Progress: func(p Progress) { phases = append(phases, p.ModelID+":"+p.Phase) }}

	paths, err := in.Install(context.Background(), Core)
	if err != nil {
		t.Fatal(err)
	}
	if paths["final"] != filepath.Join(root, "final") || len(paths) != 1 || len(s.fetched) != 2 {
		t.Fatalf("paths %v fetched %v", paths, s.fetched)
	}
	if _, err := Verify(root, Meeting); err == nil || !strings.Contains(err.Error(), "voice-io models install --meeting") {
		t.Fatalf("meeting verify: %v", err)
	}
	if _, err := in.Install(context.Background(), Core, Meeting); err != nil {
		t.Fatal(err)
	}
	s.fetched = nil
	if _, err := in.Install(context.Background(), Core, Meeting); err != nil || len(s.fetched) != 0 {
		t.Fatalf("second install fetched %v (%v)", s.fetched, err)
	}
	if phases[len(phases)-1] != "embed:present" {
		t.Errorf("phases %v", phases)
	}
	if len(leftovers(t, root)) != 0 {
		t.Errorf("staging left behind")
	}
}

func TestVerifyNamesEveryMissingFile(t *testing.T) {
	s := &server{files: map[string][]byte{}}
	useCatalog(t, plainModel(s))
	_, err := Verify(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "encoder.onnx, tokens.txt") || !strings.Contains(err.Error(), "voice-io models install") {
		t.Fatalf("verify: %v", err)
	}
}

func TestFailedDownloadsNeverPublishPartialModels(t *testing.T) {
	s := &server{files: map[string][]byte{}}
	m := plainModel(s)
	delete(s.files, "https://example/final/tokens.txt")
	useCatalog(t, m)
	root := t.TempDir()
	_, err := (&Installer{Root: root, Download: s.download}).Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found at the pinned revision") {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "final")); !os.IsNotExist(err) {
		t.Fatal("partial model published")
	}

	s.files["https://example/final/tokens.txt"] = []byte("tampered")
	if _, err := (&Installer{Root: root, Download: s.download}).Install(context.Background()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tampered file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "final")); !os.IsNotExist(err) {
		t.Fatal("unverified model published")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.files["https://example/final/tokens.txt"] = []byte("tokens")
	if _, err := (&Installer{Root: root, Download: s.download}).Install(ctx); err != context.Canceled {
		t.Fatalf("cancelled install: %v", err)
	}
	if len(leftovers(t, root)) != 0 {
		t.Error("staging left behind")
	}
}

type entry struct {
	name, link string
	body       string
}

func tarBz2(t *testing.T, entries ...entry) []byte {
	t.Helper()
	bz, err := exec.LookPath("bzip2")
	if err != nil {
		t.Skip("bzip2 not installed")
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.link != "" {
			hdr = &tar.Header{Name: e.name, Linkname: e.link, Typeflag: tar.TypeSymlink, Mode: 0o777}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bz, "-c")
	cmd.Stdin = &raw
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func archiveModel(s *server, archive []byte) Model {
	s.files["https://example/kws.tar.bz2"] = archive
	return Model{ID: "kws", Group: Core, Relative: "kws", Required: []string{"tokens.txt", "en.phone"},
		Artifacts: []Artifact{{URL: "https://example/kws.tar.bz2", SHA256: sum(archive), Size: int64(len(archive)), Archive: true}}}
}

func TestArchivesInstallOnlyRequiredRegularFiles(t *testing.T) {
	archive := tarBz2(t,
		entry{name: "kws/tokens.txt", body: "tokens"},
		entry{name: "./kws/en.phone", body: "HEY HH EY1"},
		entry{name: "kws/README.md", body: "not needed"},
		entry{name: "../outside.onnx", body: "escape"},
	)
	s := &server{files: map[string][]byte{}}
	useCatalog(t, archiveModel(s, archive))
	parent := t.TempDir()
	root := filepath.Join(parent, "models")
	if _, err := (&Installer{Root: root, Download: s.download}).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "kws", "en.phone")); string(data) != "HEY HH EY1" {
		t.Fatalf("en.phone = %q", data)
	}
	for _, unwanted := range []string{filepath.Join(root, "kws", "README.md"), filepath.Join(parent, "outside.onnx"),
		filepath.Join(root, "outside.onnx"), filepath.Join(root, "kws.tar.bz2")} {
		if _, err := os.Stat(unwanted); !os.IsNotExist(err) {
			t.Errorf("%s exists", unwanted)
		}
	}

	if err := os.Remove(filepath.Join(root, "kws", "en.phone")); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(root); err == nil || !strings.Contains(err.Error(), "en.phone") {
		t.Fatalf("verify after deletion: %v", err)
	}
}

func TestArchivesRejectLinksAndBadHashes(t *testing.T) {
	linked := tarBz2(t, entry{name: "kws/tokens.txt", body: "tokens"}, entry{name: "kws/en.phone", link: "/etc/passwd"})
	s := &server{files: map[string][]byte{}}
	useCatalog(t, archiveModel(s, linked))
	root := t.TempDir()
	if _, err := (&Installer{Root: root, Download: s.download}).Install(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe archive entry") {
		t.Fatalf("symlink member: %v", err)
	}

	good := tarBz2(t, entry{name: "kws/tokens.txt", body: "tokens"}, entry{name: "kws/en.phone", body: "x"})
	m := archiveModel(s, good)
	m.Artifacts[0].SHA256 = sum([]byte("something else"))
	useCatalog(t, m)
	if _, err := (&Installer{Root: root, Download: s.download}).Install(context.Background()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("bad hash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "kws")); !os.IsNotExist(err) {
		t.Fatal("rejected archive published")
	}
}
