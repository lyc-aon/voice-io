package models

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// InstallError is a download, license or integrity problem preventing a
// working model.
type InstallError struct{ msg string }

func (e *InstallError) Error() string { return e.msg }

func installErr(format string, args ...any) error {
	return &InstallError{msg: fmt.Sprintf(format, args...)}
}

// Missing reports required files that are absent or empty at a model path.
func Missing(m Model, path string) []string {
	base := path
	if !m.Directory() {
		base = filepath.Dir(path)
	}
	var missing []string
	for _, name := range m.Required {
		info, err := os.Stat(filepath.Join(base, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			missing = append(missing, name)
		}
	}
	return missing
}

// Status describes one catalog model on disk.
type Status struct {
	Model     Model
	Path      string
	Installed bool
	Missing   []string
}

// Inspect reports install status for models under root.
func Inspect(root string, groups ...Group) []Status {
	var out []Status
	for _, m := range Select(groups...) {
		path := filepath.Join(root, m.Relative)
		missing := Missing(m, path)
		out = append(out, Status{Model: m, Path: path, Installed: len(missing) == 0, Missing: missing})
	}
	return out
}

// Verify returns installed paths keyed by model ID, or an error naming every
// missing file and the command that installs it.
func Verify(root string, groups ...Group) (map[string]string, error) {
	found := map[string]string{}
	var problems []string
	meeting := false
	for _, s := range Inspect(root, groups...) {
		if s.Model.Group == Meeting {
			meeting = true
		}
		if s.Installed {
			found[s.Model.ID] = s.Path
			continue
		}
		problems = append(problems, fmt.Sprintf("%s: missing %s in %s",
			s.Model.ID, strings.Join(s.Missing, ", "), s.Path))
	}
	if len(problems) > 0 {
		command := "voice-io models install"
		if meeting {
			command += " --meeting"
		}
		return nil, installErr("models incomplete; run `%s`:\n%s", command, strings.Join(problems, "\n"))
	}
	return found, nil
}

// Progress reports installation advancement. Bytes count downloaded data.
type Progress struct {
	ModelID    string
	Title      string
	Phase      string // "download", "extract", "installed", "present"
	Done       int64  // bytes of the current model
	Total      int64
	TotalDone  int64 // bytes across the whole request
	TotalBytes int64
}

// Downloader fetches url into dst, returning the SHA-256 of what it wrote.
// onBytes is called with byte counts as data arrives.
type Downloader func(ctx context.Context, url string, dst io.Writer, onBytes func(int64)) error

// Installer installs catalog models under Root.
type Installer struct {
	Root     string
	Download Downloader // nil = HTTPS
	Progress func(Progress)
}

// Install fetches every missing model in groups. Complete installs are left
// untouched; a failed transfer never publishes staged bytes; re-running
// safely retries an interrupted run.
func (in *Installer) Install(ctx context.Context, groups ...Group) (map[string]string, error) {
	download := in.Download
	if download == nil {
		download = httpDownload
	}
	selected := Select(groups...)
	var totalBytes int64
	for _, m := range selected {
		if len(Missing(m, filepath.Join(in.Root, m.Relative))) > 0 {
			totalBytes += m.DownloadSize()
		}
	}
	var totalDone int64
	report := func(p Progress) {
		if in.Progress != nil {
			p.TotalDone, p.TotalBytes = atomic.LoadInt64(&totalDone), totalBytes
			in.Progress(p)
		}
	}
	if err := os.MkdirAll(in.Root, 0o755); err != nil {
		return nil, installErr("cannot create %s: %v", in.Root, err)
	}
	for _, m := range selected {
		target := filepath.Join(in.Root, m.Relative)
		if len(Missing(m, target)) == 0 {
			report(Progress{ModelID: m.ID, Title: m.Title, Phase: "present", Done: 1, Total: 1})
			continue
		}
		var done int64
		onBytes := func(n int64) {
			done += n
			atomic.AddInt64(&totalDone, n)
			report(Progress{ModelID: m.ID, Title: m.Title, Phase: "download", Done: done, Total: m.DownloadSize()})
		}
		if err := in.installOne(ctx, m, target, download, onBytes, report); err != nil {
			return nil, err
		}
		report(Progress{ModelID: m.ID, Title: m.Title, Phase: "installed", Done: m.DownloadSize(), Total: m.DownloadSize()})
	}
	return Verify(in.Root, groups...)
}

func (in *Installer) installOne(ctx context.Context, m Model, target string, download Downloader,
	onBytes func(int64), report func(Progress)) error {
	temp, err := os.MkdirTemp(in.Root, ".voice-io-model-")
	if err != nil {
		return installErr("%s: cannot stage download: %v", m.ID, err)
	}
	defer os.RemoveAll(temp)
	staging := filepath.Join(temp, m.Relative)
	if m.Directory() {
		if err := os.Mkdir(staging, 0o755); err != nil {
			return installErr("%s: cannot stage download: %v", m.ID, err)
		}
	}
	for _, a := range m.Artifacts {
		if err := fetchArtifact(ctx, m, a, temp, staging, download, onBytes, report); err != nil {
			return err
		}
	}
	if missing := Missing(m, staging); len(missing) > 0 {
		return installErr("%s: download incomplete; missing/empty %s", m.ID, strings.Join(missing, ", "))
	}
	// Preserve an existing incomplete target if the replacement fails.
	if _, err := os.Lstat(target); err == nil {
		backup := filepath.Join(temp, ".previous")
		if err := os.Rename(target, backup); err != nil {
			return installErr("%s: cannot replace %s: %v", m.ID, target, err)
		}
		if err := os.Rename(staging, target); err != nil {
			_ = os.Rename(backup, target)
			return installErr("%s: cannot publish %s: %v", m.ID, target, err)
		}
		return nil
	}
	if err := os.Rename(staging, target); err != nil {
		return installErr("%s: cannot publish %s: %v", m.ID, target, err)
	}
	return nil
}

func fetchArtifact(ctx context.Context, m Model, a Artifact, temp, staging string, download Downloader,
	onBytes func(int64), report func(Progress)) error {
	var dest string
	switch {
	case a.Archive:
		dest = filepath.Join(temp, filepath.Base(a.URL))
	case m.Directory():
		dest = filepath.Join(staging, a.File)
	default:
		dest = staging
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return installErr("%s: cannot write %s: %v", m.ID, dest, err)
	}
	hash := sha256.New()
	err = download(ctx, a.URL, io.MultiWriter(f, hash), onBytes)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var httpErr *HTTPError
		switch {
		case errors.As(err, &httpErr):
			msg := "HTTP service error"
			switch httpErr.Code {
			case 401, 403:
				msg = "authorization/license acceptance required on the model page"
			case 404:
				msg = "model file not found at the pinned revision"
			}
			return installErr("%s: %s: %s (HTTP %d)", m.ID, msg, a.URL, httpErr.Code)
		case errors.Is(err, context.Canceled):
			return err
		default:
			return installErr("%s: network/disk failure fetching %s: %v", m.ID, a.URL, err)
		}
	}
	if a.SHA256 != "" {
		if got := hex.EncodeToString(hash.Sum(nil)); got != a.SHA256 {
			return installErr("SHA-256 mismatch for %s; download may be incomplete", a.URL)
		}
	}
	if a.Archive {
		report(Progress{ModelID: m.ID, Title: m.Title, Phase: "extract"})
		if err := extract(dest, m, staging); err != nil {
			return err
		}
		_ = os.Remove(dest)
	}
	return nil
}

