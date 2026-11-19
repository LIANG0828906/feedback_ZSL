package gitsource

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"driftconverge/internal/model"
)

var branchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

type Source struct {
	RepoDir      string
	ManifestPath string
	GitBinary    string
}

type SnapshotFiles struct {
	Branch string
	Commit string
	Files  map[string][]byte
}

func (s Source) ReadSnapshot(ctx context.Context, branch string) (SnapshotFiles, error) {
	if !branchRE.MatchString(branch) || strings.Contains(branch, "..") {
		return SnapshotFiles{}, model.NewError(model.CodeGit, "invalid branch name")
	}
	git := s.GitBinary
	if git == "" {
		git = "git"
	}
	commit, err := s.output(ctx, git, "--git-dir", s.RepoDir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return SnapshotFiles{}, &model.BusinessError{
			Code:         model.CodeGit,
			SourceBranch: branch,
			Message:      "cannot resolve local branch snapshot: " + err.Error(),
			Err:          err,
		}
	}
	commit = strings.TrimSpace(commit)

	out, err := s.output(ctx, git, "--git-dir", s.RepoDir, "ls-tree", "-r", "-z", "--name-only", commit)
	if err != nil {
		return SnapshotFiles{}, &model.BusinessError{
			Code:         model.CodeGit,
			SourceBranch: branch,
			Message:      "cannot list snapshot files: " + err.Error(),
			Err:          err,
		}
	}
	prefix := strings.Trim(strings.ReplaceAll(s.ManifestPath, "\\", "/"), "/")
	names := strings.Split(out, "\x00")
	sort.Strings(names)
	files := make(map[string][]byte)
	for _, name := range names {
		if name == "" {
			continue
		}
		if prefix != "" && name != prefix && !strings.HasPrefix(name, prefix+"/") {
			continue
		}
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
			continue
		}
		content, showErr := s.bytes(ctx, git, "--git-dir", s.RepoDir, "show", commit+":"+name)
		if showErr != nil {
			return SnapshotFiles{}, &model.BusinessError{
				Code:         model.CodeGit,
				SourceBranch: branch,
				Message:      fmt.Sprintf("cannot read snapshot file %q: %v", name, showErr),
				Err:          showErr,
			}
		}
		files[name] = content
	}
	if len(files) == 0 {
		return SnapshotFiles{}, &model.BusinessError{
			Code:         model.CodeManifest,
			SourceBranch: branch,
			Message:      "snapshot contains no YAML manifests under " + prefix,
		}
	}
	return SnapshotFiles{Branch: branch, Commit: commit, Files: files}, nil
}

func (s Source) output(ctx context.Context, binary string, args ...string) (string, error) {
	data, err := s.bytes(ctx, binary, args...)
	return string(data), err
}

func (s Source) bytes(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return stdout.Bytes(), nil
}
