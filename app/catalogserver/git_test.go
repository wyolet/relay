package catalogserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestRepo_RefusesArgumentsGitWouldReadAsOptions(t *testing.T) {
	repo := Repo{Dir: t.TempDir()}
	ctx := context.Background()
	for _, ref := range []string{"", "--upload-pack=x"} {
		if _, _, err := repo.Commit(ctx, ref); err == nil || err.Error() != `invalid ref "`+ref+`"` {
			t.Errorf("Commit(%q): %v", ref, err)
		}
	}
	for _, commit := range []string{"HEAD", "--output=/tmp/x", strings.Repeat("A", 40), strings.Repeat("a", 41)} {
		if _, err := repo.Archive(ctx, commit); err == nil || err.Error() != `invalid commit "`+commit+`"` {
			t.Errorf("Archive(%q): %v", commit, err)
		}
	}
	for _, url := range []string{"", "--upload-pack=x"} {
		m := &Mirror{Repo: repo, URL: url}
		if err := m.Fetch(ctx); err == nil || err.Error() != `invalid repository URL "`+url+`"` {
			t.Errorf("Fetch(%q): %v", url, err)
		}
	}
}

func TestMirror_FetchesOnlyTagsAndMain(t *testing.T) {
	src := newSourceRepo(t)
	main := src.commit(catalogTree(fixtureAPIVersion, "Rocket"), oldCommit)
	src.tag("v0.1.0", true)
	src.git(newCommit, "checkout", "--quiet", "-b", "fork")
	fork := src.commit(catalogTree(fixtureAPIVersion, "Unreviewed"), newCommit)
	src.git(time.Time{}, "update-ref", "refs/pull/1/head", fork)
	src.git(time.Time{}, "checkout", "--quiet", "main")
	src.git(time.Time{}, "tag", "not-a-commit", src.git(time.Time{}, "rev-parse", "main:README.md"))

	m := &Mirror{Repo: Repo{Dir: t.TempDir() + "/mirror"}, URL: src.dir}
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	refs := must(m.git(ctx, gitCommandTime, "for-each-ref", "--format=%(refname)"))(t)
	if got, want := string(refs), "refs/heads/main\nrefs/tags/not-a-commit\nrefs/tags/v0.1.0\n"; got != want {
		t.Fatalf("mirror refs %q, want %q", got, want)
	}
	if _, err := m.git(ctx, gitCommandTime, "cat-file", "-e", fork+"^{commit}"); err == nil {
		t.Fatal("the pull-request commit reached the mirror")
	}
	tags := must(m.Tags(ctx))(t)
	// The annotated tag peels to its commit; the tag of a blob is no release.
	if diff := cmp.Diff([]Tag{{Name: "v0.1.0", Commit: main, Committed: oldCommit}}, tags); diff != "" {
		t.Fatalf("tags (-want +got):\n%s", diff)
	}
}
