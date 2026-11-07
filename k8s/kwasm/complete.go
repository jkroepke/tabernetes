package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// Tab completion for the terminal: command names, file names, and for
// kubectl whatever its cobra "__complete" command offers (resource types,
// names from the cluster, flags).

// cobra's ShellCompDirective bits.
const (
	compError       = 1
	compNoSpace     = 2
	compNoFileComp  = 4
	compFilterExt   = 8
	compFilterDirs  = 16
	completeTimeout = 5 * time.Second
)

type completion struct {
	word    string   // the word under the cursor, replaced by an item
	items   []string // the candidates, each a full replacement of word
	noSpace bool     // no space after a single candidate
}

// complete completes the end of line. builtins are the commands of the
// terminal (join, proxy, ...).
func (s *shell) complete(line string, builtins []string) completion {
	// Only the last command of a pipeline or list counts.
	seg := line[strings.LastIndexAny(line, "|;&($`")+1:]
	words := strings.Fields(seg)
	if len(words) == 0 || strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, "\t") {
		words = append(words, "")
	}
	word := words[len(words)-1]
	c := completion{word: word}
	switch {
	case len(words) == 1 && !strings.Contains(word, "/"):
		c.items = commandNames(word, builtins)
	case words[0] == "kubectl" || words[0] == "k":
		return s.completeKubectl(words[1:])
	default:
		c.items, c.noSpace = s.completeFile(word, nil, false)
	}
	return c
}

func commandNames(prefix string, builtins []string) []string {
	names := append([]string{"kubectl", "k"}, builtins...)
	for name := range coreutils {
		names = append(names, name)
	}
	for _, name := range shellBuiltins {
		if interp.IsBuiltin(name) {
			names = append(names, name)
		}
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) && !strings.HasPrefix(n, "kwasm-") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// completeKubectl asks kubectl itself, like the bash completion of kubectl
// does. args ends with the word under the cursor.
func (s *shell) completeKubectl(args []string) completion {
	word := args[len(args)-1]
	c := completion{word: word}
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	defer cancel()
	var out bytes.Buffer
	runKubectl(ctx, append([]string{"__complete"}, args...), nil, &out, io.Discard)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	directive := 0
	if last := lines[len(lines)-1]; strings.HasPrefix(last, ":") {
		directive, _ = strconv.Atoi(last[1:])
		lines = lines[:len(lines)-1]
	}
	if directive&compError != 0 {
		return c
	}
	var items []string
	for _, l := range lines {
		item, _, _ := strings.Cut(l, "\t") // "name\tdescription"
		if item != "" {
			items = append(items, item)
		}
	}
	switch {
	case directive&compFilterExt != 0:
		// The items are file extensions (kubectl apply -f).
		c.items, c.noSpace = s.completeFile(word, items, false)
	case directive&compFilterDirs != 0:
		c.items, c.noSpace = s.completeFile(word, nil, true)
	case len(items) == 0 && directive&compNoFileComp == 0:
		c.items, c.noSpace = s.completeFile(word, nil, false)
	default:
		for _, item := range items {
			if strings.HasPrefix(item, word) {
				c.items = append(c.items, item)
			}
		}
		c.noSpace = directive&compNoSpace != 0
	}
	return c
}

// completeFile completes word as a path, relative to the working directory of
// the shell. exts limits files to these extensions, dirsOnly to directories.
func (s *shell) completeFile(word string, exts []string, dirsOnly bool) ([]string, bool) {
	dirPart, base := "", word
	if i := strings.LastIndex(word, "/"); i >= 0 {
		dirPart, base = word[:i+1], word[i+1:]
	}
	dir := dirPart
	switch {
	case dir == "":
		dir = s.dir()
	case strings.HasPrefix(dir, "~/"):
		dir = path.Join("/root", dir[2:])
	case !path.IsAbs(dir):
		dir = path.Join(s.dir(), dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var items []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) || strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		isDir := e.IsDir()
		if !isDir {
			if fi, err := os.Stat(path.Join(dir, name)); err == nil && fi.IsDir() { // a symlink to a directory
				isDir = true
			}
		}
		switch {
		case isDir:
			items = append(items, dirPart+name+"/")
		case dirsOnly:
		case len(exts) > 0 && !slices.ContainsFunc(exts, func(ext string) bool { return strings.HasSuffix(name, "."+ext) }):
		default:
			items = append(items, dirPart+name)
		}
	}
	slices.Sort(items)
	// A directory gets no space, so the next tab goes on inside it.
	return items, len(items) == 1 && strings.HasSuffix(items[0], "/")
}
