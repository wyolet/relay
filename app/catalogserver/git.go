package catalogserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxGitOutput   = 64 << 20
	gitFetchTime   = 2 * time.Minute
	gitCommandTime = time.Minute
)

// Repo runs the git binary against one repository directory, bare or not.
type Repo struct {
	Dir string
}

// Tag is a tag peeled to its commit.
type Tag struct {
	Name      string
	Commit    string
	Committed time.Time
}

// gitEnv drops the caller's GIT_* variables and every system or user config,
// so archive bytes and fetch behaviour don't depend on the host.
func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
}

func runGit(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv()
	stdout := &cappedBuffer{max: maxGitOutput}
	stderr := &cappedBuffer{max: 4 << 10, truncate: true}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w: %s", gitSubcommand(args), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C", "-c":
			i++
		default:
			return args[i]
		}
	}
	return ""
}

// git trusts r.Dir explicitly: a checkout mounted into a container is often
// owned by another uid, which git otherwise refuses to read.
func (r Repo) git(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	dir, err := filepath.Abs(r.Dir)
	if err != nil {
		return nil, err
	}
	return runGit(ctx, timeout, append([]string{"-c", "safe.directory=" + dir, "-C", dir}, args...)...)
}

// Commit resolves ref to its commit and the commit's committer time, read
// from %cI so the original offset is kept.
func (r Repo) Commit(ctx context.Context, ref string) (string, time.Time, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return "", time.Time{}, fmt.Errorf("invalid ref %q", ref)
	}
	out, err := r.git(ctx, gitCommandTime, "show", "-s", "--format=%H%x09%cI", ref+"^{commit}")
	if err != nil {
		return "", time.Time{}, err
	}
	commit, date, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if !ok {
		return "", time.Time{}, fmt.Errorf("git show %s: unexpected output %q", ref, out)
	}
	at, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("git show %s: %w", ref, err)
	}
	return commit, at, nil
}

// Archive is the data tarball of commit: the data/ tree without data/drafts,
// gzipped by git's built-in writer, which a Go gzip can't reproduce.
func (r Repo) Archive(ctx context.Context, commit string) ([]byte, error) {
	if !isHexOID(commit) {
		return nil, fmt.Errorf("invalid commit %q", commit)
	}
	return r.git(ctx, gitCommandTime, "archive", "--format=tar.gz", commit, "--", "data", ":(exclude)data/drafts")
}

// Mirror is a bare clone of URL that follows its tags and main branch.
type Mirror struct {
	Repo
	URL string
}

// Init creates the bare repository unless one already exists.
func (m *Mirror) Init(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	_, err := runGit(ctx, gitCommandTime, "init", "--bare", "--quiet", m.Dir)
	return err
}

// Fetch updates tags and main only. Never --mirror: on GitHub it also pulls
// refs/pull/*, unreviewed fork content.
func (m *Mirror) Fetch(ctx context.Context) error {
	if m.URL == "" || strings.HasPrefix(m.URL, "-") {
		return fmt.Errorf("invalid repository URL %q", m.URL)
	}
	_, err := m.git(ctx, gitFetchTime, "fetch", "--quiet", "--no-tags", m.URL,
		"+refs/tags/*:refs/tags/*", "+refs/heads/main:refs/heads/main")
	return err
}

// Tags lists the mirror's tags that peel to a commit.
func (m *Mirror) Tags(ctx context.Context) ([]Tag, error) {
	const format = "%(refname:strip=2)%09%(objecttype)%09%(objectname)%09%(committerdate:iso-strict)" +
		"%09%(*objecttype)%09%(*objectname)%09%(*committerdate:iso-strict)"
	out, err := m.git(ctx, gitCommandTime, "for-each-ref", "--format="+format, "refs/tags")
	if err != nil {
		return nil, err
	}
	var tags []Tag
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) != 7 {
			continue
		}
		commit, date := f[2], f[3]
		switch {
		case f[1] == "commit":
		case f[1] == "tag" && f[4] == "commit":
			commit, date = f[5], f[6]
		default:
			continue
		}
		at, err := time.Parse(time.RFC3339, date)
		if err != nil {
			return nil, fmt.Errorf("tag %s: commit date %q: %w", f[0], date, err)
		}
		tags = append(tags, Tag{Name: f[0], Commit: commit, Committed: at})
	}
	return tags, nil
}

func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

var errOutputTooLarge = errors.New("output too large")

// cappedBuffer fails the write past max, or with truncate drops the rest.
type cappedBuffer struct {
	bytes.Buffer
	max      int
	truncate bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		if !b.truncate {
			return 0, errOutputTooLarge
		}
		b.Buffer.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
