// Package project resolves the project identity for a session.
//
// Precedence (highest first):
//  1. .shared-memory.json marker file in the cwd or any ancestor
//  2. git remote origin URL of the cwd, parsed to owner/repo
//  3. The config's project.slug
//  4. cwd basename
//
// Goal: identity follows the project (1 and 2 travel with the repo across
// devices), not the device. The marker file beats git remote so a repo can
// override the auto-detected slug (e.g. monorepos with multiple memories).
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Source string

const (
	SourceMarker      Source = "marker-file"
	SourceGitRemote   Source = "git-remote"
	SourceConfig      Source = "config"
	SourceCwdBasename Source = "cwd-basename"
)

type Resolved struct {
	Slug   string
	Name   string
	Source Source
}

type Input struct {
	CWD        string
	ConfigSlug string
	ConfigName string
}

const markerFilename = ".shared-memory.json"

type marker struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Resolve walks from cwd upward looking for a marker file, then falls back
// to the git remote, then to the config slug, then to the cwd basename.
func Resolve(in Input) (Resolved, error) {
	cwd := in.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return Resolved{}, fmt.Errorf("getwd: %w", err)
		}
	}

	if m, ok := findMarker(cwd); ok {
		name := m.Name
		if name == "" {
			name = m.Slug
		}
		return Resolved{Slug: m.Slug, Name: name, Source: SourceMarker}, nil
	}

	if slug, ok := gitRemoteSlug(cwd); ok {
		return Resolved{Slug: slug, Name: slug, Source: SourceGitRemote}, nil
	}

	if in.ConfigSlug != "" {
		name := in.ConfigName
		if name == "" {
			name = in.ConfigSlug
		}
		return Resolved{Slug: in.ConfigSlug, Name: name, Source: SourceConfig}, nil
	}

	base := strings.ToLower(filepath.Base(cwd))
	if base == "" || base == string(filepath.Separator) {
		base = "default"
	}
	return Resolved{Slug: base, Name: base, Source: SourceCwdBasename}, nil
}

func findMarker(start string) (marker, bool) {
	dir := start
	for {
		candidate := filepath.Join(dir, markerFilename)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			data, err := os.ReadFile(candidate)
			if err == nil {
				var m marker
				if json.Unmarshal(data, &m) == nil && m.Slug != "" {
					return m, true
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return marker{}, false
		}
		dir = parent
	}
}

// matches owner/repo trailing fragment from common git URL shapes.
var gitRemoteRe = regexp.MustCompile(`[:/]([^/:]+/[^/]+?)(?:\.git)?/?$`)

func gitRemoteSlug(cwd string) (string, bool) {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", false
	}
	m := gitRemoteRe.FindStringSubmatch(url)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}
