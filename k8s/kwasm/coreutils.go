package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// A small set of coreutils for the shell, enough for kubectl pipelines like
// "kubectl get pods -A | grep -v Running | wc -l". They are deliberately
// simple: the common flags, no locale, no options nobody types.
type util func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error

var coreutils map[string]util

func init() {
	coreutils = map[string]util{
		"base64": base64Cmd, "cat": cat, "cut": cut, "env": env, "free": free, "grep": grep,
		"head": head, "ls": ls, "mkdir": mkdir, "rm": rm, "seq": seq, "sleep": sleepCmd,
		"sort": sortCmd, "tail": tail, "tee": tee, "touch": touch, "tr": tr,
		"uniq": uniq, "vi": viCmd, "vim": viCmd, "wc": wc, "wget": wget, "xargs": xargs,
	}
}

// opts parses short flags. spec lists the flags, a trailing ":" marks a flag
// with a value: "n:v" knows -n <value> and -v. -n5, -vn5 and --long=value
// work. A bare -<number> is stored under "#" (head -5).
func opts(name string, args []string, spec string, long map[string]string) (map[string]string, []string, error) {
	o := map[string]string{}
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			args = args[1:]
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		args = args[1:]
		if strings.HasPrefix(a, "--") {
			k, v, hasV := strings.Cut(a[2:], "=")
			short, ok := long[k]
			if !ok {
				return nil, nil, fmt.Errorf("%s: unknown option --%s", name, k)
			}
			if strings.Contains(spec, short+":") && !hasV {
				if len(args) == 0 {
					return nil, nil, fmt.Errorf("%s: option --%s needs a value", name, k)
				}
				v, args = args[0], args[1:]
			}
			o[short] = v
			continue
		}
		if _, err := strconv.Atoi(a[1:]); err == nil && strings.Contains(spec, "#") {
			o["#"] = a[1:]
			continue
		}
		for i := 1; i < len(a); i++ {
			f := string(a[i])
			j := strings.Index(spec, f)
			if j < 0 || f == ":" {
				return nil, nil, fmt.Errorf("%s: unknown option -%s", name, f)
			}
			if j+1 < len(spec) && spec[j+1] == ':' {
				v := a[i+1:]
				if v == "" {
					if len(args) == 0 {
						return nil, nil, fmt.Errorf("%s: option -%s needs a value", name, f)
					}
					v, args = args[0], args[1:]
				}
				o[f] = v
				break
			}
			o[f] = ""
		}
	}
	return o, args, nil
}

func has(o map[string]string, k string) bool { _, ok := o[k]; return ok }

func abs(hc interp.HandlerContext, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(hc.Dir, p)
}

func fail(hc interp.HandlerContext, code int, format string, a ...any) error {
	fmt.Fprintf(hc.Stderr, format+"\n", a...)
	return interp.ExitStatus(uint8(code))
}

// inputs calls fn for stdin or each named file ("-" is stdin).
func inputs(hc interp.HandlerContext, name string, files []string, fn func(file string, r io.Reader) error) error {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	for _, f := range files {
		var r io.Reader = hc.Stdin
		if r == nil {
			r = strings.NewReader("")
		}
		if f == "/dev/null" {
			r = strings.NewReader("")
		} else if f != "-" {
			fh, err := os.Open(abs(hc, f))
			if err != nil {
				fmt.Fprintf(hc.Stderr, "%s: %s: %v\n", name, f, unwrap(err))
				code = 1
				continue
			}
			defer fh.Close()
			r = fh
		}
		if err := fn(f, r); err != nil {
			return err
		}
	}
	return exitCode(code)
}

func unwrap(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}

func lines(r io.Reader) []string {
	b, _ := io.ReadAll(r)
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" && len(b) == 0 {
		return nil
	}
	return strings.Split(s, "\n")
}

func count(o map[string]string, def int) (int, error) {
	v, ok := o["n"]
	if !ok {
		v, ok = o["#"]
	}
	if !ok {
		return def, nil
	}
	return strconv.Atoi(strings.TrimPrefix(v, "+"))
}

func cat(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	return inputs(hc, "cat", args, func(_ string, r io.Reader) error {
		_, err := io.Copy(hc.Stdout, r)
		return err
	})
}

