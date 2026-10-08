package extract

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/moby/patternmatcher"
)

func WriteTarExclude(
	writer io.Writer,
	localPath string,
	compress bool,
	excludedPaths []string,
) error {
	absolute, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("absolute: %w", err)
	}

	stat, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	gw := writer
	if compress {
		gwWriter := gzip.NewWriter(writer)
		defer func() { _ = gwWriter.Close() }()

		gw = gwWriter
	}

	tarWriter := tar.NewWriter(gw)
	defer func() { _ = tarWriter.Close() }()

	if !stat.IsDir() {
		archiver, err := NewArchiver(filepath.Dir(absolute), tarWriter, excludedPaths)
		if err != nil {
			return err
		}
		return archiver.AddToArchive(filepath.Base(absolute))
	}

	archiver, err := NewArchiver(absolute, tarWriter, excludedPaths)
	if err != nil {
		return err
	}
	return archiver.AddToArchive("")
}

func WriteTar(writer io.Writer, localPath string, compress bool) error {
	return WriteTarExclude(writer, localPath, compress, nil)
}

// Archiver is responsible for compressing specific files and folders within a target directory.
type Archiver struct {
	basePath     string
	writer       *tar.Writer
	writtenFiles map[string]bool

	// excludes holds the exclude patterns, with .dockerignore semantics. It is
	// nil when nothing is excluded.
	excludes *patternmatcher.PatternMatcher
	// reincludes holds the components of each negation (!) pattern.
	reincludes [][]string
}

// NewArchiver creates a new archiver. excludedPaths are patterns relative to
// basePath, with the same syntax as a .dockerignore file.
func NewArchiver(basePath string, writer *tar.Writer, excludedPaths []string) (*Archiver, error) {
	var excludes *patternmatcher.PatternMatcher
	if len(excludedPaths) > 0 {
		var err error
		excludes, err = patternmatcher.New(excludedPaths)
		if err != nil {
			return nil, fmt.Errorf("parse exclude patterns: %w", err)
		}
	}

	return &Archiver{
		basePath:     basePath,
		writer:       writer,
		writtenFiles: map[string]bool{},
		excludes:     excludes,
		reincludes:   reincludePatterns(excludes),
	}, nil
}

// reincludePatterns returns the components of each negation pattern.
func reincludePatterns(excludes *patternmatcher.PatternMatcher) [][]string {
	if excludes == nil || !excludes.Exclusions() {
		return nil
	}

	var reincludes [][]string
	for _, p := range excludes.Patterns() {
		if p.Exclusion() {
			reincludes = append(reincludes, strings.Split(filepath.ToSlash(p.String()), "/"))
		}
	}
	return reincludes
}

// AddToArchive adds a new path to the archive.
func (a *Archiver) AddToArchive(relativePath string) error {
	return a.addToArchive(relativePath, patternmatcher.MatchInfo{})
}

// addToArchive adds a path to the archive. parentInfo holds the exclude
// results of the parent directory, or the zero value when they are unknown.
func (a *Archiver) addToArchive(relativePath string, parentInfo patternmatcher.MatchInfo) error {
	if a.writtenFiles[relativePath] {
		return nil
	}

	stat, err := os.Lstat(path.Join(a.basePath, relativePath))
	if err != nil {
		return nil
	}

	excluded, info, err := a.isExcluded(relativePath, parentInfo)
	if err != nil {
		return err
	}

	if stat.IsDir() {
		// Skip an excluded folder without reading it, unless a negation
		// pattern could re-include something below it.
		if excluded && !a.mayReincludeBelow(relativePath) {
			return nil
		}

		return a.tarFolder(relativePath, stat, excluded, info)
	}

	if excluded {
		return nil
	}
	return a.tarFile(relativePath, stat)
}

// mayReincludeBelow reports whether a negation pattern could match a path
// inside the excluded folder dir. It may return true for a pattern that turns
// out not to match, but never false for one that does.
func (a *Archiver) mayReincludeBelow(dir string) bool {
	dirComponents := strings.Split(path.Clean(filepath.ToSlash(dir)), "/")
	for _, pattern := range a.reincludes {
		if patternMayMatchBelow(pattern, dirComponents) {
			return true
		}
	}
	return false
}

// patternMayMatchBelow reports whether pattern could match a path below the
// folder dir, both given as path components.
func patternMayMatchBelow(pattern, dir []string) bool {
	// "**" matches any number of folders, so do not try to rule it out
	if slices.Contains(pattern, "**") {
		return true
	}
	// A pattern matching dir itself, or one of its parents, was already
	// applied to dir, which is still excluded
	if len(pattern) <= len(dir) {
		return false
	}
	for i, component := range dir {
		matched, err := path.Match(pattern[i], component)
		if err != nil || !matched {
			return err != nil
		}
	}
	return true
}

