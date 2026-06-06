package testdata

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type Credential interface {
	Apply(req *http.Request)
}

type Client struct {
	baseURL    string
	cacheRoot  string
	httpClient *http.Client
	cred       Credential
	logger     logging.Logger
}

func New(baseURL, cacheRoot string, httpClient *http.Client, cred Credential, logger logging.Logger) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		cacheRoot:  cacheRoot,
		httpClient: httpClient,
		cred:       cred,
		logger:     logger,
	}
}

func (c *Client) Ensure(ctx context.Context, problemID, expectedVersion int64) ([]model.Case, int64, error) {
	problemRoot := filepath.Join(c.cacheRoot, "problems", strconv.FormatInt(problemID, 10))
	testdataDir := filepath.Join(problemRoot, "testdata")
	versionFile := filepath.Join(problemRoot, "data-version")
	localVersion := readVersion(versionFile)

	reqURL := fmt.Sprintf("%s/judge/problems/%d/testdata", c.baseURL, problemID)
	if localVersion > 0 {
		reqURL += "?version=" + strconv.FormatInt(localVersion, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, err
	}
	c.cred.Apply(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	remoteVersion := parseVersion(resp.Header.Get("X-Data-Version"), localVersion)
	switch resp.StatusCode {
	case http.StatusNotModified:
		c.logger.Info("testdata cache hit", logging.Int64("problemId", problemID), logging.Int64("version", remoteVersion))
	case http.StatusOK:
		c.logger.Info("testdata cache miss", logging.Int64("problemId", problemID), logging.Int64("version", remoteVersion))
		if err := c.replaceFromZip(resp.Body, problemRoot, testdataDir, versionFile, remoteVersion); err != nil {
			return nil, 0, err
		}
		c.logger.Info("testdata downloaded", logging.Int64("problemId", problemID), logging.Int64("version", remoteVersion))
	default:
		return nil, 0, fmt.Errorf("testdata download failed with status %d", resp.StatusCode)
	}

	cases, err := LoadCases(testdataDir)
	if err != nil {
		return nil, 0, err
	}
	return cases, remoteVersion, nil
}

func (c *Client) replaceFromZip(r io.Reader, problemRoot, testdataDir, versionFile string, version int64) error {
	tmpZip, err := os.CreateTemp("", "hnieoj-testdata-*.zip")
	if err != nil {
		return err
	}
	tmpZipPath := tmpZip.Name()
	defer os.Remove(tmpZipPath)
	if _, err := io.Copy(tmpZip, r); err != nil {
		tmpZip.Close()
		return err
	}
	if err := tmpZip.Close(); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp(problemRoot, "testdata-new-*")
	if err != nil {
		if mkErr := os.MkdirAll(problemRoot, 0o755); mkErr != nil {
			return mkErr
		}
		tmpDir, err = os.MkdirTemp(problemRoot, "testdata-new-*")
		if err != nil {
			return err
		}
	}
	defer os.RemoveAll(tmpDir)

	if err := unzipSafe(tmpZipPath, tmpDir); err != nil {
		return err
	}
	if _, err := LoadCases(tmpDir); err != nil {
		return err
	}

	oldDir := testdataDir + ".old"
	os.RemoveAll(oldDir)
	if _, err := os.Stat(testdataDir); err == nil {
		if err := os.Rename(testdataDir, oldDir); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpDir, testdataDir); err != nil {
		if _, statErr := os.Stat(oldDir); statErr == nil {
			_ = os.Rename(oldDir, testdataDir)
		}
		return err
	}
	if err := os.WriteFile(versionFile, []byte(strconv.FormatInt(version, 10)), 0o644); err != nil {
		return err
	}
	os.RemoveAll(oldDir)
	return nil
}

func unzipSafe(zipPath, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if err := validateZipEntry(f.Name); err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			return fmt.Errorf("zip directory entry is not allowed: %s", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, f.Name)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func validateZipEntry(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid zip entry name: %q", name)
	}
	if filepath.IsAbs(name) || filepath.Base(name) != name || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("zip entry must be a plain filename: %s", name)
	}
	if ext := filepath.Ext(name); ext != ".in" && ext != ".out" {
		return fmt.Errorf("unsupported testdata file extension: %s", name)
	}
	return nil
}

func LoadCases(dir string) ([]model.Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	inputs := make(map[string]string)
	outputs := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base := strings.TrimSuffix(name, filepath.Ext(name))
		switch filepath.Ext(name) {
		case ".in":
			inputs[base] = filepath.Join(dir, name)
		case ".out":
			outputs[base] = filepath.Join(dir, name)
		}
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		if _, ok := outputs[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no complete testdata pairs in %s", dir)
	}
	cases := make([]model.Case, 0, len(keys))
	for _, k := range keys {
		in, err := os.ReadFile(inputs[k])
		if err != nil {
			return nil, err
		}
		out, err := os.ReadFile(outputs[k])
		if err != nil {
			return nil, err
		}
		cases = append(cases, model.Case{
			ID:       k,
			Input:    string(in),
			Expected: string(out),
		})
	}
	return cases, nil
}

func readVersion(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseVersion(v string, fallback int64) int64 {
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}