func grep(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("grep", args, "ivcnEFoqwe:", map[string]string{"ignore-case": "i", "invert-match": "v", "count": "c", "regexp": "e"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	pat, ok := o["e"]
	if !ok {
		if len(args) == 0 {
			return fail(hc, 2, "usage: grep [-ivcnoqwFE] pattern [file...]")
		}
		pat, args = args[0], args[1:]
	}
	if has(o, "F") {
		pat = regexp.QuoteMeta(pat)
	} else if !has(o, "E") {
		// basic regular expressions: \| \( \) \+ are the special ones
		pat = strings.NewReplacer(`\|`, `|`, `\(`, `(`, `\)`, `)`, `\+`, `+`, `\?`, `?`, `|`, `\|`, `(`, `\(`, `)`, `\)`, `+`, `\+`, `?`, `\?`).Replace(pat)
	}
	if has(o, "w") {
		pat = `\b(?:` + pat + `)\b`
	}
	if has(o, "i") {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return fail(hc, 2, "grep: %v", err)
	}
	found := false
	err = inputs(hc, "grep", args, func(file string, r io.Reader) error {
		prefix := ""
		if len(args) > 1 {
			prefix = file + ":"
		}
		n := 0
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 16<<20)
		for i := 1; sc.Scan(); i++ {
			l := sc.Text()
			if re.MatchString(l) == has(o, "v") {
				continue
			}
			found, n = true, n+1
			switch {
			case has(o, "q"), has(o, "c"):
			case has(o, "o") && !has(o, "v"):
				for _, m := range re.FindAllString(l, -1) {
					fmt.Fprintln(hc.Stdout, prefix+m)
				}
			case has(o, "n"):
				fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, i, l)
			default:
				fmt.Fprintln(hc.Stdout, prefix+l)
			}
		}
		if has(o, "c") {
			fmt.Fprintf(hc.Stdout, "%s%d\n", prefix, n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return interp.ExitStatus(1)
	}
	return nil
}

func head(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("head", args, "n:#", map[string]string{"lines": "n"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	n, err := count(o, 10)
	if err != nil {
		return fail(hc, 2, "head: invalid number of lines")
	}
	return inputs(hc, "head", args, func(_ string, r io.Reader) error {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 16<<20)
		for i := 0; i < n && sc.Scan(); i++ {
			fmt.Fprintln(hc.Stdout, sc.Text())
		}
		// keep draining, so the writer of a pipe does not block forever
		io.Copy(io.Discard, r)
		return nil
	})
}

func tail(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("tail", args, "n:#", map[string]string{"lines": "n"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	n, err := count(o, 10)
	if err != nil {
		return fail(hc, 2, "tail: invalid number of lines")
	}
	from := strings.HasPrefix(o["n"], "+")
	return inputs(hc, "tail", args, func(_ string, r io.Reader) error {
		l := lines(r)
		switch {
		case from:
			l = l[min(len(l), max(n-1, 0)):]
		case len(l) > n:
			l = l[len(l)-n:]
		}
		for _, s := range l {
			fmt.Fprintln(hc.Stdout, s)
		}
		return nil
	})
}

func wc(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("wc", args, "lwc", map[string]string{"lines": "l", "words": "w", "bytes": "c"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(o) == 0 {
		o = map[string]string{"l": "", "w": "", "c": ""}
	}
	return inputs(hc, "wc", args, func(file string, r io.Reader) error {
		b, _ := io.ReadAll(r)
		var f []string
		if has(o, "l") {
			f = append(f, strconv.Itoa(bytes.Count(b, []byte("\n"))))
		}
		if has(o, "w") {
			f = append(f, strconv.Itoa(len(strings.Fields(string(b)))))
		}
		if has(o, "c") {
			f = append(f, strconv.Itoa(len(b)))
		}
		if file != "-" {
			f = append(f, file)
		}
		fmt.Fprintln(hc.Stdout, strings.Join(f, " "))
		return nil
	})
}

func sortCmd(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("sort", args, "rnuk:t:", map[string]string{"reverse": "r", "numeric-sort": "n", "unique": "u", "key": "k", "field-separator": "t"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	key := func(s string) string { return s }
	if k, ok := o["k"]; ok {
		n, err := strconv.Atoi(strings.SplitN(k, ",", 2)[0])
		if err != nil || n < 1 {
			return fail(hc, 2, "sort: invalid key %q", k)
		}
		key = func(s string) string {
			var f []string
			if t, ok := o["t"]; ok {
				f = strings.Split(s, t)
			} else {
				f = strings.Fields(s)
			}
			if n > len(f) {
				return ""
			}
			return strings.Join(f[n-1:], " ")
		}
	}
	var all []string
	inputs(hc, "sort", args, func(_ string, r io.Reader) error { all = append(all, lines(r)...); return nil })
	less := func(a, b string) bool { return key(a) < key(b) }
	if has(o, "n") {
		num := func(s string) float64 {
			f, _ := strconv.ParseFloat(strings.Fields(key(s) + " 0")[0], 64)
			return f
		}
		less = func(a, b string) bool { return num(a) < num(b) }
	}
	sort.SliceStable(all, func(i, j int) bool {
		if has(o, "r") {
			return less(all[j], all[i])
		}
		return less(all[i], all[j])
	})
	if has(o, "u") {
		all = slices.CompactFunc(all, func(a, b string) bool { return key(a) == key(b) })
	}
	for _, l := range all {
		fmt.Fprintln(hc.Stdout, l)
	}
	return nil
}

func uniq(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("uniq", args, "cd", map[string]string{"count": "c", "repeated": "d"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	return inputs(hc, "uniq", args, func(_ string, r io.Reader) error {
		l := lines(r)
		for i := 0; i < len(l); {
			j := i
			for j < len(l) && l[j] == l[i] {
				j++
			}
			switch {
			case has(o, "d") && j-i < 2:
			case has(o, "c"):
				fmt.Fprintf(hc.Stdout, "%7d %s\n", j-i, l[i])
			default:
				fmt.Fprintln(hc.Stdout, l[i])
			}
			i = j
		}
		return nil
	})
}

func cut(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("cut", args, "d:f:", map[string]string{"delimiter": "d", "fields": "f"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if !has(o, "f") {
		return fail(hc, 2, "usage: cut -f list [-d delim] [file...]")
	}
	d := "\t"
	if v, ok := o["d"]; ok {
		d = v
	}
	var fields [][2]int
	for _, part := range strings.Split(o["f"], ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, _ := strconv.Atoi(lo)
		b := a
		if isRange {
			b = 1 << 30
			if hi != "" {
				b, _ = strconv.Atoi(hi)
			}
			if lo == "" {
				a = 1
			}
		}
		if a < 1 || b < a {
			return fail(hc, 2, "cut: invalid field list %q", o["f"])
		}
		fields = append(fields, [2]int{a, b})
	}
	return inputs(hc, "cut", args, func(_ string, r io.Reader) error {
		for _, l := range lines(r) {
			f := strings.Split(l, d)
			if len(f) == 1 {
				fmt.Fprintln(hc.Stdout, l)
				continue
			}
			var out []string
			for i := range f {
				for _, rg := range fields {
					if i+1 >= rg[0] && i+1 <= rg[1] {
						out = append(out, f[i])
						break
					}
				}
			}
			fmt.Fprintln(hc.Stdout, strings.Join(out, d))
		}
		return nil
	})
}

func tr(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("tr", args, "d", nil)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	set := func(s string) []rune {
		var out []rune
		r := []rune(s)
		for i := 0; i < len(r); i++ {
			if i+2 < len(r) && r[i+1] == '-' {
				for c := r[i]; c <= r[i+2]; c++ {
					out = append(out, c)
				}
				i += 2
				continue
			}
			out = append(out, r[i])
		}
		return out
	}
	if (has(o, "d") && len(args) != 1) || (!has(o, "d") && len(args) != 2) {
		return fail(hc, 2, "usage: tr set1 set2 | tr -d set1")
	}
	from := set(args[0])
	var to []rune
	if !has(o, "d") {
		to = set(args[1])
	}
	b, _ := io.ReadAll(hc.Stdin)
	out := strings.Map(func(c rune) rune {
		i := slices.Index(from, c)
		switch {
		case i < 0:
			return c
		case has(o, "d") || len(to) == 0:
			return -1
		case i < len(to):
			return to[i]
		default:
			return to[len(to)-1]
		}
	}, string(b))
	io.WriteString(hc.Stdout, out)
	return nil
}

func base64Cmd(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("base64", args, "dw:", map[string]string{"decode": "d", "wrap": "w"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	return inputs(hc, "base64", args, func(_ string, r io.Reader) error {
		b, _ := io.ReadAll(r)
		if has(o, "d") {
			s := strings.Join(strings.Fields(string(b)), "")
			out, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				out, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
			}
			if err != nil {
				return fail(hc, 1, "base64: invalid input")
			}
			hc.Stdout.Write(out)
			return nil
		}
		s := base64.StdEncoding.EncodeToString(b)
		w := 76
		if v, ok := o["w"]; ok {
			w, _ = strconv.Atoi(v)
		}
		for w > 0 && len(s) > w {
			fmt.Fprintln(hc.Stdout, s[:w])
			s = s[w:]
		}
		fmt.Fprintln(hc.Stdout, s)
		return nil
	})
}

func ls(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("ls", args, "la1Ad", nil)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(args) == 0 {
		args = []string{"."}
	}
	code := 0
	for n, a := range args {
		fi, err := os.Lstat(abs(hc, a))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: %s: %v\n", a, unwrap(err))
			code = 2
			continue
		}
		var entries []fs.FileInfo
		if fi.IsDir() && !has(o, "d") {
			des, _ := os.ReadDir(abs(hc, a))
			for _, de := range des {
				if strings.HasPrefix(de.Name(), ".") && !has(o, "a") && !has(o, "A") {
					continue
				}
				if info, err := de.Info(); err == nil {
					entries = append(entries, info)
				}
			}
			if len(args) > 1 {
				if n > 0 {
					fmt.Fprintln(hc.Stdout)
				}
				fmt.Fprintf(hc.Stdout, "%s:\n", a)
			}
		} else {
			entries = []fs.FileInfo{fi}
		}
		for _, e := range entries {
			name := e.Name()
			if !fi.IsDir() || has(o, "d") {
				name = a
			}
			if e.IsDir() {
				name = "\x1b[1;34m" + name + "\x1b[0m"
			}
			if has(o, "l") {
				fmt.Fprintf(hc.Stdout, "%s 1 root root %8d %s %s\n", e.Mode(), e.Size(), e.ModTime().Format("Jan _2 15:04"), name)
			} else {
				fmt.Fprintln(hc.Stdout, name)
			}
		}
	}
	return exitCode(code)
}

func mkdir(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("mkdir", args, "p", map[string]string{"parents": "p"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	for _, a := range args {
		if has(o, "p") {
			err = os.MkdirAll(abs(hc, a), 0o755)
		} else {
			err = os.Mkdir(abs(hc, a), 0o755)
		}
		if err != nil {
			return fail(hc, 1, "mkdir: %s: %v", a, unwrap(err))
		}
	}
	return nil
}

func rm(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("rm", args, "rfR", map[string]string{"recursive": "r", "force": "f"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	code := 0
	for _, a := range args {
		p := abs(hc, a)
		if _, err := os.Lstat(p); err != nil {
			if !has(o, "f") {
				fmt.Fprintf(hc.Stderr, "rm: %s: %v\n", a, unwrap(err))
				code = 1
			}
			continue
		}
		if has(o, "r") || has(o, "R") {
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "rm: %s: %v\n", a, unwrap(err))
			code = 1
		}
	}
	return exitCode(code)
}

func touch(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	for _, a := range args {
		f, err := os.OpenFile(abs(hc, a), os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fail(hc, 1, "touch: %s: %v", a, unwrap(err))
		}
		f.Close()
	}
	return nil
}

func tee(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("tee", args, "a", map[string]string{"append": "a"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if has(o, "a") {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	ws := []io.Writer{hc.Stdout}
	for _, a := range args {
		f, err := os.OpenFile(abs(hc, a), flag, 0o644)
		if err != nil {
			return fail(hc, 1, "tee: %s: %v", a, unwrap(err))
		}
		defer f.Close()
		ws = append(ws, f)
	}
	_, err = io.Copy(io.MultiWriter(ws...), hc.Stdin)
	return err
}

func seq(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	n := make([]int, len(args))
	for i, a := range args {
		v, err := strconv.Atoi(a)
		if err != nil {
			return fail(hc, 1, "seq: invalid number %q", a)
		}
		n[i] = v
	}
	first, step, last := 1, 1, 0
	switch len(n) {
	case 1:
		last = n[0]
	case 2:
		first, last = n[0], n[1]
	case 3:
		first, step, last = n[0], n[1], n[2]
	default:
		return fail(hc, 1, "usage: seq [first [step]] last")
	}
	if step == 0 {
		return fail(hc, 1, "seq: step must not be 0")
	}
	for i := first; (step > 0 && i <= last) || (step < 0 && i >= last); i += step {
		fmt.Fprintln(hc.Stdout, i)
	}
	return nil
}

func sleepCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if len(args) != 1 {
		return fail(hc, 1, "usage: sleep seconds")
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		f, err2 := strconv.ParseFloat(args[0], 64)
		if err2 != nil {
			return fail(hc, 1, "sleep: invalid time %q", args[0])
		}
		d = time.Duration(f * float64(time.Second))
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return interp.ExitStatus(130)
	}
}

func env(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var out []string
	hc.Env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			out = append(out, name+"="+vr.Str)
		}
		return true
	})
	sort.Strings(out)
	for _, l := range out {
		fmt.Fprintln(hc.Stdout, l)
	}
	return nil
}

// xargs runs a command with the words of stdin as extra arguments, through
// the shell, so "kubectl get pods -o name | xargs kubectl delete" works.
func xargs(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	o, args, err := opts("xargs", args, "n:I:", nil)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(args) == 0 {
		args = []string{"echo"}
	}
	b, _ := io.ReadAll(hc.Stdin)
	var words []string
	if repl, ok := o["I"]; ok {
		for _, l := range lines(bytes.NewReader(b)) {
			cmd := make([]string, len(args))
			for i, a := range args {
				cmd[i] = strings.ReplaceAll(a, repl, l)
			}
			if err := exec(ctx, cmd); err != nil {
				return err
			}
		}
		return nil
	}
	words = strings.Fields(string(b))
	n := len(words)
	if v, ok := o["n"]; ok {
		n, _ = strconv.Atoi(v)
	}
	if n < 1 {
		n = max(len(words), 1)
	}
	for i := 0; i < len(words) || i == 0; i += n {
		chunk := words[i:min(i+n, len(words))]
		if err := exec(ctx, append(slices.Clone(args), chunk...)); err != nil {
			return err
		}
		if len(words) == 0 {
			break
		}
	}
	return nil
}

// wget downloads with the fetch() of the browser (net/http on js/wasm), so
// only servers that send CORS headers work, raw.githubusercontent.com does.
// localhost stays in the wasm: kubectl port-forward 8080:80 & then
// wget -qO- localhost:8080 reaches the pod.
func wget(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	// Options may follow the urls, as with GNU wget: wget url -O x.
	var flags, urls []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			urls = append(urls, args[i+1:]...)
			i = len(args)
		case a == "-O" || a == "--output-document":
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
		default:
			urls = append(urls, a)
		}
	}
	o, _, err := opts("wget", flags, "qvO:", map[string]string{"quiet": "q", "verbose": "v", "output-document": "O"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	args = urls
	if len(args) == 0 {
		return fail(hc, 2, "usage: wget [-q] [-v] [-O file] url...")
	}
	out, toFile := o["O"], has(o, "O")
	if toFile && out != "-" && len(args) > 1 {
		return fail(hc, 2, "wget: -O with several urls is not supported")
	}
	for _, raw := range args {
		if !strings.Contains(raw, "://") {
			// Without a scheme: http for localhost (kubectl port-forward), else https.
			scheme := "https://"
			if h, _, _ := strings.Cut(raw, "/"); h == "localhost" || strings.HasPrefix(h, "localhost:") || h == "127.0.0.1" || strings.HasPrefix(h, "127.0.0.1:") {
				scheme = "http://"
			}
			raw = scheme + raw
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fail(hc, 1, "wget: %s: invalid url", raw)
		}
		name := out
		if !toFile {
			if name = path.Base(u.Path); name == "/" || name == "." || name == "" {
				name = "index.html"
			}
		}
		if err := download(ctx, hc, u.String(), name, has(o, "q")); err != nil {
			return err
		}
	}
	return nil
}

// No keep-alive: a pooled connection would keep answering for a server that
// has stopped since.
var localClient = &http.Client{Transport: &http.Transport{DialContext: dialLocal, DisableKeepAlives: true}}

// dialLocal dials in the in-process network of the wasm. It has no resolver,
// so localhost becomes 127.0.0.1.
func dialLocal(ctx context.Context, network, addr string) (net.Conn, error) {
	if host, port, err := net.SplitHostPort(addr); err == nil && host == "localhost" {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func download(ctx context.Context, hc interp.HandlerContext, u, name string, quiet bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail(hc, 1, "wget: %v", err)
	}
	client := http.DefaultClient
	if h := req.URL.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
		// localhost is the in-process network of the wasm, where kubectl
		// port-forward listens. A Transport with a dialer skips fetch().
		client = localClient
	}
	resp, err := client.Do(req)
	if err != nil && client == localClient {
		return fail(hc, 4, "wget: %s: %v", u, err)
	}
	if err != nil {
		// fetch() reports a CORS block as a plain network error.
		return fail(hc, 4, "wget: %s: %v (the server must allow CORS requests)", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fail(hc, 8, "wget: %s: %s", u, resp.Status)
	}
	w := hc.Stdout
	if name != "-" {
		f, err := os.Create(abs(hc, name))
		if err != nil {
			return fail(hc, 1, "wget: %s: %v", name, unwrap(err))
		}
		defer f.Close()
		w = f
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return fail(hc, 4, "wget: %s: %v", u, err)
	}
	if !quiet && name != "-" {
		fmt.Fprintf(hc.Stderr, "%s saved [%d bytes]\n", name, n)
	}
	return nil
}