// extract copies only the required regular files out of an untrusted
// .tar.bz2. Traversal, links and device entries for required names abort.
func extract(archive string, m Model, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return installErr("%s: cannot open archive: %v", m.ID, err)
	}
	defer f.Close()
	required := map[string]bool{}
	for _, name := range m.Required {
		required[name] = true
	}
	tr := tar.NewReader(bzip2.NewReader(f))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return installErr("%s: corrupt model archive: %v", m.ID, err)
		}
		parts := strings.Split(strings.TrimPrefix(hdr.Name, "./"), "/")
		if len(parts) != 2 || parts[0] != m.Relative || !required[parts[1]] {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return installErr("%s: unsafe archive entry %q", m.ID, hdr.Name)
		}
		out, err := os.OpenFile(filepath.Join(dest, parts[1]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return installErr("%s: cannot extract %s: %v", m.ID, parts[1], err)
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return installErr("%s: corrupt model archive: %v", m.ID, err)
		}
	}
}

// HTTPError is a non-2xx response.
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

var client = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	TLSHandshakeTimeout:   30 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
	IdleConnTimeout:       30 * time.Second,
}}

const stallTimeout = 60 * time.Second

func httpDownload(ctx context.Context, url string, dst io.Writer, onBytes func(int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "voice-io-model-installer/2")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Code: resp.StatusCode}
	}
	// A connection that stops delivering bytes fails instead of hanging.
	stall := time.AfterFunc(stallTimeout, func() {
		cancel(errors.New("download stalled for 60s"))
	})
	defer stall.Stop()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(stallTimeout)
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			if onBytes != nil {
				onBytes(int64(n))
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				return cause
			}
			return rerr
		}
	}
}
