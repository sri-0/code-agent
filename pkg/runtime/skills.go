package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// UploadSkills tars the bundle directories at bundleDirs (each becomes a
// top-level entry named after its base dir) and POSTs the gzipped
// archive to adminURL + "/skills". No-op if either argument is empty.
//
// The cmd/runner receiver extracts into the worker's global opencode
// skills dir (/home/worker/.config/opencode/skills) where opencode
// auto-discovers SKILL.md files at the next session.
func UploadSkills(ctx context.Context, adminURL string, bundleDirs []string) error {
	if adminURL == "" || len(bundleDirs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, dir := range bundleDirs {
		if err := addBundleToTar(tw, dir); err != nil {
			return fmt.Errorf("tar %s: %w", dir, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(adminURL, "/")+"/skills", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post skills: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("skills upload: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// addBundleToTar walks srcDir and writes entries into tw under
// "<base(srcDir)>/<relative path>". Follows nothing (no symlinks).
func addBundleToTar(tw *tar.Writer, srcDir string) error {
	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", srcDir)
	}
	base := filepath.Base(srcDir)
	return filepath.Walk(srcDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !fi.Mode().IsDir() && !fi.Mode().IsRegular() {
			return nil // skip symlinks/devices
		}
		h, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(filepath.Join(base, rel))
		if fi.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}
