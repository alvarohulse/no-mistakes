package eval

import (
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// RepoDisplayNames resolves the opaque repository fingerprints stored in case
// manifests from repositories already registered in the local pipeline DB.
func RepoDisplayNames(repos []*db.Repo) map[string]string {
	names := map[string]string{}
	for _, repo := range repos {
		if repo == nil || strings.TrimSpace(repo.UpstreamURL) == "" {
			continue
		}
		if name := RepoDisplayName(repo); name != "" {
			names[fingerprint(repo.UpstreamURL)] = name
		}
	}
	return names
}

// RepoDisplayName prefers a remote namespace/name, then the clone directory,
// then the stable repository ID.
func RepoDisplayName(repo *db.Repo) string {
	if repo == nil {
		return ""
	}
	if path := scm.RepoPath(repo.UpstreamURL); strings.Contains(path, "/") {
		return path
	}
	if path := strings.TrimSpace(repo.WorkingPath); path != "" {
		base := filepath.Base(path)
		if base != "." && base != string(filepath.Separator) && base != "" {
			return base
		}
		return path
	}
	return strings.TrimSpace(repo.ID)
}
