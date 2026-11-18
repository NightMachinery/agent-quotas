package paseo

import (
	"os"
	"path/filepath"
	"strings"
)

// realpath ports Python 3.14's posixpath.realpath(strict=False), which
// Path.resolve() uses: symlinks are resolved as far as the path exists, and a
// missing component is kept literally. The old tool stored these paths in
// registrations and hashed them, so the rules must match.
func realpath(filename string) string {
	const sep = "/"
	parts := strings.Split(filename, sep)
	// rest is a stack; nil marks "the symlink below has been resolved".
	type item struct {
		name   string
		marker bool
	}
	rest := make([]item, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		rest = append(rest, item{name: parts[i]})
	}
	partCount := len(parts)
	path := sep
	if !strings.HasPrefix(filename, sep) {
		wd, err := os.Getwd()
		if err == nil {
			path = wd
		}
	}
	seen := map[string]*string{}
	pop := func() item {
		it := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		return it
	}
	for partCount > 0 {
		it := pop()
		if it.marker {
			link := pop()
			p := path
			seen[link.name] = &p
			continue
		}
		partCount--
		name := it.name
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			i := strings.LastIndex(path, sep)
			path = path[:i]
			if path == "" {
				path = sep
			}
			continue
		}
		var newpath string
		if path == sep {
			newpath = path + name
		} else {
			newpath = path + sep + name
		}
		info, err := os.Lstat(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
				continue
			}
			path = newpath // a symlink loop
			continue
		}
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if strings.HasPrefix(target, sep) {
			path = sep
		}
		seen[newpath] = nil
		rest = append(rest, item{name: newpath}, item{marker: true})
		tparts := strings.Split(target, sep)
		for i := len(tparts) - 1; i >= 0; i-- {
			rest = append(rest, item{name: tparts[i]})
		}
		partCount += len(tparts)
	}
	return path
}

// expandUser is Path.expanduser for "~" and "~/..." (the forms the old tool
// met), with home as the user's home directory.
func expandUser(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return strings.TrimRight(home, "/") + p[1:]
	}
	return p
}

// pathName is pathlib's Path(p).name.
func pathName(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" || p == "." {
		return ""
	}
	return filepath.Base(p)
}

// withSuffix is pathlib's Path.with_suffix(suffix) for a file path.
func withSuffix(p, suffix string) string {
	dir, name := filepath.Split(p)
	i := strings.LastIndex(name, ".")
	if i > 0 && i < len(name)-1 {
		name = name[:i]
	}
	return dir + name + suffix
}