// isExcluded matches relativePath against the exclude patterns. It also
// returns the match results to pass down to the path's children.
func (a *Archiver) isExcluded(
	relativePath string,
	parentInfo patternmatcher.MatchInfo,
) (bool, patternmatcher.MatchInfo, error) {
	if a.excludes == nil {
		return false, patternmatcher.MatchInfo{}, nil
	}

	// Patterns are relative to the archive root, so the root itself never matches
	relativePath = path.Clean(filepath.ToSlash(relativePath))
	if relativePath == "." || relativePath == "/" {
		return false, patternmatcher.MatchInfo{}, nil
	}

	excluded, info, err := a.excludes.MatchesUsingParentResults(relativePath, parentInfo)
	if err != nil {
		return false, info, fmt.Errorf("match %s against exclude patterns: %w", relativePath, err)
	}
	return excluded, info, nil
}

func (a *Archiver) tarFolder(
	target string,
	targetStat os.FileInfo,
	excluded bool,
	matchInfo patternmatcher.MatchInfo,
) error {
	filePath := path.Join(a.basePath, target)
	files, err := os.ReadDir(filePath)
	if err != nil {
		return nil
	}

	if len(files) == 0 && target != "" && !excluded {
		return a.tarEmptyFolder(target, targetStat)
	}

	for _, dirEntry := range files {
		f, err := dirEntry.Info()
		if err != nil {
			continue
		}

		if err = a.addToArchive(path.Join(target, f.Name()), matchInfo); err != nil {
			return fmt.Errorf("recursive tar %s: %w", f.Name(), err)
		}
	}

	return nil
}

func (a *Archiver) tarEmptyFolder(target string, targetStat os.FileInfo) error {
	hdr, _ := tar.FileInfoHeader(targetStat, path.Join(a.basePath, target))
	// #nosec G115 -- a tar header mode always fits in an os.FileMode
	hdr.Mode = fillGo18FileTypeBits(int64(chmodTarEntry(os.FileMode(hdr.Mode))), targetStat)
	hdr.Name = target
	if err := a.writer.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar write header: %w", err)
	}
	a.writtenFiles[target] = true

	return nil
}

func (a *Archiver) tarFile(target string, targetStat os.FileInfo) error {
	var err error
	filepath := path.Join(a.basePath, target)

	// don't resolve symlinks
	linkName := ""
	if targetStat.Mode()&os.ModeSymlink == os.ModeSymlink {
		linkName, err = os.Readlink(filepath)
		if err != nil {
			return nil
		}
	}

	hdr, err := tar.FileInfoHeader(targetStat, linkName)
	if err != nil {
		return fmt.Errorf("create tar file info header: %w", err)
	}
	hdr.Name = target
	hdr.Mode = fillGo18FileTypeBits(int64(chmodTarEntry(os.FileMode(hdr.Mode))), targetStat)
	hdr.ModTime = time.Unix(targetStat.ModTime().Unix(), 0)

	if err := a.writer.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar write header: %w", err)
	}

	// nothing more to do for non-regular
	if !targetStat.Mode().IsRegular() {
		a.writtenFiles[target] = true
		return nil
	}

	return a.writeRegularFileBody(target, filepath, targetStat)
}

func (a *Archiver) writeRegularFileBody(target, filePath string, targetStat os.FileInfo) error {
	// #nosec G304 -- path is derived from the archive being created, not external input
	f, err := os.Open(filePath)
	if err != nil {
		// We ignore open file and just treat it as okay
		return nil
	}
	defer func() { _ = f.Close() }()
	copied, err := io.CopyN(a.writer, f, targetStat.Size())
	if err != nil {
		return fmt.Errorf("tar copy file: %w", err)
	} else if copied != targetStat.Size() {
		return errors.New("tar: file truncated during read")
	}

	a.writtenFiles[target] = true
	return nil
}

const (
	modeISDIR  = 0o40000  // Directory
	modeISFIFO = 0o10000  // FIFO
	modeISREG  = 0o100000 // Regular file
	modeISLNK  = 0o120000 // Symbolic link
	modeISBLK  = 0o60000  // Block special file
	modeISCHR  = 0o20000  // Character special file
	modeISSOCK = 0o140000 // Socket
)

// chmodTarEntry is used to adjust the file permissions used in tar header based
// on the platform the archival is done.
func chmodTarEntry(perm os.FileMode) os.FileMode {
	if runtime.GOOS != "windows" {
		return perm
	}

	// perm &= 0755 // this 0-ed out tar flags (like link, regular file, directory marker etc.)
	permPart := perm & os.ModePerm
	noPermPart := perm &^ os.ModePerm
	// Add the x bit: make everything +x from windows
	permPart |= 0o111
	permPart &= 0o755

	return noPermPart | permPart
}

// fillGo18FileTypeBits fills type bits which have been removed on Go 1.9 archive/tar
// https://github.com/golang/go/commit/66b5a2f
func fillGo18FileTypeBits(mode int64, fi os.FileInfo) int64 {
	fm := fi.Mode()
	switch {
	case fm.IsRegular():
		mode |= modeISREG
	case fi.IsDir():
		mode |= modeISDIR
	case fm&os.ModeSymlink != 0:
		mode |= modeISLNK
	case fm&os.ModeDevice != 0:
		if fm&os.ModeCharDevice != 0 {
			mode |= modeISCHR
		} else {
			mode |= modeISBLK
		}
	case fm&os.ModeNamedPipe != 0:
		mode |= modeISFIFO
	case fm&os.ModeSocket != 0:
		mode |= modeISSOCK
	}
	return mode
}
