package main

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// sh runs script with the coreutils in a temporary directory and returns the
// combined output and the exit status. The tests run under node
// (go_js_wasm_exec), which lacks os.Pipe, so xargs is not covered here.
func sh(t *testing.T, script string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	var self interp.ExecHandlerFunc
	handler := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		self = func(ctx context.Context, args []string) error {
			if u := coreutils[args[0]]; u != nil {
				return u(ctx, interp.HandlerCtx(ctx), args[1:], self)
			}
			return next(ctx, args)
		}
		return self
	}
	r, err := interp.New(interp.StdIO(strings.NewReader(""), &out, &out), interp.Dir(t.TempDir()), interp.ExecHandlers(handler))
	if err != nil {
		t.Fatal(err)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	err = r.Run(context.Background(), f)
	code := 0
	if st, ok := interp.IsExitStatus(err); ok {
		code = int(st)
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out.String(), code
}

func TestCoreutils(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{"seq 3", "1\n2\n3\n"},
		{"seq 3 | wc -l", "3\n"},
		{"seq 10 | head -3", "1\n2\n3\n"},
		{"seq 10 | head -n 2", "1\n2\n"},
		{"seq 10 | tail -2", "9\n10\n"},
		{"printf 'b\\na\\nb\\n' | sort | uniq", "a\nb\n"},
		{"printf 'b\\na\\nb\\n' | sort | uniq -c", "      1 a\n      2 b\n"},
		{"printf 'Running\\nPending\\nRunning\\n' | grep -c Running", "2\n"},
		{"printf 'Running\\nPending\\n' | grep -v Running", "Pending\n"},
		{"echo a,b,c | cut -d, -f2", "b\n"},
		{"echo hello | tr a-z A-Z", "HELLO\n"},
		{"echo -n kwasm | base64", "a3dhc20=\n"},
		{"echo a3dhc20= | base64 -d", "kwasm"},
		{"echo hi | tee f >/dev/null; cat f", "hi\n"},
		{"mkdir -p a/b && touch a/b/c && ls a/b", "c\n"},
		{"touch x && rm x && ls", ""},
	} {
		t.Run(tc.script, func(t *testing.T) {
			got, code := sh(t, tc.script)
			if code != 0 || got != tc.want {
				t.Errorf("got %q (exit %d), want %q", got, code, tc.want)
			}
		})
	}
}

func TestCoreutilsErrors(t *testing.T) {
	for _, script := range []string{
		"cat missing",
		"head --nope",
		"wget",
		"wget ftp://example.com",
		"wget -O a -O",
	} {
		if out, code := sh(t, script); code == 0 {
			t.Errorf("%s: exit 0, output %q", script, out)
		}
	}
}

func TestOpts(t *testing.T) {
	long := map[string]string{"lines": "n", "verbose": "v"}
	for _, tc := range []struct {
		args []string
		spec string
		want map[string]string
		rest []string
	}{
		{[]string{"-n5", "f"}, "n:v", map[string]string{"n": "5"}, []string{"f"}},
		{[]string{"-n", "5", "f"}, "n:v", map[string]string{"n": "5"}, []string{"f"}},
		{[]string{"-vn5"}, "n:v", map[string]string{"v": "", "n": "5"}, nil},
		{[]string{"--lines=7", "-"}, "n:v", map[string]string{"n": "7"}, []string{"-"}},
		{[]string{"--lines", "7"}, "n:v", map[string]string{"n": "7"}, nil},
		{[]string{"-5", "f"}, "n:#", map[string]string{"#": "5"}, []string{"f"}},
		{[]string{"--", "-v"}, "n:v", map[string]string{}, []string{"-v"}},
	} {
		o, rest, err := opts("t", tc.args, tc.spec, long)
		if err != nil || !maps.Equal(o, tc.want) || !slices.Equal(rest, tc.rest) {
			t.Errorf("opts(%q, %q) = %v, %q, %v; want %v, %q", tc.args, tc.spec, o, rest, err, tc.want, tc.rest)
		}
	}
	for _, args := range [][]string{{"-x"}, {"--nope"}, {"-n"}, {"--lines"}} {
		if _, _, err := opts("t", args, "n:v", long); err == nil {
			t.Errorf("opts(%q): no error", args)
		}
	}
}

func TestSplitWgetArgs(t *testing.T) {
	for _, tc := range []struct{ args, flags, urls []string }{
		{[]string{"https://jkroepke.de", "-v"}, []string{"-v"}, []string{"https://jkroepke.de"}},
		{[]string{"-qO-", "localhost:8080"}, []string{"-qO-"}, []string{"localhost:8080"}},
		{[]string{"localhost:8080", "-qO-"}, []string{"-qO-"}, []string{"localhost:8080"}},
		{[]string{"u", "-O", "-"}, []string{"-O", "-"}, []string{"u"}},
		{[]string{"-O", "x.md", "u"}, []string{"-O", "x.md"}, []string{"u"}},
		{[]string{"--", "-u"}, nil, []string{"-u"}},
	} {
		flags, urls := splitWgetArgs(tc.args)
		if !slices.Equal(flags, tc.flags) || !slices.Equal(urls, tc.urls) {
			t.Errorf("splitWgetArgs(%q) = %q, %q; want %q, %q", tc.args, flags, urls, tc.flags, tc.urls)
		}
	}
}

func TestWgetURL(t *testing.T) {
	for raw, want := range map[string]string{
		"localhost:8080":           "http://localhost:8080",
		"localhost/x":              "http://localhost/x",
		"127.0.0.1:80":             "http://127.0.0.1:80",
		"http://localhost:8080":    "http://localhost:8080",
		"jkroepke.de":              "https://jkroepke.de",
		"localhost.example.com:80": "https://localhost.example.com:80",
		"raw.githubusercontent.com/kubernetes/website/main/README.md": "https://raw.githubusercontent.com/kubernetes/website/main/README.md",
	} {
		u, err := wgetURL(raw)
		if err != nil || u.String() != want {
			t.Errorf("wgetURL(%q) = %v, %v; want %s", raw, u, err, want)
		}
	}
	for _, raw := range []string{"ftp://example.com", "https://"} {
		if u, err := wgetURL(raw); err == nil {
			t.Errorf("wgetURL(%q) = %v, want an error", raw, u)
		}
	}
}
