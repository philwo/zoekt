package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sourcegraph/zoekt/ignore"
)

func do(w io.Writer) error {
	gitdir, err := getGitDir()
	if err != nil {
		return err
	}

	// TODO PERF skip object caching since we don't need it for archive. See
	// cache for filesystem.NewStorage
	r, err := git.PlainOpenWithOptions(gitdir, &git.PlainOpenOptions{
		DetectDotGit: true,
	})
	if err != nil {
		return err
	}

	head, err := r.Head()
	if err != nil {
		return err
	}

	commit, err := r.CommitObject(head.Hash())
	if err != nil {
		return err
	}

	root, err := r.TreeObject(commit.TreeHash)
	if err != nil {
		return err
	}

	return archiveWrite(w, r, root, &archiveOpts{
		Ignore: getIgnoreFilter(r, root),
		SkipContent: func(hdr *tar.Header) string {
			if hdr.Size > 2<<20 {
				return "large file"
			}
			return ""
		},
	})
}

type archiveOpts struct {
	// Ignore if true will exclude path from the archive
	Ignore func(path string) bool
	// SkipContent if returning a non-empty string will include an entry for
	// path but with no content. The PAX header SOURCEGRAPH.skip will contain
	// the returned string (a reason for skipping).
	SkipContent func(hdr *tar.Header) string
}

func archiveWrite(w io.Writer, repo *git.Repository, tree *object.Tree, opts *archiveOpts) error {
	tw := tar.NewWriter(w)
	err := archiveWriteTree(tw, repo, tree, "", opts)
	if err != nil {
		return err
	}
	return tw.Close()
}

func archiveWriteTree(w *tar.Writer, repo *git.Repository, tree *object.Tree, path string, opts *archiveOpts) error {
	// TODO share
	// 32*1024 is the same size used by io.Copy
	buf := make([]byte, 32*1024)

	for _, e := range tree.Entries {
		var p string
		if e.Mode == filemode.Dir {
			p = path + e.Name + "/"
		} else {
			p = path + e.Name
		}

		if opts.Ignore(p) {
			continue
		}

		switch e.Mode {
		case filemode.Dir:
			child, err := repo.TreeObject(e.Hash)
			if err != nil {
				log.Printf("failed to fetch tree object for %s %v: %v", p, e.Hash, err)
				continue
			}

			if err := w.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     p,
				Format: tar.FormatPAX, // TODO ?
			}); err != nil {
				return err
			}

			if err := archiveWriteTree(w, repo, child, p, opts); err != nil {
				return err
			}

		case filemode.Deprecated, filemode.Executable, filemode.Regular, filemode.Symlink:
			blob, err := repo.BlobObject(e.Hash)
			if err != nil {
				log.Printf("failed to get blob object for %s %v: %v", p, e.Hash, err)
				continue
			}

			// TODO symlinks, mode, etc. Handle large Linkname
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     p,
				Size:     blob.Size,

				Format: tar.FormatPAX, // TODO ?
			}

			skip := func(reason string) error {
				hdr.PAXRecords = map[string]string{"SG.skip": reason}
				hdr.Size = 0
				return w.WriteHeader(hdr)
			}

			if reason := opts.SkipContent(hdr); reason != "" {
				if err := skip(reason); err != nil {
					return err
				}
				continue
			}

			r, err := blob.Reader()
			if err != nil {
				log.Printf("failed to read blob object for %s %v: %v", p, e.Hash, err)
				continue
			}

			// Heuristic: Assume file is binary if first 256 bytes contain a 0x00.
			blobSample := buf[:256]
			if n, err := io.ReadAtLeast(r, blobSample, 256); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				_ = r.Close()
				log.Printf("failed to read blob object for %s %v: %v", p, e.Hash, err)
				continue
			} else {
				blobSample = blobSample[:n]
			}

			// TODO instead of just binary, should we only allow utf8? utf.Valid
			// works except for the fact we may be invalid utf8 at the 256 boundary
			// since we cut it off. So will need to copypasta that.
			if bytes.IndexByte(blobSample, 0x00) >= 0 {
				_ = r.Close()
				if err := skip("binary"); err != nil {
					return err
				}
				continue
			}

			if err := w.WriteHeader(hdr); err != nil {
				_= r.Close()
				return err
			}

			// We read some bytes from r already, first write those.
			if _, err := w.Write(blobSample); err != nil {
				_ = r.Close()
				return err
			}

			// Write out the rest of r.
			if _, err := io.CopyBuffer(w, r, buf); err != nil {
				_ = r.Close()
				return err
			}

			if err := r.Close(); err != nil {
				return err
			}

		case filemode.Submodule:
			// TODO what do?
			continue

		default:
			log.Printf("WARN: unexpected filemode %+v", e)
		}
	}

	return nil
}

func getIgnoreFilter(r *git.Repository, root *object.Tree) func(string) bool {
	m, err := parseIgnoreFile(r, root)
	if err != nil {
		// likely malformed, just log and ignore
		log.Printf("WARN: failed to parse sourcegraph ignore file: %v", err)
		return func(_ string) bool { return false }
	}

	return m.Match
}

func parseIgnoreFile(r *git.Repository, root *object.Tree) (*ignore.Matcher, error) {
	entry, err := root.FindEntry(ignore.IgnoreFile)
	if isNotExist(err) {
		return &ignore.Matcher{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to find %s: %w", ignore.IgnoreFile, err)
	}

	if !entry.Mode.IsFile() {
		return &ignore.Matcher{}, nil
	}

	blob, err := r.BlobObject(entry.Hash)
	if err != nil {
		return nil, err
	}

	reader, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	m, err := ignore.ParseIgnoreFile(reader)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	// go-git does not have an interface to check for not found, and can
	// returned a myraid of errors for not found depending on were along looking
	// for a file it failed (object, tree, entry, etc). So strings are the best
	// we can do.
	return os.IsNotExist(err) || strings.Contains(err.Error(), "not found")
}

func getGitDir() (string, error) {
	dir := os.Getenv("GIT_DIR")
	if dir == "" {
		return os.Getwd()
	}
	return dir, nil
}

func main() {
	err := do(os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
}
