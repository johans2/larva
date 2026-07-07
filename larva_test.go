package main

import (
	"path/filepath"
	"testing"
)

// TestObjectPathNoCollision guards the invariant that distinct source files
// never map to the same object path. A regression here silently drops a
// translation unit from the link (this test was written after exactly that
// bug: foo.c and foo.cpp in one directory both compiled to foo.o).
func TestObjectPathNoCollision(t *testing.T) {
	cacheDir = "cache"

	sources := []string{
		filepath.Join("src", "foo.c"),
		filepath.Join("src", "foo.cpp"), // same basename, different extension
		filepath.Join("src", "bar.c"),
		filepath.Join("libs", "foo.c"), // same name+ext, different directory
		filepath.Join("libs", "vendor", "foo.c"),
	}

	seen := map[string]string{}
	for _, src := range sources {
		obj := objectPath(src)
		if prev, ok := seen[obj]; ok {
			t.Errorf("collision: %q and %q both map to %q", prev, src, obj)
		}
		seen[obj] = src
	}
}

// TestObjectPathStaysUnderCache checks that sources reached via a parent
// directory or an absolute path still produce objects under cacheDir rather
// than escaping it.
func TestObjectPathStaysUnderCache(t *testing.T) {
	cacheDir = "cache"

	for _, src := range []string{
		filepath.Join("..", "shared", "a.c"),
		filepath.Join("..", "..", "b.c"),
	} {
		obj := objectPath(src)
		rel, err := filepath.Rel(cacheDir, obj)
		if err != nil {
			t.Fatalf("objectPath(%q) = %q: not relative to cacheDir: %v", src, obj, err)
		}
		if filepath.IsAbs(obj) || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
			t.Errorf("objectPath(%q) = %q escapes cacheDir (rel %q)", src, obj, rel)
		}
	}
}
